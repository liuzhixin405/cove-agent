package engine

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/liuzhixin405/cove-agent/internal/api"
	"github.com/liuzhixin405/cove-agent/internal/cost"
	"github.com/liuzhixin405/cove-agent/internal/dream"
	"github.com/liuzhixin405/cove-agent/internal/extract"
	"github.com/liuzhixin405/cove-agent/internal/session"
)

// slowExtractProvider answers the extraction request after a delay with one
// memory, and records when it finished.
type slowExtractProvider struct {
	delay    time.Duration
	finished atomic.Bool
}

func (p *slowExtractProvider) Name() string        { return "x" }
func (p *slowExtractProvider) DisplayName() string { return "x" }
func (p *slowExtractProvider) Validate() error     { return nil }
func (p *slowExtractProvider) Chat(ctx context.Context, _ api.ChatRequest) (*api.ChatResponse, error) {
	select {
	case <-time.After(p.delay):
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	p.finished.Store(true)
	return &api.ChatResponse{Content: "---MEMORY---\nFILE: fact.md\nMODE: write\nCONTENT:\n项目使用 Go 1.25\n---END---"}, nil
}
func (p *slowExtractProvider) ChatStream(ctx context.Context, req api.ChatRequest, _ api.StreamHandler) (*api.ChatResponse, error) {
	return p.Chat(ctx, req)
}

type summaryRecorder struct {
	mu  sync.Mutex
	got []BackgroundSummary
}

func (r *summaryRecorder) record(s BackgroundSummary) {
	r.mu.Lock()
	r.got = append(r.got, s)
	r.mu.Unlock()
}

func (r *summaryRecorder) all() []BackgroundSummary {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]BackgroundSummary(nil), r.got...)
}

func extractingEngine(t *testing.T, delay time.Duration) (*Engine, *slowExtractProvider) {
	t.Helper()
	isolateHome(t)
	eng := newPatternEngine(t, &seqProvider{}, nil)
	xp := &slowExtractProvider{delay: delay}
	eng.setExtractRunner(extract.NewRunner(xp, "m"))
	// Extraction needs a few messages of history.
	eng.LoadMessages([]api.Message{
		{Role: "user", Content: "我们用 Go"}, {Role: "assistant", Content: "好的"},
	})
	return eng, xp
}

// cove -p waits for the memory extraction started at the end of its turn
// (the process used to exit first and kill it).
func TestWaitBackgroundWaitsForExtraction(t *testing.T) {
	eng, xp := extractingEngine(t, 150*time.Millisecond)
	if _, err := run(t, eng, "记住这个"); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	eng.WaitBackground(ctx)
	if !xp.finished.Load() {
		t.Fatal("WaitBackground returned before the extraction finished")
	}
}

func TestWaitBackgroundHonoursContext(t *testing.T) {
	eng, _ := extractingEngine(t, 3*time.Second)
	if _, err := run(t, eng, "记住这个"); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	eng.WaitBackground(ctx)
	if el := time.Since(start); el > time.Second {
		t.Fatalf("WaitBackground ignored its context: returned after %v", el)
	}
}

func TestBackgroundSummaryReportsExtractedMemories(t *testing.T) {
	eng, _ := extractingEngine(t, 10*time.Millisecond)
	rec := &summaryRecorder{}
	eng.OnBackgroundSummary = rec.record
	if _, err := run(t, eng, "记住这个"); err != nil {
		t.Fatal(err)
	}
	eng.WaitBackground(context.Background())
	got := rec.all()
	if len(got) != 1 || got[0].MemoriesExtracted != 1 || !got[0].SessionSaved {
		t.Fatalf("summaries = %+v, want one with MemoriesExtracted=1, SessionSaved", got)
	}
}

// A turn where nothing happened in the background says nothing.
func TestBackgroundSummarySilentWhenNothingHappened(t *testing.T) {
	isolateHome(t)
	eng := newPatternEngine(t, &seqProvider{}, nil)
	rec := &summaryRecorder{}
	eng.OnBackgroundSummary = rec.record
	if _, err := run(t, eng, "你好"); err != nil {
		t.Fatal(err)
	}
	eng.WaitBackground(context.Background())
	if got := rec.all(); len(got) != 0 {
		t.Fatalf("summaries = %+v, want none", got)
	}
}

func TestBackgroundExtractionDoesNotCallProviderOverBudget(t *testing.T) {
	isolateHome(t)
	provider := &seqProvider{reply: func(context.Context, int, api.ChatRequest) (*api.ChatResponse, error) {
		return &api.ChatResponse{Content: "NONE"}, nil
	}}
	eng, err := New(Config{
		Model: "test-model", MaxBudget: 0.01,
		Provider: api.ProviderConfig{Name: "mock", APIKey: "test"},
	})
	if err != nil {
		t.Fatal(err)
	}
	eng.provRef.Set(provider)
	eng.costTracker.Add("test-model", 1_000_000, 0)
	if !eng.costTracker.OverBudget() {
		t.Fatal("test did not exhaust the budget")
	}
	eng.extractRunner.Extract(context.Background(), []api.Message{
		{Role: "user", Content: "Project uses Go"}, {Role: "assistant", Content: "Noted"},
		{Role: "user", Content: "Remember this"}, {Role: "assistant", Content: "Done"},
	})
	if len(provider.requests()) != 0 {
		t.Fatal("background extraction called the model after budget exhaustion")
	}
}

func TestBackgroundBudgetProviderChecksEveryRequest(t *testing.T) {
	for _, streaming := range []bool{false, true} {
		for _, unlimited := range []bool{false, true} {
			name := "chat"
			if streaming {
				name = "stream"
			}
			if unlimited {
				name += "/unlimited"
			} else {
				name += "/bounded"
			}
			t.Run(name, func(t *testing.T) {
				tracker := cost.NewTracker(0.01)
				if unlimited {
					tracker.SetMaxBudget(0)
				}
				provider := &seqProvider{reply: func(context.Context, int, api.ChatRequest) (*api.ChatResponse, error) {
					return &api.ChatResponse{Content: "NONE", InputTokens: 1_000_000}, nil
				}}
				eng := &Engine{costTracker: tracker, llm: api.NewMeteredProvider(provider, func(model string, response *api.ChatResponse) {
					tracker.Add(model, response.InputTokens, response.OutputTokens)
				})}
				guarded := eng.backgroundProvider()
				chat := func() error {
					request := api.ChatRequest{Model: "test-model"}
					var err error
					if streaming {
						_, err = guarded.ChatStream(context.Background(), request, nil)
					} else {
						_, err = guarded.Chat(context.Background(), request)
					}
					return err
				}
				for attempt := 0; attempt < 3; attempt++ {
					err := chat()
					if !unlimited && attempt > 0 {
						if !errors.Is(err, errBackgroundBudget) {
							t.Fatalf("attempt %d: got %v, want budget rejection", attempt, err)
						}
					} else if err != nil {
						t.Fatal(err)
					}
				}
				wantCalls := 1
				if unlimited {
					wantCalls = 3
				}
				if got := len(provider.requests()); got != wantCalls {
					t.Fatalf("provider calls = %d, want %d", got, wantCalls)
				}
				tracker.SetMaxBudget(0)
				if err := chat(); err != nil {
					t.Fatalf("removing budget did not resume background calls: %v", err)
				}
				if got := len(provider.requests()); got != wantCalls+1 {
					t.Fatalf("provider calls after budget off = %d, want %d", got, wantCalls+1)
				}
			})
		}
	}
}

func TestBackgroundBudgetKeepsLocalBookkeeping(t *testing.T) {
	eng, provider := extractingEngine(t, 0)
	eng.costTracker.SetMaxBudget(0.01)
	eng.costTracker.Add("test-model", 1_000_000, 0)
	eng.reviewRunning = true
	rec := &summaryRecorder{}
	eng.OnBackgroundSummary = rec.record
	eng.bg.Add(1)
	eng.bgPending.Add(1)
	eng.runBackgroundWork(backgroundJob{
		learn: true, saved: true, checkpointed: true,
		msgs: []api.Message{
			{Role: "user", Content: "Use Go"}, {Role: "assistant", Content: "OK"},
			{Role: "user", Content: "Remember"}, {Role: "assistant", Content: "OK"},
		},
		review: []api.Message{{Role: "user", Content: "Review this"}},
	})
	if provider.finished.Load() {
		t.Fatal("background extraction ran despite budget exhaustion")
	}
	if eng.reviewRunning || eng.bgPending.Load() != 0 {
		t.Fatal("skipping learning left background work pending")
	}
	got := rec.all()
	if len(got) != 1 || !got[0].SessionSaved || len(got[0].Extra) != 1 || got[0].Extra[0] != checkpointHint {
		t.Fatalf("summaries = %+v, want saved session with checkpoint hint", got)
	}
}

func TestBackgroundSummaryReportThreshold(t *testing.T) {
	cases := []struct {
		s    BackgroundSummary
		want bool
	}{
		{BackgroundSummary{SessionSaved: true}, false},
		{BackgroundSummary{SessionSaved: true, MemoriesExtracted: 2}, true},
		{BackgroundSummary{SessionSaved: true, DreamChanged: true}, true},
		{BackgroundSummary{SessionSaved: false}, true},
	}
	for _, c := range cases {
		if got := c.s.Notable(); got != c.want {
			t.Fatalf("%+v.Notable() = %v, want %v", c.s, got, c.want)
		}
	}
}

// The dream gate's "sessions since the last consolidation" must not count the
// session in use; after a resume that is the resumed one.
func TestResumeSessionMovesDreamCurrentSession(t *testing.T) {
	home := isolateHome(t)
	dir := filepath.Join(home, ".cove", "sessions")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "session-old.jsonl"), []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	eng := newPatternEngine(t, &seqProvider{}, nil)
	eng.store = mustSessionStore(t)
	eng.dreamRunner = dream.NewRunner(nil, "m", "session-new")
	if n := eng.DreamStatus().SessionsSinceLast; n != 1 {
		t.Fatalf("before resume: SessionsSinceLast = %d, want 1", n)
	}
	eng.ResumeSession(&session.Record{ID: "session-old", Cwd: home})
	if n := eng.DreamStatus().SessionsSinceLast; n != 0 {
		t.Fatalf("after resume: SessionsSinceLast = %d, want 0 (the resumed session is in use)", n)
	}
}

// The first turn only records the dream gate; it is reported when it moves
// afterwards, or when a consolidation fires.
func TestDreamGateMovedNeedsAChange(t *testing.T) {
	eng := newPatternEngine(t, &seqProvider{}, nil)
	st := func(since int) dream.Status {
		return dream.Status{Enabled: true, MinSessions: 3, SessionsSinceLast: since, HoursSinceLast: -1}
	}
	if eng.dreamGateMoved(st(1), false) {
		t.Fatal("the first observation is a baseline, not a change")
	}
	if eng.dreamGateMoved(st(1), false) {
		t.Fatal("unchanged gate reported")
	}
	if !eng.dreamGateMoved(st(2), false) {
		t.Fatal("a session closer to the gate must be reported")
	}
	if !eng.dreamGateMoved(st(2), true) {
		t.Fatal("a consolidation that fired must be reported")
	}
}
