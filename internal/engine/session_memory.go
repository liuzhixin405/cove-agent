package engine

import (
	"context"
	"strings"

	"github.com/liuzhixin405/cove-agent/internal/memory"
	"github.com/liuzhixin405/cove-agent/internal/textutil"
)

// sessionMemoryNoteMaxBytes caps the note that carries this session's new
// memories to the next turn.
const sessionMemoryNoteMaxBytes = 2048

// sessionMemoryLineRunes caps one memory's line in that note.
const sessionMemoryLineRunes = 200

// memorySnapshot maps each memory file to its content, so what an
// extraction added can be found afterwards (diffNewMemories). Nil without a
// memory store.
func (e *Engine) memorySnapshot() map[string]string {
	if e.memStore == nil {
		return nil
	}
	snap := map[string]string{}
	for _, m := range e.memStore.All() {
		snap[m.Path] = m.Content
	}
	return snap
}

// diffNewMemories lists what after adds to before, one "name: first line"
// per new or grown memory file (for a grown one, the first line of what was
// appended). A grown file whose new line it already held is skipped.
func diffNewMemories(before map[string]string, after []memory.Entry) []string {
	var out []string
	for _, m := range after {
		old, seen := before[m.Path]
		if seen && old == m.Content {
			continue
		}
		added := m.Content
		if seen && strings.HasPrefix(m.Content, old) {
			added = m.Content[len(old):]
		}
		line := firstNonBlankLine(added)
		if line == "" || (seen && strings.Contains(old, line)) {
			// Nothing new: extraction re-appends a fact it saw again.
			continue
		}
		out = append(out, m.Name+": "+textutil.ClipRunes(line, sessionMemoryLineRunes))
	}
	return out
}

func firstNonBlankLine(s string) string {
	for _, l := range strings.Split(s, "\n") {
		if t := strings.TrimSpace(l); t != "" {
			return t
		}
	}
	return ""
}

// recordNewMemories notes what this turn's extraction added to the store
// (before is the snapshot taken before it ran), for the next turn's note.
func (e *Engine) recordNewMemories(before map[string]string) {
	if e.memStore == nil || before == nil {
		return
	}
	e.addNewMemories(diffNewMemories(before, e.memStore.All()))
}

// addNewMemories queues memory lines for the next turn's note.
func (e *Engine) addNewMemories(lines []string) {
	if len(lines) == 0 {
		return
	}
	e.bgMu.Lock()
	e.newMemories = append(e.newMemories, lines...)
	e.bgMu.Unlock()
}

// clearNewMemories drops the queued lines (after compaction the rebuilt
// system prompt carries every memory anyway) and forgets which relevant
// memories were shown, since the summary may have dropped them.
func (e *Engine) clearNewMemories() {
	e.bgMu.Lock()
	e.newMemories = nil
	e.shownMemories = nil
	e.bgMu.Unlock()
}

// takeNewMemoriesNote returns the note for the memories learned since the
// last one, and empties the queue. The system prompt is snapshotted (only
// compaction rebuilds it), so without this a memory learned in this session
// reached the model only after the next compaction or in the next session.
// The note is at most sessionMemoryNoteMaxBytes; the newest lines win.
func (e *Engine) takeNewMemoriesNote() string {
	e.bgMu.Lock()
	lines := e.newMemories
	e.newMemories = nil
	e.bgMu.Unlock()
	if len(lines) == 0 {
		return ""
	}
	const head = "<session_memories>\nMemories saved earlier in this session (not yet in the system prompt):\n"
	const tail = "</session_memories>"
	budget := sessionMemoryNoteMaxBytes - len(head) - len(tail)
	var kept []string
	for i := len(lines) - 1; i >= 0; i-- {
		l := "- " + lines[i] + "\n"
		if len(l) > budget {
			break
		}
		budget -= len(l)
		kept = append(kept, l)
	}
	if len(kept) == 0 {
		return ""
	}
	var sb strings.Builder
	sb.WriteString(head)
	for i := len(kept) - 1; i >= 0; i-- {
		sb.WriteString(kept[i])
	}
	sb.WriteString(tail)
	return sb.String()
}

// Per-turn memory note caps: the relevant memories of one turn, and the
// whole note (new memories included).
const (
	relevantMemoryNoteMaxBytes = 4096
	turnMemoryNoteMaxBytes     = 6144
)

// turnMemoryNote is the memory part of the note that follows the user's
// message: the memories learned since the last turn and the saved memories
// relevant to query that the system prompt does not carry in full, in one
// <turn_memories> block of at most turnMemoryNoteMaxBytes.
func (e *Engine) turnMemoryNote(query string) string {
	return e.turnMemoryNoteContext(context.Background(), query)
}

func (e *Engine) turnMemoryNoteContext(ctx context.Context, query string) string {
	const open, closing = "<turn_memories>\n", "</turn_memories>"
	learned := e.takeNewMemoriesNote()
	if learned != "" {
		learned += "\n"
	}
	// Both caps scale with the model's window (window_budget.go).
	budget := turnMemoryBudget(e.config.Model) - len(open) - len(closing) - len(learned)
	if rel := relevantMemoryBudget(e.config.Model); budget > rel {
		budget = rel
	}
	relevant := e.relevantMemoriesNote(ctx, query, budget)
	if learned == "" && relevant == "" {
		return ""
	}
	return open + learned + relevant + closing
}

// relevantMemoriesNote returns, within budget bytes and cut at entry
// boundaries, the full text of saved memories ranked for query (the store's
// BM25 search) that were not shown yet this session. It is empty while the
// store is small enough that the system prompt carries every memory in full
// (memory.InlineBudgetBytes, the rule BuildPrompt and RelevantMemoriesFor
// use); past that the system prompt only lists memory names. Entries are
// taken one by one rather than from RelevantMemoriesFor's rendered block so
// each can be deduplicated and the block cut between entries.
func (e *Engine) relevantMemoriesNote(ctx context.Context, query string, budget int) string {
	if ctx.Err() != nil || e.memStore == nil || strings.TrimSpace(query) == "" {
		return ""
	}
	total := 0
	for _, m := range e.memStore.All() {
		if !m.Project {
			total += len(m.Content)
		}
	}
	if total <= memory.InlineBudgetBytes {
		return ""
	}
	const head = "<relevant_memories>\nSaved memories relevant to this request (the system prompt lists only their names):\n"
	const tail = "</relevant_memories>\n"
	used := len(head) + len(tail)
	var sb strings.Builder
	matches := e.memStore.SearchContext(ctx, query, memory.RelevantTopK*2)
	if ctx.Err() != nil {
		return ""
	}
	e.bgMu.Lock()
	defer e.bgMu.Unlock()
	for _, m := range matches {
		if m.Entry.Project || e.shownMemories[m.Entry.Path] {
			continue
		}
		block := "<memory>\n<name>" + m.Entry.Name + "</name>\n<content>\n" + m.Entry.Content + "\n</content>\n</memory>\n"
		if used+len(block) > budget {
			continue
		}
		used += len(block)
		sb.WriteString(block)
		if e.shownMemories == nil {
			e.shownMemories = map[string]bool{}
		}
		e.shownMemories[m.Entry.Path] = true
	}
	if sb.Len() == 0 {
		return ""
	}
	return head + sb.String() + tail
}
