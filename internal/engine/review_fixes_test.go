package engine

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/liuzhixin405/cove-agent/internal/api"
	"github.com/liuzhixin405/cove-agent/internal/diagnostic"
)

// A 16K window cannot hold cove's ~13K of system prompt and tool definitions
// plus room for a reply, so the compaction trigger falls below the fixed
// overhead and every iteration used to run a summary call. Now the engine
// says so once and stops compacting automatically.
func TestSmallWindowDoesNotCompactEveryTurn(t *testing.T) {
	t.Cleanup(api.ClearModelContextWindows)
	api.SetModelContextWindow("test-model", 16384)
	prov := &mockProvider{}
	eng := newTestEngine(prov)
	eng.requestOverhead = 13000
	for i := 0; i < 20; i++ {
		eng.messages = append(eng.messages,
			api.Message{Role: "user", Content: fmt.Sprintf("question %d about the parser and its many edge cases", i)},
			api.Message{Role: "assistant", Content: fmt.Sprintf("answer %d about the parser and its many edge cases", i)})
	}
	var lines []string
	eng.SetOutput(LineSink(func(line string) { lines = append(lines, line) }))

	eng.checkAndCompress(context.Background(), "test-model")
	eng.checkAndCompress(context.Background(), "test-model")

	prov.mu.Lock()
	calls := prov.callCount
	prov.mu.Unlock()
	if calls != 0 {
		t.Fatalf("compaction called the model %d times on a window too small to hold the overhead", calls)
	}
	warned := 0
	for _, l := range lines {
		if strings.Contains(l, "自动压缩") && strings.Contains(l, "16384") {
			warned++
		}
	}
	if warned != 1 {
		t.Fatalf("small-window warning shown %d times, want once:\n%s", warned, strings.Join(lines, "\n"))
	}
}

// One tool call with unparsable arguments is one E4009 event in a full turn:
// the result loop must not add an uncoded "工具 bash 失败" line for it.
func TestUnparsableToolArgsRecordedOnceInAFullTurn(t *testing.T) {
	clearDiagnostics(t)
	prov := &mockProvider{responses: []mockResponse{
		{toolCalls: []api.ToolCall{{ID: "1", Name: "bash", ParseError: true, Input: map[string]any{"_cove_parse_error": "tool call arguments were not valid JSON"}}}},
		{content: "ok"},
	}}
	eng := newTestEngine(prov)
	if _, err := eng.RunMessageWithStream(context.Background(), api.Message{Role: "user", Content: "hi"}, nil, nil); err != nil {
		t.Fatalf("turn failed: %v", err)
	}
	coded, uncoded := 0, 0
	for _, ev := range diagnostic.RecentRuntime() {
		switch {
		case ev.Code == diagnostic.ErrToolArgsInvalid:
			coded++
		case ev.Code == "" && strings.Contains(ev.Message, "bash"):
			uncoded++
		}
	}
	if coded != 1 || uncoded != 0 {
		t.Fatalf("E4009 ×%d, uncoded bash failures ×%d; want 1 and 0: %+v", coded, uncoded, diagnostic.RecentRuntime())
	}
}

// Ctrl+C during a model call is the user's doing, not a problem to record.
func TestCanceledCallIsNotReported(t *testing.T) {
	clearDiagnostics(t)
	prov := &mockProvider{responses: []mockResponse{{delay: 500 * time.Millisecond, content: "late"}}}
	eng := newTestEngine(prov)
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()
	if _, err := eng.RunMessageWithStream(ctx, api.Message{Role: "user", Content: "hi"}, nil, nil); err == nil {
		t.Fatal("want the cancellation back")
	}
	for _, ev := range diagnostic.RecentRuntime() {
		if ev.Source == "report" {
			t.Errorf("cancellation recorded as a problem: %+v", ev)
		}
	}
}

// A stage that stays stuck is warned about every threshold, but recorded as
// one E5007: the count in /diagnose errors is stalls, not reminders.
func TestStallRecordedOncePerStage(t *testing.T) {
	clearDiagnostics(t)
	eng := newTestEngine(&mockProvider{})
	var lines []string
	eng.SetOutput(LineSink(func(line string) { lines = append(lines, line) }))
	eng.reportStall("call model qwen", 30*time.Second, true)
	eng.reportStall("call model qwen", 60*time.Second, false)
	if len(lines) != 2 {
		t.Fatalf("stall lines = %d, want 2", len(lines))
	}
	n := 0
	for _, ev := range diagnostic.RecentRuntime() {
		if ev.Code == diagnostic.ErrEngineStall {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("E5007 recorded %d times, want 1", n)
	}
}

// /model and /provider go through ReloadProvider; the router used to keep the
// construction-time model, so requests kept naming the old one.
func TestReloadProviderRetargetsTheRouter(t *testing.T) {
	prov := &mockProvider{}
	eng := newTestEngine(prov)
	if err := eng.ReloadProvider("openai", "model-b", "", "sk-test"); err != nil {
		t.Fatalf("ReloadProvider: %v", err)
	}
	eng.SetProvider(prov) // keep answering from the mock
	if _, err := eng.RunMessageWithStream(context.Background(), api.Message{Role: "user", Content: "hi"}, nil, nil); err != nil {
		t.Fatalf("turn failed: %v", err)
	}
	prov.mu.Lock()
	got := prov.lastReq.Model
	prov.mu.Unlock()
	if got != "model-b" {
		t.Fatalf("request model = %q, want model-b after /model", got)
	}
}

func TestReloadProviderPreservesAndDisablesImageFilesAPI(t *testing.T) {
	eng := newPatternEngine(t, &seqProvider{}, func(cfg *Config) {
		cfg.Provider.Name = "deepseek"
		cfg.Provider.ImageFilesAPI = true
	})
	if err := eng.ReloadProvider("deepseek", "deepseek-flash", "", "sk-test"); err != nil {
		t.Fatal(err)
	}
	if !eng.config.Provider.ImageFilesAPI {
		t.Fatal("model reload lost Files API opt-in")
	}
	cfg := eng.config.Provider
	cfg.ImageFilesAPI = false
	if err := eng.ReloadProviderConfig(cfg, "deepseek-flash"); err != nil {
		t.Fatal(err)
	}
	if eng.config.Provider.ImageFilesAPI {
		t.Fatal("explicit Files API disable was ignored")
	}
}

// learningRuntime forwards the remedy's window to the api layer, like the
// CLI's Runtime does, and keeps its notes.
type learningRuntime struct{ notes []string }

func (r *learningRuntime) SetModelContextWindow(model string, tokens int) {
	api.SetModelContextWindow(model, tokens)
}
func (r *learningRuntime) Notify(line string) { r.notes = append(r.notes, line) }

// The first overflow is reported before the compact-and-retry, so the E2008
// remedy learns the server's window at once and the retry compacts to a
// target that fits it, instead of failing once more first.
func TestFirstOverflowIsReportedBeforeRetry(t *testing.T) {
	clearDiagnostics(t)
	t.Cleanup(api.ClearModelContextWindows)
	rt := &learningRuntime{}
	diagnostic.SetRuntime(rt)
	var history []api.Message
	for i := 0; i < 12; i++ {
		history = append(history,
			api.Message{Role: "user", Content: fmt.Sprintf("question %d about the parser", i)},
			api.Message{Role: "assistant", Content: fmt.Sprintf("answer %d about the parser", i)})
	}
	prov := &mockProvider{responses: []mockResponse{
		{err: &api.StatusError{Status: 400, Msg: llamaOverflowMsg}},
		{content: "Summary: questions and answers about the parser."},
		{content: "ok"},
	}}
	eng := newTestEngine(prov)
	eng.LoadMessages(history)
	reply, err := eng.RunMessageWithStream(context.Background(), api.Message{Role: "user", Content: "next"}, nil, nil)
	if err != nil || reply != "ok" {
		t.Fatalf("reply = %q, err = %v", reply, err)
	}
	if w := api.ContextWindowForModel("test-model"); w != 16384 {
		t.Errorf("window not learned on the first overflow: %d", w)
	}
	coded, recovered := 0, 0
	for _, ev := range diagnostic.RecentRuntime() {
		switch {
		case ev.Code == diagnostic.ErrAPIContextLength && ev.Severity == diagnostic.SevRecovered:
			recovered++
		case ev.Code == diagnostic.ErrAPIContextLength:
			coded++
		}
	}
	if coded != 1 || recovered != 1 {
		t.Errorf("E2008 recorded %d times and remedied %d times, want 1 and 1", coded, recovered)
	}
}

// A reply cut off by max_tokens with nothing in it (a reasoning model that
// spent the whole budget thinking) used to be re-requested until the
// iteration cap, billing a full reply each time. Now the turn stops after a
// few and says why.
func TestEmptyTruncatedRepliesStopTheTurn(t *testing.T) {
	var responses []mockResponse
	for i := 0; i < 10; i++ {
		responses = append(responses, mockResponse{stopReason: "length"})
	}
	prov := &mockProvider{responses: responses}
	eng := newTestEngine(prov)
	_, err := eng.RunMessageWithStream(context.Background(), api.Message{Role: "user", Content: "hi"}, nil, nil)
	if err == nil || !strings.Contains(err.Error(), "截断") {
		t.Fatalf("err = %v, want a truncation stop", err)
	}
	prov.mu.Lock()
	calls := prov.callCount
	prov.mu.Unlock()
	if calls > 3 {
		t.Fatalf("model called %d times for empty truncated replies, want at most 3", calls)
	}
}

// A local model spends a long time on a 13K prompt before its first token;
// the stall warning for model calls waits longer there than for a cloud API.
func TestStallThresholdIsLongerForLocalProviders(t *testing.T) {
	eng := newTestEngine(&mockProvider{})
	if got := eng.stallThresholdFor("执行工具 read"); got != 30*time.Second {
		t.Errorf("tool threshold = %v, want 30s", got)
	}
	// A build or test without output for a minute is normal.
	if got := eng.stallThresholdFor("执行工具 bash"); got != 2*time.Minute {
		t.Errorf("shell threshold = %v, want 2m", got)
	}
	if got := eng.stallThresholdFor("调用模型 x"); got != 30*time.Second {
		t.Errorf("cloud model threshold = %v, want 30s", got)
	}
	eng.config.Provider.BaseURL = "http://127.0.0.1:1234/v1"
	if got := eng.stallThresholdFor("调用模型 x"); got != 90*time.Second {
		t.Errorf("local model threshold = %v, want 90s", got)
	}
	eng.config.Provider.BaseURL = ""
	eng.config.Provider.Name = "ollama"
	if got := eng.stallThresholdFor("调用模型 x"); got != 90*time.Second {
		t.Errorf("ollama threshold = %v, want 90s", got)
	}
}
