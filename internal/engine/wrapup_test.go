package engine

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/liuzhixin405/cove-agent/internal/api"
)

const testWrapUp = "已完成：读取了三个文件。未完成：修改。建议下一步：继续修改。"

// wrapUpProvider calls read_tool on every request that offers tools and
// answers a tool-less request (the wrap-up) with testWrapUp, or wrapErr.
func wrapUpProvider(wrapErr error) *seqProvider {
	return &seqProvider{reply: func(_ context.Context, n int, req api.ChatRequest) (*api.ChatResponse, error) {
		if len(req.Tools) == 0 {
			if wrapErr != nil {
				return nil, wrapErr
			}
			return &api.ChatResponse{Content: testWrapUp}, nil
		}
		return toolCallResp(fmt.Sprintf("c%d", n), "read_tool", map[string]any{"query": fmt.Sprintf("q%d", n)}), nil
	}}
}

func TestWrapUpOnLimitStop(t *testing.T) {
	prov := wrapUpProvider(nil)
	eng := limitEngine(t, prov, 3)
	eng.IterationLimitPrompt = func(LimitStats) LimitDecision { return LimitStop }

	var streamed strings.Builder
	_, err := eng.RunMessageWithStream(context.Background(), api.Message{Role: "user", Content: "做点事"},
		func(d string) { streamed.WriteString(d) }, nil)
	var le *LimitError
	if !errors.As(err, &le) {
		t.Fatalf("err = %v, want a LimitError", err)
	}
	reqs := prov.requests()
	if len(reqs) != 4 {
		t.Fatalf("model called %d times, want 3 + the wrap-up", len(reqs))
	}
	w := reqs[3]
	if len(w.Tools) != 0 || w.MaxTokens <= 0 || w.MaxTokens > 1024 {
		t.Fatalf("wrap-up request: tools=%d max_tokens=%d, want no tools and <= 1024", len(w.Tools), w.MaxTokens)
	}
	lastReq := w.Messages[len(w.Messages)-1]
	if !strings.Contains(lastReq.Content, "The run is stopping (") || !strings.Contains(lastReq.Content, "Without calling tools") {
		t.Fatalf("wrap-up prompt = %q", lastReq.Content)
	}
	last := eng.messages[len(eng.messages)-1]
	if last.Role != "assistant" || last.Content != testWrapUp {
		t.Fatalf("last history message = %+v, want the wrap-up summary", last)
	}
	if !strings.Contains(streamed.String(), testWrapUp) {
		t.Fatalf("the summary was not streamed: %q", streamed.String())
	}
	// The wrap-up prompt itself is not kept.
	if got := countMsgs(eng.messages, "The run is stopping"); got != 0 {
		t.Fatalf("wrap-up prompt kept in history %d times", got)
	}
}

func TestWrapUpOnPrintModeHardCap(t *testing.T) {
	prov := wrapUpProvider(nil)
	eng := limitEngine(t, prov, 3) // no IterationLimitPrompt: the -p hard cap
	_, err := run(t, eng, "做点事")
	var le *LimitError
	if !errors.As(err, &le) || le.Reason != LimitReasonIterations {
		t.Fatalf("err = %#v, want the iteration LimitError", err)
	}
	if got := eng.LastWrapUp(); got != testWrapUp {
		t.Fatalf("LastWrapUp = %q, want the summary for -p to print", got)
	}
}

func TestWrapUpSkippedWhenBudgetExhausted(t *testing.T) {
	prov := wrapUpProvider(nil)
	eng := limitEngine(t, prov, 3)
	exhaustBudget(eng)
	if s, err := eng.wrapUpSummary(context.Background(), "test-model", "测试"); err == nil || s != "" {
		t.Fatalf("wrapUpSummary = %q, %v; want it skipped", s, err)
	}
	if n := len(prov.requests()); n != 0 {
		t.Fatalf("model called %d times with the budget exhausted", n)
	}
}

func TestWrapUpSkippedWhenCancelled(t *testing.T) {
	prov := wrapUpProvider(nil)
	eng := limitEngine(t, prov, 3)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if s, err := eng.wrapUpSummary(ctx, "test-model", "测试"); err == nil || s != "" {
		t.Fatalf("wrapUpSummary = %q, %v; want it skipped", s, err)
	}
	if n := len(prov.requests()); n != 0 {
		t.Fatalf("model called %d times after cancel", n)
	}
}

func TestWrapUpFailureIsSilent(t *testing.T) {
	prov := wrapUpProvider(errors.New("boom"))
	eng := limitEngine(t, prov, 3)
	_, err := run(t, eng, "做点事")
	var le *LimitError
	if !errors.As(err, &le) {
		t.Fatalf("err = %v, want the LimitError unchanged by a failed wrap-up", err)
	}
	if eng.LastWrapUp() != "" {
		t.Fatalf("LastWrapUp = %q after a failed wrap-up", eng.LastWrapUp())
	}
	for _, m := range eng.messages {
		if m.Role == "assistant" && len(m.ToolCalls) == 0 {
			t.Fatalf("assistant text %q appended although the wrap-up failed", m.Content)
		}
	}
}

func TestWrapUpUsesRoutedModelWithoutThinking(t *testing.T) {
	prov := wrapUpProvider(nil)
	eng := limitEngine(t, prov, 3)
	eng.config.Thinking, eng.config.Effort = "adaptive", "high"
	if _, err := eng.wrapUpSummary(context.Background(), "routed-fast", "测试"); err != nil {
		t.Fatal(err)
	}
	r := prov.requests()[0]
	if r.Model != "routed-fast" || r.Thinking != "disabled" || r.Effort != "" {
		t.Fatalf("wrap-up request model=%q thinking=%q effort=%q, want the routed model with thinking off", r.Model, r.Thinking, r.Effort)
	}
}

// Claude Opus 5.5 (and Haiku 5.5 above effort "high") answer
// thinking:{type:"disabled"} with a 400: thinking is always on there. The
// wrap-up must then ask again with the model's default instead of losing
// the summary, which it used to do silently.
func TestWrapUpRetriesWithoutThinkingWhenModelRejectsDisabled(t *testing.T) {
	prov := &seqProvider{reply: func(_ context.Context, n int, req api.ChatRequest) (*api.ChatResponse, error) {
		if req.Thinking == "disabled" {
			return nil, &api.StatusError{Status: 400, Msg: `{"type":"error","error":{"type":"invalid_request_error","message":"\"thinking.type.disabled\" is not supported for this model. Use \"thinking.type.adaptive\" and \"output_config.effort\" to control thinking behavior."}}`}
		}
		return &api.ChatResponse{Content: testWrapUp}, nil
	}}
	eng := limitEngine(t, prov, 3)
	s, err := eng.wrapUpSummary(context.Background(), "anthropic.claude-opus-5-5", "测试")
	if err != nil || s != testWrapUp {
		t.Fatalf("wrapUpSummary = %q, %v; want the summary from the retry", s, err)
	}
	reqs := prov.requests()
	if len(reqs) != 2 || reqs[0].Thinking != "disabled" || reqs[1].Thinking != "" {
		t.Fatalf("requests = %d (thinking %q then %q), want disabled first, then the model default", len(reqs), reqs[0].Thinking, reqs[len(reqs)-1].Thinking)
	}
}

// Any other 400 is not about thinking and is not retried.
func TestWrapUpDoesNotRetryUnrelatedBadRequest(t *testing.T) {
	prov := wrapUpProvider(&api.StatusError{Status: 400, Msg: "max_tokens: must be positive"})
	eng := limitEngine(t, prov, 3)
	if s, err := eng.wrapUpSummary(context.Background(), "test-model", "测试"); err == nil || s != "" {
		t.Fatalf("wrapUpSummary = %q, %v; want the error", s, err)
	}
	if n := len(prov.requests()); n != 1 {
		t.Fatalf("model called %d times, want once", n)
	}
}
