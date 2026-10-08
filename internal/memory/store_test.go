package memory

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// newTestStore creates a Store rooted at a temp directory, bypassing
// NewStore()'s dependency on the real user home directory.
func newTestStore(t *testing.T, files map[string]string) *Store {
	t.Helper()
	dir := t.TempDir()
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0644); err != nil {
			t.Fatalf("failed to write fixture %s: %v", name, err)
		}
	}
	return &Store{dirs: []string{dir}}
}

func TestMemoryProvenanceBoundToContentAndNotInjectedIntoPrompt(t *testing.T) {
	store := newTestStore(t, map[string]string{"legacy.md": "legacy memory"})
	legacy, err := store.Provenance("legacy.md")
	if err != nil || legacy != nil {
		t.Fatalf("legacy source invented: %+v, %v", legacy, err)
	}
	source := ProvenanceSource{Kind: "extraction", SessionIDs: []string{"session-one"}, Evidence: "specific evidence"}
	if err := store.SaveWithSource("new.md", "first fact", source); err != nil {
		t.Fatal(err)
	}
	if _, err := store.AppendWithSource("new.md", "second fact", ProvenanceSource{Kind: "manual", SessionIDs: []string{"session-two"}}); err != nil {
		t.Fatal(err)
	}
	record, err := store.Provenance("new.md")
	if err != nil || record == nil || len(record.Sources) != 2 || record.Sources[0].SessionIDs[0] != "session-one" || record.Sources[1].SessionIDs[0] != "session-two" || record.ContentHash != provenanceHash("first fact\nsecond fact") {
		t.Fatalf("sources = %+v, %v", record, err)
	}
	if strings.Contains(store.BuildPrompt(), "specific evidence") || len(store.All()) != 2 {
		t.Fatal("source metadata was injected as memory")
	}
	if err := os.WriteFile(filepath.Join(store.PrimaryDir(), "new.md"), []byte("externally corrected"), 0600); err != nil {
		t.Fatal(err)
	}
	if stale, err := store.Provenance("new.md"); err != nil || stale != nil {
		t.Fatalf("external edit inherited obsolete sources: %+v, %v", stale, err)
	}
}

func TestMemoryProvenanceFailureDoesNotChangeBody(t *testing.T) {
	store := newTestStore(t, map[string]string{"facts.md": "original"})
	if err := os.WriteFile(filepath.Join(store.PrimaryDir(), ".provenance"), []byte("blocks directory creation"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveWithSource("facts.md", "replacement", ProvenanceSource{Kind: "manual"}); err == nil {
		t.Fatal("source persistence failure ignored")
	}
	data, err := os.ReadFile(filepath.Join(store.PrimaryDir(), "facts.md"))
	if err != nil || string(data) != "original" {
		t.Fatalf("body changed without durable source: %q, %v", data, err)
	}
}

func TestMemoryDeleteRemovesProvenanceRevisions(t *testing.T) {
	store := newTestStore(t, nil)
	if err := store.SaveWithSource("facts.md", "fact", ProvenanceSource{Kind: "manual", Evidence: "private source"}); err != nil {
		t.Fatal(err)
	}
	path := filepath.Dir(provenancePath(store.PrimaryDir(), "facts.md", "fact"))
	if err := store.Delete("facts.md"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("deleted memory's source revisions remain: %v", err)
	}
}

type fakeEmbedProvider struct {
	calls  int
	vecFor func(text string) []float32
	err    error
}

func (f *fakeEmbedProvider) Dim() int { return 3 }

func (f *fakeEmbedProvider) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	out := make([][]float32, len(texts))
	for i, txt := range texts {
		out[i] = f.vecFor(txt)
	}
	return out, nil
}

func TestStore_Search_PureBM25WhenEmbeddingsDisabled(t *testing.T) {
	s := newTestStore(t, map[string]string{
		"a.md": "The deployment pipeline uses docker and kubernetes for releases.",
		"b.md": "Unrelated notes about lunch preferences and scheduling.",
	})

	results := s.Search("docker kubernetes deployment", 5)
	if len(results) == 0 {
		t.Fatal("expected at least one BM25 match")
	}
	if results[0].Entry.Name != "a.md" {
		t.Fatalf("expected a.md to rank first for a matching query, got %s", results[0].Entry.Name)
	}
}

func TestStore_VectorScores_NilWhenDisabled(t *testing.T) {
	s := newTestStore(t, map[string]string{"a.md": "hello world"})
	entries := s.All()
	if got := s.vectorScores(context.Background(), "hello", entries); got != nil {
		t.Fatalf("expected nil vector scores when embeddings are not enabled, got %v", got)
	}
}

func TestStore_VectorScores_BlendsWhenEnabled(t *testing.T) {
	s := newTestStore(t, map[string]string{
		"a.md": "alpha content",
		"b.md": "beta content",
	})
	entries := s.All()

	// Query vector matches "b.md" exactly (cosine 1.0), "a.md" not at all (0.0).
	provider := &fakeEmbedProvider{
		vecFor: func(text string) []float32 {
			if text == "beta content" {
				return []float32{1, 0, 0}
			}
			if text == "alpha content" {
				return []float32{0, 1, 0}
			}
			return []float32{1, 0, 0} // query
		},
	}
	s.EnableRemoteEmbeddings(provider)

	scores := s.vectorScores(context.Background(), "query", entries)
	if scores == nil {
		t.Fatal("expected non-nil vector scores when embeddings are enabled")
	}

	var aIdx, bIdx = -1, -1
	for i, e := range entries {
		switch e.Name {
		case "a.md":
			aIdx = i
		case "b.md":
			bIdx = i
		}
	}
	if aIdx < 0 || bIdx < 0 {
		t.Fatal("fixture entries not found")
	}
	if scores[bIdx] <= scores[aIdx] {
		t.Fatalf("expected b.md's vector score (%v) to exceed a.md's (%v)", scores[bIdx], scores[aIdx])
	}

	// Second call should reuse the cache for entries (only the query needs
	// re-embedding), not re-embed everything from scratch.
	callsAfterFirst := provider.calls
	_ = s.vectorScores(context.Background(), "query again", entries)
	if provider.calls != callsAfterFirst+1 {
		t.Fatalf("expected exactly one more Embed call (query only, entries cached), calls went %d -> %d", callsAfterFirst, provider.calls)
	}
}

func TestStore_VectorScores_BacksOffAfterFailure(t *testing.T) {
	s := newTestStore(t, map[string]string{"a.md": "hello world"})
	entries := s.All()

	provider := &fakeEmbedProvider{err: errors.New("endpoint unreachable")}
	s.EnableRemoteEmbeddings(provider)

	if got := s.vectorScores(context.Background(), "hello", entries); got != nil {
		t.Fatalf("expected nil scores on failure, got %v", got)
	}
	if provider.calls != 1 {
		t.Fatalf("expected exactly 1 call before backoff kicks in, got %d", provider.calls)
	}

	// Immediately retrying should NOT call the provider again — it's
	// backing off after the failure.
	if got := s.vectorScores(context.Background(), "hello", entries); got != nil {
		t.Fatalf("expected nil scores while backing off, got %v", got)
	}
	if provider.calls != 1 {
		t.Fatalf("expected no additional call while backing off, got %d calls", provider.calls)
	}
}

func TestStore_Search_StillWorksWithEmbeddingsEnabledButFailing(t *testing.T) {
	// Search must degrade gracefully to pure BM25 ranking if the embeddings
	// endpoint is enabled but broken — never error out or return nothing.
	s := newTestStore(t, map[string]string{
		"a.md": "The deployment pipeline uses docker and kubernetes for releases.",
	})
	s.EnableRemoteEmbeddings(&fakeEmbedProvider{err: errors.New("boom")})

	results := s.Search("docker kubernetes deployment", 5)
	if len(results) == 0 {
		t.Fatal("expected BM25 fallback results despite a broken embeddings provider")
	}
}

type cancellingEmbedProvider struct {
	cancel context.CancelFunc
	calls  int
	ctxErr error
}

func (*cancellingEmbedProvider) Dim() int { return 3 }

func (provider *cancellingEmbedProvider) Embed(ctx context.Context, _ []string) ([][]float32, error) {
	provider.calls++
	provider.cancel()
	provider.ctxErr = ctx.Err()
	return nil, context.Canceled
}

func TestSearchContextCancelsEmbeddingWithoutBackoff(t *testing.T) {
	store := newTestStore(t, map[string]string{"a.md": "docker deployment configuration"})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	provider := &cancellingEmbedProvider{cancel: cancel}
	store.EnableRemoteEmbeddings(provider)
	if results := store.SearchContext(ctx, "docker", 5); len(results) != 0 {
		t.Fatalf("cancelled search returned results: %+v", results)
	}
	if provider.calls != 1 || !errors.Is(provider.ctxErr, context.Canceled) {
		t.Fatalf("embedding did not receive the request context: calls=%d err=%v", provider.calls, provider.ctxErr)
	}
	if !store.embedBackoffUntil.IsZero() {
		t.Fatal("user cancellation must not disable semantic search")
	}
	provider.calls = 0
	store.SearchContext(ctx, "docker", 5)
	if provider.calls != 0 {
		t.Fatal("already cancelled search called embeddings")
	}
}

func TestBM25UpdateAndClearMaintainLengths(t *testing.T) {
	index := NewBM25(1.2, 0.75)
	updated := time.Unix(1, 0)
	index.Index(7, "docker deployment", updated)
	index.Index(9, "compiler", updated)
	index.Index(7, "compiler configuration testing", updated)
	if len(index.tokens) != 2 || index.totalLength != 4 || index.avgDL != 2 {
		t.Fatalf("updated index: documents=%d length=%d average=%v", len(index.tokens), index.totalLength, index.avgDL)
	}
	if found := index.Search("docker", 5); len(found) != 0 {
		t.Fatalf("updated document kept stale terms: %+v", found)
	}
	index.Clear()
	index.Index(7, "docker", updated)
	if len(index.tokens) != 1 || index.totalLength != 1 || index.avgDL != 1 {
		t.Fatalf("cleared index retained previous lengths: %+v", index)
	}
	if found := index.Search("docker", 5); len(found) != 1 || found[0].ID != 7 {
		t.Fatalf("cleared index cannot be reused: %+v", found)
	}
}

func TestSearchIndexReusedAndInvalidated(t *testing.T) {
	store := newTestStore(t, map[string]string{"a.md": "docker deployment"})
	store.cwd = store.PrimaryDir()
	store.cacheTTL = time.Hour
	store.Search("docker", 5)
	first := store.searchIndex
	if first == nil {
		t.Fatal("search did not build an index")
	}
	var searches sync.WaitGroup
	for index := 0; index < 8; index++ {
		searches.Add(1)
		go func() {
			defer searches.Done()
			if results := store.Search("docker", 5); len(results) != 1 || results[0].Entry.Name != "a.md" {
				t.Errorf("concurrent search returned %+v", results)
			}
		}()
	}
	searches.Wait()
	if store.searchIndex != first {
		t.Fatal("unchanged memories rebuilt the index")
	}
	if err := store.Save("a.md", "compiler configuration"); err != nil {
		t.Fatal(err)
	}
	if results := store.Search("docker", 5); len(results) != 0 {
		t.Fatalf("saved memory kept stale terms: %+v", results)
	}
	if store.searchIndex == first {
		t.Fatal("save did not replace the search index")
	}
	if results := first.Search("docker", 5); len(results) != 1 {
		t.Fatalf("replacing the cache mutated a published snapshot: %+v", results)
	}
	if results := store.Search("compiler", 5); len(results) != 1 || results[0].Entry.Content != "compiler configuration" {
		t.Fatalf("new memory missing from index: %+v", results)
	}
	if err := store.Delete("a.md"); err != nil {
		t.Fatal(err)
	}
	if results := store.Search("compiler", 5); len(results) != 0 {
		t.Fatalf("deleted memory remained searchable: %+v", results)
	}
}

func BenchmarkBM25Build(b *testing.B) {
	for _, count := range []int{100, 1000, 5000} {
		b.Run(fmt.Sprintf("documents_%d", count), func(b *testing.B) {
			updated := time.Unix(0, 0)
			b.ReportAllocs()
			for iteration := 0; iteration < b.N; iteration++ {
				index := NewBM25(1.2, 0.75)
				for document := 0; document < count; document++ {
					index.Index(document, "project memory compiler configuration testing", updated)
				}
			}
		})
	}
}

func TestSearchIndexRefreshesAfterExternalEdit(t *testing.T) {
	store := newTestStore(t, map[string]string{"a.md": "docker deployment"})
	store.cwd = store.PrimaryDir()
	store.cacheTTL = time.Hour
	store.Search("docker", 5)
	first := store.searchIndex
	if err := os.WriteFile(filepath.Join(store.PrimaryDir(), "a.md"), []byte("compiler configuration"), 0600); err != nil {
		t.Fatal(err)
	}
	store.cacheTime = time.Time{}
	if results := store.Search("docker", 5); len(results) != 0 {
		t.Fatalf("external edit left stale search terms: %+v", results)
	}
	if store.searchIndex == first {
		t.Fatal("expired directory cache retained its old search index")
	}
	if results := store.Search("compiler", 5); len(results) != 1 || results[0].Entry.Content != "compiler configuration" {
		t.Fatalf("external edit missing from rebuilt index: %+v", results)
	}
}
