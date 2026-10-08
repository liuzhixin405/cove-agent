package memory

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/liuzhixin405/cove-agent/internal/fsatomic"
	"github.com/liuzhixin405/cove-agent/internal/safety"
	"github.com/liuzhixin405/cove-agent/internal/textutil"
)

const (
	// MaxIndexLines is the maximum number of lines for any single memory file.
	MaxIndexLines = 200
	// MaxEntryBytes is the maximum size in bytes per memory entry.
	MaxEntryBytes = 25 * 1024 // 25KB
	// MaxTotalBytes is the maximum total size across all memory files.
	MaxTotalBytes = 300 * 1024 // 300KB
)

// Entry sources, shown by /memory.
const (
	SourceInstructions = "instructions" // CLAUDE.md / AGENTS.md / .cove.md
	SourceProject      = "project"      // ~/.cove/projects/<hash>/memory
	SourceGlobal       = "global"       // ~/.cove/memory
)

type Store struct {
	dirs []string
	// projectDirs is how many leading entries of dirs are per-project memory
	// directories (their entries are SourceProject; the rest SourceGlobal).
	projectDirs int
	// cwd overrides the working directory used to find instruction files
	// (tests); empty means os.Getwd.
	cwd string
	// instrTruncated is set by All when the instruction files were clipped.
	instrTruncated bool

	// Cache to avoid repeated disk reads on every system prompt build
	mu          sync.Mutex
	cachedAll   []Entry
	cacheTime   time.Time
	cacheTTL    time.Duration
	promptCache string
	promptDirty bool
	searchIndex *BM25

	// Optional semantic search (opt-in; nil by default = pure BM25, the
	// original and still fully-supported behavior). See
	// EnableRemoteEmbeddings and docs/中等模型平替优化建议.md §2.2 — this
	// deliberately does NOT require any locally-installed model.
	embedProvider     EmbeddingProvider
	embedCache        map[string][]float32 // content-hash -> embedding vector
	embedBackoffUntil time.Time
}

// embedBackoffDuration bounds how often a failing/unreachable embeddings
// endpoint is retried: one slow failure costs one call, not every
// subsequent prompt build until the user notices and fixes their config.
const embedBackoffDuration = 5 * time.Minute

// EnableRemoteEmbeddings opts the store into blending BM25 keyword search
// with real semantic similarity from the given EmbeddingProvider (typically
// a RemoteAPIEmbeddingProvider — see embed.go). Call this only when the
// user has explicitly configured an embeddings endpoint; leaving it unset
// keeps the original pure-BM25 behavior with zero extra network calls or
// cost. Passing nil disables it again.
func (s *Store) EnableRemoteEmbeddings(provider EmbeddingProvider) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.embedProvider = provider
	if provider != nil && s.embedCache == nil {
		s.embedCache = make(map[string][]float32)
	}
}

func NewStore() *Store {
	home, _ := os.UserHomeDir()
	return &Store{
		dirs: []string{
			filepath.Join(home, ".cove", "memory"),
		},
		cacheTTL:    30 * time.Second,
		promptDirty: true,
	}
}

// NewStoreForDirs is a Store over the given directories instead of
// ~/.cove/memory (the first one is where Save and RecordExtraction write).
func NewStoreForDirs(dirs ...string) *Store {
	return &Store{dirs: dirs, cacheTTL: 30 * time.Second, promptDirty: true}
}

// NewStoreForProject is a Store over a per-project memory directory and the
// global one. Entries in the project directory win over global entries of the
// same name, and Save writes into the project directory. The old global
// directory is read as before and never migrated.
func NewStoreForProject(projectDir, globalDir string) *Store {
	return &Store{dirs: []string{projectDir, globalDir}, projectDirs: 1, cacheTTL: 30 * time.Second, promptDirty: true}
}

// PrimaryDir is the directory Save (and extraction) writes into.
func (s *Store) PrimaryDir() string {
	if len(s.dirs) == 0 {
		return ""
	}
	return s.dirs[0]
}

func (s *Store) AddDir(dir string) {
	s.dirs = append(s.dirs, dir)
}

// contentHash keys the embedding cache by content, so editing a memory
// file naturally invalidates its old cached vector without any explicit
// cache-invalidation bookkeeping.
func contentHash(content string) string {
	sum := sha256.Sum256([]byte(content))
	return hex.EncodeToString(sum[:16])
}

// vectorScoreWeight is how much a perfect (cosine similarity 1.0) semantic
// match can add on top of the existing BM25+recency score. It is additive
// rather than a full re-normalized blend deliberately: BM25 scores are
// unbounded and query-dependent, so mixing them with a 0-1 cosine score via
// fixed weights would require normalizing BM25 first (another source of
// bugs); adding a bounded bonus preserves BM25's existing ranking behavior
// exactly when semantic search is unavailable (bonus is simply 0) and only
// nudges results when it is.
const vectorScoreWeight = 0.5

// Embedding request limits. Instruction files alone can reach 32KB, and a
// single input over the provider's per-input token limit (8191 for the
// OpenAI family) fails the WHOLE request. Every entry used to be sent
// unclipped in one call, so one large file made every search fail and
// re-arm the 5-minute backoff forever — semantic search silently never
// worked again.
const (
	// embedTokenBudget is the estimated token cap per input, well under the
	// common 8k limit so estimation error cannot push it over.
	embedTokenBudget = 6000
	// maxEmbedInputBytes is a hard byte cap on each input on top of the
	// token estimate.
	maxEmbedInputBytes = 24 * 1024
	// embedBatchSize is the maximum number of inputs per Embed call.
	embedBatchSize = 32
)

// clipForEmbedding shortens text to fit embedTokenBudget and
// maxEmbedInputBytes. Tokens are estimated conservatively: ~4 ASCII bytes
// per token, and 1.5 tokens per non-ASCII rune (CJK text commonly costs one
// to two tokens per character).
func clipForEmbedding(text string) string {
	var cost float64
	for i, r := range text {
		if r < 0x80 {
			cost += 0.25
		} else {
			cost += 1.5
		}
		if cost > embedTokenBudget || i+utf8.RuneLen(r) > maxEmbedInputBytes {
			return text[:i]
		}
	}
	return text
}

// isInputSizeError reports whether an embeddings error is about an input
// being too long (a problem with this call's data, not with the endpoint),
// so it must not arm the backoff that disables semantic search for everyone.
func isInputSizeError(err error) bool {
	msg := strings.ToLower(err.Error())
	// "tokens per minute" rate limits mention tokens too, but they are an
	// endpoint condition that the backoff exists for.
	if strings.Contains(msg, "429") || strings.Contains(msg, "rate limit") {
		return false
	}
	for _, k := range []string{"too long", "too large", "maximum context", "context length", "max_tokens", "token limit", "tokens", "input length", "413"} {
		if strings.Contains(msg, k) {
			return true
		}
	}
	return false
}

// vectorScores returns cosine similarity between query and each entry in
// entries (by slice index), using the configured EmbeddingProvider. Callers
// pass only the BM25 candidates. It returns nil — not an error — whenever
// semantic search isn't usable for this call (disabled, backing off after a
// recent failure, or the API call itself failed just now), so callers can
// unconditionally add vectorScoreWeight*score without a separate
// enabled/disabled branch.
func (s *Store) vectorScores(ctx context.Context, query string, entries []Entry) map[int]float64 {
	s.mu.Lock()
	provider := s.embedProvider
	backoff := s.embedBackoffUntil
	s.mu.Unlock()

	if provider == nil || len(entries) == 0 {
		return nil
	}
	if time.Now().Before(backoff) {
		return nil
	}

	type pendingEntry struct {
		text string
		hash string
	}
	hashes := make([]string, len(entries))
	var toEmbed []pendingEntry
	queued := map[string]bool{}

	s.mu.Lock()
	for i, e := range entries {
		text := clipForEmbedding(e.Content)
		h := contentHash(text)
		hashes[i] = h
		if _, ok := s.embedCache[h]; !ok && !queued[h] {
			queued[h] = true
			toEmbed = append(toEmbed, pendingEntry{text: text, hash: h})
		}
	}
	s.mu.Unlock()

	fail := func(err error) {
		if ctx.Err() != nil {
			return
		}
		if err != nil && isInputSizeError(err) {
			return
		}
		s.mu.Lock()
		s.embedBackoffUntil = time.Now().Add(embedBackoffDuration)
		s.mu.Unlock()
	}

	// The query rides in the first batch; the rest go in batches of at most
	// embedBatchSize inputs.
	var queryVec []float32
	fresh := map[string][]float32{}
	pending := toEmbed
	first := true
	for first || len(pending) > 0 {
		inputs := make([]string, 0, embedBatchSize)
		if first {
			inputs = append(inputs, clipForEmbedding(query))
		}
		n := embedBatchSize - len(inputs)
		if n > len(pending) {
			n = len(pending)
		}
		batch := pending[:n]
		pending = pending[n:]
		for _, p := range batch {
			inputs = append(inputs, p.text)
		}

		vecs, err := provider.Embed(ctx, inputs)
		off := 0
		if first {
			if err != nil || len(vecs) == 0 || vecs[0] == nil {
				fail(err)
				return nil
			}
			queryVec = vecs[0]
			off = 1
		} else if err != nil {
			// A later batch failing still leaves the query and everything
			// embedded so far usable; score with that.
			fail(err)
			break
		}
		first = false
		for i, p := range batch {
			if off+i < len(vecs) && vecs[off+i] != nil {
				fresh[p.hash] = vecs[off+i]
			}
		}
	}

	s.mu.Lock()
	for h, v := range fresh {
		s.embedCache[h] = v
	}
	scores := make(map[int]float64, len(entries))
	for i, h := range hashes {
		if v, ok := s.embedCache[h]; ok {
			scores[i] = cosineSimilarity(queryVec, v)
		}
	}
	// Bound unbounded growth (renamed/deleted memory files leave stale entries
	// behind) by resetting rather than partial-evicting.
	//
	// This now runs AFTER scoring. Resetting first threw away the vectors that
	// had just been fetched for this very query, so the one search that
	// happened to cross the threshold silently degraded to pure BM25 — semantic
	// re-ranking quietly disappearing for no reason the user could observe.
	// Everything still in flight has already been scored by this point, and the
	// next call re-embeds what it needs.
	if len(s.embedCache) > 2000 {
		s.embedCache = make(map[string][]float32)
	}
	s.mu.Unlock()

	return scores
}

func (s *Store) All() []Entry {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.allLocked()
}

func (s *Store) allLocked() []Entry {
	// Return cached if fresh
	if s.cachedAll != nil && time.Since(s.cacheTime) < s.cacheTTL {
		return s.cachedAll
	}

	var entries []Entry
	seen := map[string]bool{}
	seenName := map[string]bool{}
	for di, dir := range s.dirs {
		source := SourceGlobal
		if di < s.projectDirs {
			source = SourceProject
		}
		files, _ := os.ReadDir(dir)
		for _, f := range files {
			if f.IsDir() {
				continue
			}
			// An atomic write that was interrupted leaves its in-progress file
			// behind in this very directory. Loading it would turn a fragment
			// of a half-written entry into a permanent memory that is injected
			// into every system prompt.
			if fsatomic.IsTempName(f.Name()) {
				continue
			}
			// Hidden files are bookkeeping, not memories: the dream
			// consolidation lock (.consolidate-lock) lives here and holds a
			// PID, which used to be injected into every prompt as a memory.
			if strings.HasPrefix(f.Name(), ".") {
				continue
			}
			path := filepath.Join(dir, f.Name())
			if seen[pathKey(path)] || seenName[f.Name()] {
				// Same file, or a same-named entry from a higher-priority
				// (project) directory already loaded.
				continue
			}
			seen[pathKey(path)] = true
			data, err := os.ReadFile(path)
			if err != nil {
				continue
			}
			seenName[f.Name()] = true
			var mtime time.Time
			if info, err := f.Info(); err == nil {
				mtime = info.ModTime()
			}
			entries = append(entries, Entry{
				Name:    f.Name(),
				Path:    path,
				Content: string(data),
				Source:  source,
				Updated: mtime,
			})
		}
	}

	cwd := s.cwd
	if cwd == "" {
		cwd, _ = os.Getwd()
	}
	s.instrTruncated = false
	if cwd != "" {
		s.instrTruncated = loadInstructionFiles(cwd, &entries, seen)
	}

	s.cachedAll = entries
	s.cacheTime = time.Now()
	s.promptDirty = true
	s.searchIndex = nil
	return entries
}

func (s *Store) searchSnapshot() ([]Entry, *BM25) {
	s.mu.Lock()
	defer s.mu.Unlock()
	entries := s.allLocked()
	if s.searchIndex == nil {
		index := NewBM25(1.2, 0.75)
		now := time.Now()
		for id, entry := range entries {
			updated := entry.Updated
			if updated.IsZero() {
				updated = now
			}
			index.Index(id, entry.Content, updated)
		}
		s.searchIndex = index
	}
	return entries, s.searchIndex
}

// BuildPrompt renders the memory block without a query: instruction files
// and memories in full while they fit InlineBudgetBytes, otherwise an index.
// The result is cached until the store changes.
func (s *Store) BuildPrompt() string {
	entries := s.All()
	if len(entries) == 0 {
		return ""
	}

	s.mu.Lock()
	if !s.promptDirty && s.promptCache != "" {
		cached := s.promptCache
		s.mu.Unlock()
		return cached
	}
	s.mu.Unlock()

	result := s.render(entries, "")
	s.mu.Lock()
	s.promptCache = result
	s.promptDirty = false
	s.mu.Unlock()
	return result
}

// InstructionsPrompt renders only the project instruction files (CLAUDE.md,
// AGENTS.md, .cove.md, ...) in a <project_instructions> block; empty when
// there are none. Sub-agents get it: they follow the project's rules too.
func (s *Store) InstructionsPrompt() string {
	var sb strings.Builder
	for _, e := range s.All() {
		if e.Project {
			writeMemory(&sb, e)
		}
	}
	if sb.Len() == 0 {
		return ""
	}
	return "<project_instructions>\n" + sb.String() + "</project_instructions>\n"
}

// PromptFor is BuildPrompt ranked for query: when the saved memories exceed
// InlineBudgetBytes, the BM25 top RelevantTopK matches for query are included
// in full (within InlineBudgetBytes) and the rest are listed in an index.
// Under the budget it equals BuildPrompt.
func (s *Store) PromptFor(query string) string {
	entries := s.All()
	if len(entries) == 0 {
		return ""
	}
	if strings.TrimSpace(query) == "" || autoBytes(entries) <= InlineBudgetBytes {
		return s.BuildPrompt()
	}
	return s.render(entries, query)
}

// RelevantMemoriesFor returns only the memories ranked for query that the
// query-less prompt (BuildPrompt) does not already carry in full, wrapped in
// <relevant_memories>. Empty when every memory is inlined anyway or nothing
// matches. Meant for a per-turn injection next to the user message.
func (s *Store) RelevantMemoriesFor(query string) string {
	entries := s.All()
	if strings.TrimSpace(query) == "" || autoBytes(entries) <= InlineBudgetBytes {
		return ""
	}
	top := s.rankedFullText(query)
	if len(top) == 0 {
		return ""
	}
	var sb strings.Builder
	sb.WriteString("<relevant_memories>\n")
	for _, e := range top {
		writeMemory(&sb, e)
	}
	sb.WriteString("</relevant_memories>\n")
	return sb.String()
}

// RelevantTopK is how many ranked memories PromptFor includes in full.
const RelevantTopK = 8

func autoBytes(entries []Entry) int {
	n := 0
	for _, e := range entries {
		if !e.Project {
			n += len(e.Content)
		}
	}
	return n
}

// rankedFullText returns up to RelevantTopK non-instruction memories ranked
// for query, skipping any that would push their combined size past
// InlineBudgetBytes.
func (s *Store) rankedFullText(query string) []Entry {
	var out []Entry
	used := 0
	// Ask for extra: instruction files take part in Search and are skipped.
	for _, m := range s.Search(query, RelevantTopK*2) {
		if m.Entry.Project {
			continue
		}
		if used+len(m.Entry.Content) > InlineBudgetBytes {
			continue
		}
		used += len(m.Entry.Content)
		out = append(out, m.Entry)
		if len(out) == RelevantTopK {
			break
		}
	}
	return out
}

func (s *Store) render(entries []Entry, query string) string {
	var project, auto []Entry
	for _, e := range entries {
		if e.Project {
			project = append(project, e)
			continue
		}
		auto = append(auto, e)
	}

	var sb strings.Builder
	sb.WriteString("\n\n<user_memories>\n")
	// Project instruction files (CLAUDE.md, AGENTS.md, ...) are always
	// included in full (within MaxInstructionBytes).
	for _, e := range project {
		writeMemory(&sb, e)
	}
	if autoBytes(entries) <= InlineBudgetBytes {
		for _, e := range auto {
			writeMemory(&sb, e)
		}
	} else {
		full := map[string]bool{}
		if query != "" {
			for _, e := range s.rankedFullText(query) {
				writeMemory(&sb, e)
				full[e.Path] = true
			}
		}
		// Past the budget, pasting every memory would crowd out the rest of
		// the context on every request. List them instead; the model reads
		// the ones relevant to the task with its read tool.
		sb.WriteString("<memory_index>\n")
		sb.WriteString("Saved memories (too many to include in full). Read the file of any entry relevant to the current task before relying on it:\n")
		for _, e := range auto {
			if full[e.Path] {
				continue
			}
			fmt.Fprintf(&sb, "- %s (%s): %s\n", e.Name, e.Path, firstLine(e.Content))
		}
		sb.WriteString("</memory_index>\n")
	}
	sb.WriteString("</user_memories>\n")
	return sb.String()
}

// InlineBudgetBytes is the total size of saved memories that BuildPrompt still
// includes in full; beyond it the prompt carries an index instead (PromptFor
// additionally inlines the top matches for the current query).
const InlineBudgetBytes = 24 * 1024

func writeMemory(sb *strings.Builder, e Entry) {
	sb.WriteString("<memory>\n")
	sb.WriteString("<name>" + e.Name + "</name>\n")
	sb.WriteString("<content>\n")
	sb.WriteString(e.Content)
	sb.WriteString("\n</content>\n")
	sb.WriteString("</memory>\n")
}

func firstLine(content string) string {
	line := strings.TrimSpace(content)
	if i := strings.IndexByte(line, '\n'); i >= 0 {
		line = strings.TrimSpace(line[:i])
	}
	return textutil.ClipRunes(line, 120)
}

// ScreenContent rejects memory content that carries injected instructions or
// secrets. Memories are injected into every future system prompt, so text
// planted in a web page or file must not be able to persist that way. Every
// writer of memory files (Save, extraction, consolidation) calls it.
func ScreenContent(content string) error {
	if f := safety.NewContentChecker().Scan(content, "memory").BlockingFinding(); f != nil {
		return fmt.Errorf("%s", f.Message)
	}
	return nil
}

// EntryMatch is one ranked memory entry returned by Search.
type EntryMatch struct {
	Entry Entry
	Score float64
}

// Search runs BM25 keyword retrieval over all memory entries and returns the
// top matches ranked by relevance (blended with recency). No embeddings are
// used. Results are deduplicated by entry name.
func (s *Store) Search(query string, topK int) []EntryMatch {
	return s.SearchContext(context.Background(), query, topK)
}

// SearchContext retrieves ranked memories and cancels semantic requests with ctx.
func (s *Store) SearchContext(ctx context.Context, query string, topK int) []EntryMatch {
	if ctx.Err() != nil {
		return nil
	}
	query = strings.TrimSpace(query)
	if query == "" {
		return nil
	}
	if topK <= 0 {
		topK = 5
	}
	entries, bm25 := s.searchSnapshot()
	if len(entries) == 0 {
		return nil
	}

	now := time.Now()

	scored := bm25.Search(query, topK*2)
	// Optional semantic re-ranking bonus (nil map when disabled/unavailable
	// — see vectorScores' doc comment), computed once over the BM25
	// candidate set rather than the full corpus, since embedding every
	// memory entry on every search would be wasteful. It used to be passed
	// the full corpus despite this comment, embedding every memory and
	// instruction file in one request.
	cands := make([]Entry, 0, len(scored))
	for _, r := range scored {
		if r.ID >= 0 && r.ID < len(entries) {
			cands = append(cands, entries[r.ID])
		}
	}
	var vecScores map[int]float64
	if ctx.Err() != nil {
		return nil
	}
	if candScores := s.vectorScores(ctx, query, cands); candScores != nil {
		vecScores = make(map[int]float64, len(candScores))
		ci := 0
		for _, r := range scored {
			if r.ID >= 0 && r.ID < len(entries) {
				if v, ok := candScores[ci]; ok {
					vecScores[r.ID] = v
				}
				ci++
			}
		}
	}
	if ctx.Err() != nil {
		return nil
	}

	seen := make(map[string]bool)
	var results []EntryMatch
	for _, r := range scored {
		if r.ID < 0 || r.ID >= len(entries) {
			continue
		}
		e := entries[r.ID]
		if seen[e.Name] {
			continue
		}
		seen[e.Name] = true
		score := r.CombinedScore(now, 48)
		if vecScores != nil {
			if v, ok := vecScores[r.ID]; ok {
				score += vectorScoreWeight * v
			}
		}
		results = append(results, EntryMatch{Entry: e, Score: score})
	}
	sort.Slice(results, func(i, j int) bool {
		return results[i].Score > results[j].Score
	})
	if len(results) > topK {
		results = results[:topK]
	}
	return results
}

// Stats summarizes the memory store contents for observability.
type Stats struct {
	FileCount     int
	ProjectCount  int
	TotalBytes    int
	TotalLines    int
	MaxEntryBytes int
	MaxTotalBytes int
	// LastExtractedAt and LastExtractedCount describe the last automatic
	// memory extraction (RecordExtraction): when it finished and how many
	// memories it saved. Zero time means none is on record.
	LastExtractedAt    time.Time
	LastExtractedCount int
}

// Stats returns aggregate statistics over all memory entries.
func (s *Store) Stats() Stats {
	entries := s.All()
	st := Stats{
		MaxEntryBytes: MaxEntryBytes,
		MaxTotalBytes: MaxTotalBytes,
	}
	for _, e := range entries {
		st.FileCount++
		if e.Project {
			st.ProjectCount++
		}
		st.TotalBytes += len(e.Content)
		st.TotalLines += strings.Count(e.Content, "\n") + 1
	}
	rec := s.lastExtraction()
	st.LastExtractedAt, st.LastExtractedCount = rec.At, rec.Count
	return st
}

type Entry struct {
	Name    string
	Path    string
	Content string
	// Project marks a project instruction file (CLAUDE.md, AGENTS.md, ...).
	Project bool
	// Source is SourceInstructions, SourceProject or SourceGlobal.
	Source string
	// Updated is the file's modification time; Search feeds it to the
	// recency term of the ranking.
	Updated time.Time
}

// validName reports whether name is a plain file name inside the memory
// directory. Save and Delete join it onto the directory, so "../x" used to
// write or delete a file outside it.
func validName(name string) error {
	if name == "" || name == "." || name == ".." ||
		strings.ContainsAny(name, `/\`) || filepath.Base(name) != name || filepath.VolumeName(name) != "" {
		return fmt.Errorf("invalid memory name %q: use a plain file name such as notes.md", name)
	}
	// Dot files in the directory are bookkeeping (the dream consolidation
	// lock, the memory write lock, the extraction record), which All skips;
	// they were accepted here, so Save(".consolidate-lock") replaced the lock
	// of a running consolidation and Delete could remove it.
	if strings.HasPrefix(name, ".") {
		return fmt.Errorf("invalid memory name %q: memory names never start with a dot (dot files are the memory directory's bookkeeping)", name)
	}
	return nil
}

func (s *Store) Save(name, content string) error {
	return s.save(name, content, nil, "")
}

func (s *Store) save(name, content string, source *ProvenanceSource, appendTo string) error {
	if err := validName(name); err != nil {
		return err
	}
	if err := ScreenContent(content); err != nil {
		return fmt.Errorf("memory entry %q refused: %w", name, err)
	}
	// Validate entry size
	if len(content) > MaxEntryBytes {
		return fmt.Errorf("memory entry %q exceeds max size (%d > %d bytes)", name, len(content), MaxEntryBytes)
	}
	// Validate line count
	lines := strings.Count(content, "\n") + 1
	if lines > MaxIndexLines {
		return fmt.Errorf("memory entry %q exceeds max lines (%d > %d)", name, lines, MaxIndexLines)
	}

	// Always the primary directory: with a per-project store that is the
	// project directory, which wins over the global one on load.
	dir := s.PrimaryDir()
	if dir == "" {
		return fmt.Errorf("memory store has no directory")
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}

	// Check total size would not exceed limit
	totalSize := 0
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if e.IsDir() || fsatomic.IsTempName(e.Name()) || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		info, err := e.Info()
		if err == nil {
			if e.Name() == name {
				continue // replacing this file, don't count old size
			}
			totalSize += int(info.Size())
		}
	}
	if totalSize+len(content) > MaxTotalBytes {
		return fmt.Errorf("total memory size would exceed limit (%d + %d > %d bytes)", totalSize, len(content), MaxTotalBytes)
	}

	// Atomic replace so a crash cannot leave a half-written memory entry.
	if err := s.prepareProvenance(name, content, source, appendTo); err != nil {
		return err
	}
	err := fsatomic.WriteFile(filepath.Join(dir, name), []byte(content), 0644)
	if err == nil {
		s.invalidateCache()
	}
	return err
}

// TruncateEntry truncates content to fit within limits.
func TruncateEntry(content string) string {
	// Truncate by lines
	lines := strings.Split(content, "\n")
	if len(lines) > MaxIndexLines {
		lines = lines[:MaxIndexLines]
		content = strings.Join(lines, "\n") + "\n... [truncated to 200 lines]"
	}
	// Truncate by bytes
	// Clip on a rune boundary — memory entries are mostly Chinese here, so a
	// raw byte slice cuts a rune in half and yields invalid UTF-8.
	content = textutil.ClipBytes(content, MaxEntryBytes-50, "\n... [truncated to 25KB]")
	return content
}

func (s *Store) Delete(name string) error {
	if err := validName(name); err != nil {
		return err
	}
	for _, d := range s.dirs {
		path := filepath.Join(d, name)
		if _, err := os.Stat(path); err == nil {
			err := os.Remove(path)
			if err == nil {
				s.invalidateCache()
				if cleanupErr := os.RemoveAll(filepath.Join(d, ".provenance", provenanceHash(pathKey(name)))); cleanupErr != nil {
					return fmt.Errorf("memory removed but provenance cleanup failed: %w", cleanupErr)
				}
			}
			return err
		}
	}
	// Nothing was removed. Returning nil here made /memory remove report a
	// typo'd name as deleted.
	return fmt.Errorf("memory %q not found", name)
}

func (s *Store) invalidateCache() {
	s.mu.Lock()
	s.cachedAll = nil
	s.searchIndex = nil
	s.cacheTime = time.Time{}
	s.promptDirty = true
	s.promptCache = ""
	s.mu.Unlock()
}
