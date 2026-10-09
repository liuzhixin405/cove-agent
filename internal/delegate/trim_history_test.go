package delegate

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/liuzhixin405/cove-agent/internal/api"
	"github.com/liuzhixin405/cove-agent/internal/tool"
)

func TestTrimHistoryCutsOldToolResultsOnly(t *testing.T) {
	big := strings.Repeat("x", 30000)
	msgs := []api.Message{{Role: "user", Content: "task"}}
	for i := 0; i < 10; i++ {
		msgs = append(msgs, api.Message{Role: "assistant", ToolCalls: []api.ToolCall{{ID: "c", Name: "read"}}},
			api.Message{Role: "tool", ToolCallID: "c", Content: big})
	}
	out := trimHistory(msgs, 40000)
	var trimmed, full int
	for _, m := range out {
		if m.Role != "tool" {
			continue
		}
		if strings.HasSuffix(m.Content, trimmedToolNote) {
			trimmed++
		} else {
			full++
		}
	}
	if trimmed == 0 || full < keepRecentToolResults {
		t.Fatalf("trimmed %d, full %d", trimmed, full)
	}
	if last := out[len(out)-1].Content; last != big {
		t.Fatal("the latest tool result was trimmed")
	}
	if got := trimHistory([]api.Message{{Role: "user", Content: "hi"}}, 10); len(got) != 1 || got[0].Content != "hi" {
		t.Fatalf("small history changed: %+v", got)
	}
}

type captureProvider struct {
	api.Provider
	system string
}

func (p *captureProvider) Chat(_ context.Context, req api.ChatRequest) (*api.ChatResponse, error) {
	p.system = req.SystemBase
	return &api.ChatResponse{Content: "done"}, nil
}

func TestDelegateReportsProgress(t *testing.T) {
	d := NewDelegator(&captureProvider{}, "m", nil)
	var lines []string
	d.SetProgress(func(l string) { lines = append(lines, l) })
	d.DelegateWith(context.Background(), "agent-explore-1", "find the parser", "sys", Options{})
	if len(lines) != 2 || !strings.HasPrefix(lines[0], "▸ agent-explore-1 开始：find the parser") || !strings.HasPrefix(lines[1], "✓ agent-explore-1 完成") {
		t.Fatalf("progress = %q", lines)
	}
}

func TestDelegateStructuredEventsWithoutProgress(t *testing.T) {
	d := NewDelegator(&captureProvider{}, "m", nil)
	var events []Event
	d.SetEventSink(func(event Event) { events = append(events, event) })
	for range 2 {
		d.DelegateWith(context.Background(), "explore", "find parser", "sys", Options{})
	}
	if len(events) != 6 {
		t.Fatalf("events = %d, want two start/model/finish sequences", len(events))
	}
	for _, offset := range []int{0, 3} {
		if events[offset].Stage != "starting" || events[offset+1].Stage != "model" || events[offset+2].Stage != "finished" || !events[offset+2].Success {
			t.Fatalf("incorrect lifecycle: %+v", events[offset:offset+3])
		}
		for _, event := range events[offset : offset+3] {
			if event.ID != events[offset].ID || event.TaskID != "explore" || event.Task != "find parser" || event.Model != "m" || event.At.IsZero() {
				t.Fatalf("missing event identity: %+v", event)
			}
		}
	}
	if events[0].ID == events[3].ID {
		t.Fatal("repeated task IDs shared an execution identity")
	}
	rebuilt := NewDelegator(&captureProvider{}, "m", nil)
	rebuilt.SetEventSink(func(event Event) { events = append(events, event) })
	rebuilt.DelegateWith(context.Background(), "explore", "find parser", "sys", Options{})
	if events[6].ID == events[0].ID || events[6].ID == events[3].ID {
		t.Fatal("rebuilt delegator reused an execution identity")
	}
}

func TestDelegateStructuredCancellationAndTimeout(t *testing.T) {
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	timedOut, stop := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer stop()
	for _, sample := range []struct {
		ctx   context.Context
		stage string
	}{{cancelled, "cancelled"}, {timedOut, "timed_out"}} {
		t.Run(sample.stage, func(t *testing.T) {
			delegator := NewDelegator(&captureProvider{}, "m", nil)
			var events []Event
			delegator.SetEventSink(func(event Event) { events = append(events, event) })
			result := delegator.DelegateWith(sample.ctx, "stopped", "inspect", "sys", Options{})
			if result.Success || len(events) != 2 || events[0].Stage != "starting" || events[1].Stage != sample.stage || events[1].Success {
				t.Fatalf("incorrect stopped lifecycle: %+v", events)
			}
		})
	}
}

func TestActivityStoreKeepsTerminalStateAndBoundsHistory(t *testing.T) {
	var store ActivityStore
	wake := store.Wake()
	start := time.Now()
	store.Update(Event{ID: "active", TaskID: "explore", Stage: "starting", At: start})
	for index := range 150 {
		id := fmt.Sprintf("run-%d", index)
		store.Update(Event{ID: id, Stage: "starting", At: start.Add(time.Duration(index) * time.Second)})
		store.Update(Event{ID: id, Stage: "finished", Success: true, At: start.Add(time.Duration(index+1) * time.Second)})
		store.Update(Event{ID: id, Stage: "tool", At: start.Add(time.Duration(index+2) * time.Second)})
	}
	entries := store.Snapshot()
	if len(entries) != 128 {
		t.Fatalf("history size = %d, want 128", len(entries))
	}
	if entries[0].ID != "active" || !entries[0].Ended.IsZero() {
		t.Fatal("history eviction removed a running agent")
	}
	for _, entry := range entries[1:] {
		if entry.Stage != "finished" || !entry.Success {
			t.Fatal("late progress overwrote a terminal state")
		}
	}
	select {
	case <-wake:
	default:
		t.Fatal("state changes did not wake the observer")
	}
}

func TestActivityStoreConcurrentSnapshots(t *testing.T) {
	var store ActivityStore
	var workers sync.WaitGroup
	for index := range 20 {
		workers.Add(1)
		go func() {
			defer workers.Done()
			id := fmt.Sprintf("parallel-%d", index)
			for range 30 {
				store.Update(Event{ID: id, Stage: "tool", Summary: "read file.go", At: time.Now()})
				_ = store.Snapshot()
			}
			store.Update(Event{ID: id, Stage: "finished", Success: true, At: time.Now()})
		}()
	}
	workers.Wait()
	entries := store.Snapshot()
	if len(entries) != 20 {
		t.Fatalf("parallel agents lost records: %d", len(entries))
	}
	for _, entry := range entries {
		if !entry.Success || entry.Ended.IsZero() || entry.LastStep != "read file.go" {
			t.Fatal("parallel record lost its final state or last step")
		}
	}
}

type toolsProvider struct {
	api.Provider
	tools []string
}

func (p *toolsProvider) Chat(_ context.Context, req api.ChatRequest) (*api.ChatResponse, error) {
	for _, d := range req.Tools {
		p.tools = append(p.tools, d.Name)
	}
	return &api.ChatResponse{Content: "done"}, nil
}

// A code review gets no web tools: it spent seven searches on the
// definition of a median.
func TestDelegateExcludesTools(t *testing.T) {
	p := &toolsProvider{}
	d := NewDelegator(p, "m", []tool.Tool{&namedTool{name: "read", readOnly: true}, &namedTool{name: "websearch", readOnly: true}, &namedTool{name: "grep", readOnly: true}})
	d.DelegateWith(context.Background(), "r1", "review", "sys", Options{ReadOnly: true, Exclude: []string{"websearch"}})
	if strings.Join(p.tools, ",") != "read,grep" {
		t.Fatalf("tools offered = %v", p.tools)
	}
}

type overloadedProvider struct {
	api.Provider
	models []string
}

func (p *overloadedProvider) Chat(_ context.Context, req api.ChatRequest) (*api.ChatResponse, error) {
	p.models = append(p.models, req.Model)
	if req.Model == "main" {
		return nil, &api.RetryableError{Status: 503, Msg: "high demand"}
	}
	return &api.ChatResponse{Content: "done"}, nil
}

// A sub-agent whose model is overloaded moves to the fallback once instead
// of dying on the first 503.
func TestSubAgentFallsBackOnOverload(t *testing.T) {
	p := &overloadedProvider{}
	d := NewDelegator(p, "main", nil)
	d.SetFallback(func(m string) string {
		if m == "main" {
			return "fast"
		}
		return ""
	})
	var lines []string
	d.SetProgress(func(l string) { lines = append(lines, l) })
	res := d.DelegateWith(context.Background(), "a1", "task", "sys", Options{})
	if !res.Success || strings.Join(p.models, ",") != "main,fast" {
		t.Fatalf("result %+v, models %v", res, p.models)
	}
	if !strings.Contains(strings.Join(lines, "\n"), "改用 fast") {
		t.Fatalf("progress = %q", lines)
	}
}

func TestDelegateAppendsProjectContext(t *testing.T) {
	p := &captureProvider{}
	d := NewDelegator(p, "m", nil)
	d.SetContextSource(func() string { return "# Project context\n\nAGENTS rule" })
	d.DelegateWith(context.Background(), "t1", "do it", "You are a sub-agent.", Options{})
	if !strings.HasPrefix(p.system, "You are a sub-agent.") || !strings.Contains(p.system, "AGENTS rule") {
		t.Fatalf("system prompt = %q", p.system)
	}
}
