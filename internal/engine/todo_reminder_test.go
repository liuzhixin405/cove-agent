package engine

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/liuzhixin405/cove-agent/internal/api"
	ctxt "github.com/liuzhixin405/cove-agent/internal/context"
	"github.com/liuzhixin405/cove-agent/internal/memory"
)

func sampleTodos() []any {
	return []any{
		map[string]any{"content": "read the parser", "status": "completed", "priority": "high"},
		map[string]any{"content": "fix the tokenizer", "status": "in_progress", "priority": "high"},
		map[string]any{"content": "add tests", "status": "pending", "priority": "medium"},
	}
}

// The open task list comes back every todoReminderRounds rounds without a
// todowrite call; a todowrite round restarts the count.
func TestTodoRoundReminder(t *testing.T) {
	isolatedHome(t)
	eng := newTestEngine(&mockProvider{})
	eng.runtime.SetTodos(sampleTodos())

	read := []toolResult{{Name: "read"}}
	tr := &turn{}
	for i := 1; i < todoReminderRounds; i++ {
		if n := eng.todoRoundReminder(tr, read); n != "" {
			t.Fatalf("reminder after %d rounds: %q", i, n)
		}
	}
	n := eng.todoRoundReminder(tr, read)
	if !strings.Contains(n, "<todo_list>") || !strings.Contains(n, "[>] todo-2. fix the tokenizer [high]") || !strings.Contains(n, "[ ] todo-3. add tests") {
		t.Fatalf("reminder = %q", n)
	}
	if eng.todoRoundReminder(tr, read) != "" {
		t.Fatal("the count did not restart after a reminder")
	}

	for i := 0; i < todoReminderRounds-2; i++ {
		eng.todoRoundReminder(tr, read)
	}
	eng.todoRoundReminder(tr, []toolResult{{Name: "TodoWrite"}})
	if eng.roundsSinceTodo != 0 {
		t.Fatalf("todowrite did not restart the count: %d", eng.roundsSinceTodo)
	}
}

func TestTodoRoundReminderQuietWhenAllDone(t *testing.T) {
	isolatedHome(t)
	eng := newTestEngine(&mockProvider{})
	eng.runtime.SetTodos([]any{map[string]any{"content": "x", "status": "completed", "priority": "low"}})
	for i := 0; i < 3*todoReminderRounds; i++ {
		if n := eng.todoRoundReminder(&turn{}, []toolResult{{Name: "read"}}); n != "" {
			t.Fatalf("finished list reminded: %q", n)
		}
	}
}

// A turn ending with open items of a list it wrote is asked about them once.
func TestTodoFinishNudge(t *testing.T) {
	isolatedHome(t)
	eng := newTestEngine(&mockProvider{})
	tr := &turn{todoTouched: true}
	if eng.todoFinishNudge(tr) != "" {
		t.Fatal("no list, yet a nudge")
	}
	eng.runtime.SetTodos(sampleTodos())
	n := eng.todoFinishNudge(tr)
	if !strings.Contains(n, "2 item(s)") || !strings.Contains(n, "add tests") {
		t.Fatalf("nudge = %q", n)
	}
	if eng.todoFinishNudge(tr) != "" {
		t.Fatal("nudged twice in one turn")
	}
}

// An overloaded model (503 after the provider's retries) hands the turn to
// the other configured model instead of failing it.
func TestOverloadFallsBackToTheOtherModel(t *testing.T) {
	isolatedHome(t)
	prov := &mockProvider{responses: []mockResponse{
		{err: &api.RetryableError{Status: 503, Msg: "high demand"}},
		{content: "done on the fast model"},
	}}
	eng := newTestEngine(prov)
	eng.config.ModelFast = "fast-model"
	var lines []string
	eng.SetOutput(LineSink(func(s string) { lines = append(lines, s) }))
	reply, err := eng.RunMessageWithStream(context.Background(), api.Message{Role: "user", Content: "do it"}, nil, nil)
	if err != nil || reply != "done on the fast model" {
		t.Fatalf("reply %q, err %v", reply, err)
	}
	if prov.lastReq.Model != "fast-model" || !strings.Contains(strings.Join(lines, ""), "改用 fast-model") {
		t.Fatalf("last model %q, lines %q", prov.lastReq.Model, lines)
	}

	// A second overload in the same turn fails it: one fallback per turn.
	prov2 := &mockProvider{responses: []mockResponse{
		{err: &api.RetryableError{Status: 503}}, {err: &api.RetryableError{Status: 503}},
	}}
	eng2 := newTestEngine(prov2)
	eng2.config.ModelFast = "fast-model"
	if _, err := eng2.RunMessageWithStream(context.Background(), api.Message{Role: "user", Content: "do it"}, nil, nil); err == nil {
		t.Fatal("two overloads did not fail the turn")
	}
}

// Compaction drops the todowrite result; the list goes on the summary.
func TestTodoListSurvivesCompaction(t *testing.T) {
	isolatedHome(t)
	eng := newTestEngine(&mockProvider{})
	eng.runtime.SetTodos(sampleTodos())
	eng.messages = []api.Message{{Role: "user", Content: "<original_request>x</original_request> summary"}, {Role: "assistant", Content: "ok"}}
	eng.todoAfterCompaction(true)
	if !strings.Contains(eng.messages[0].Content, "fix the tokenizer") {
		t.Fatalf("summary lacks the task list: %q", eng.messages[0].Content)
	}
}

// Resuming rebuilds the list from the last todowrite call; a history without
// one has no list.
func TestLoadMessagesRestoresTodos(t *testing.T) {
	isolatedHome(t)
	eng := newTestEngine(&mockProvider{})
	old := []any{map[string]any{"content": "stale", "status": "pending", "priority": "low"}}
	eng.LoadMessages([]api.Message{
		{Role: "user", Content: "go"},
		{Role: "assistant", ToolCalls: []api.ToolCall{{ID: "1", Name: "todowrite", Input: map[string]any{"todos": old}}}},
		{Role: "tool", ToolCallID: "1", Content: "ok"},
		{Role: "assistant", ToolCalls: []api.ToolCall{{ID: "2", Name: "todowrite", Input: map[string]any{"todos": sampleTodos()}}}},
		{Role: "tool", ToolCallID: "2", Content: "ok"},
	})
	list, open := eng.runtime.TodoList()
	if open != 2 || strings.Contains(list, "stale") {
		t.Fatalf("restored list (%d open):\n%s", open, list)
	}
	eng.LoadMessages(nil)
	if list, _ := eng.runtime.TodoList(); list != "" {
		t.Fatalf("list kept after loading an empty history:\n%s", list)
	}
}

// Sub-agents get the project's instruction files and environment.
func TestSubAgentContextCarriesProjectInstructions(t *testing.T) {
	isolatedHome(t)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "AGENTS.md"), []byte("Always run make lint before finishing.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	wd, _ := os.Getwd()
	t.Cleanup(func() { _ = os.Chdir(wd) })
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	eng := newTestEngine(&mockProvider{})
	eng.memStore = memory.NewStoreForDirs(t.TempDir())
	eng.SetProjectContext(&ctxt.ProjectContext{Cwd: dir, Platform: "test", Shell: "sh"})
	eng.SetCustomInstructions("Answer tersely.")
	ctx := eng.subAgentContext()
	for _, want := range []string{"# Project context", "Working directory:", "Always run make lint", "Answer tersely."} {
		if !strings.Contains(ctx, want) {
			t.Errorf("sub-agent context lacks %q:\n%s", want, ctx)
		}
	}
}

// A fast model that does not exist (404: a mistyped model_fast, or a model
// the provider's region does not serve) hands the turn to the main model
// instead of failing it. The user never chose that model: the router did.
func TestMissingFastModelFallsBackToTheMainModel(t *testing.T) {
	prov := &seqProvider{reply: func(_ context.Context, n int, req api.ChatRequest) (*api.ChatResponse, error) {
		if req.Model == "test-fast" {
			return nil, &api.StatusError{Status: 404, Msg: `{"type":"error","error":{"type":"not_found_error","message":"The model 'test-fast' does not exist"}}`}
		}
		return &api.ChatResponse{Content: "done on the main model"}, nil
	}}
	eng := newPatternEngine(t, prov, func(c *Config) { c.ModelFast = "test-fast" })
	var lines []string
	eng.SetOutput(LineSink(func(s string) { lines = append(lines, s) }))
	reply, err := eng.RunMessageWithStream(context.Background(), api.Message{Role: "user", Content: "hi"}, nil, nil)
	if err != nil || reply != "done on the main model" {
		t.Fatalf("reply %q, err %v", reply, err)
	}
	reqs := prov.requests()
	if len(reqs) != 2 || reqs[0].Model != "test-fast" || reqs[1].Model != "test-model" {
		t.Fatalf("requests %+v, want the fast model first, then the main model", reqs)
	}
	if !strings.Contains(strings.Join(lines, ""), "改用 test-model") {
		t.Fatalf("lines %q, want the fallback notice", lines)
	}
}

// A 404 on the main model is not papered over with the fast model: the
// base_url or the configured model is wrong and the user has to fix it.
func TestMissingMainModelDoesNotFallBackToTheFastModel(t *testing.T) {
	prov := &seqProvider{reply: func(_ context.Context, n int, req api.ChatRequest) (*api.ChatResponse, error) {
		return nil, &api.StatusError{Status: 404, Msg: "The model 'test-model' does not exist"}
	}}
	// The fast model is set after construction, as the overload tests do,
	// so the router sends the message to the main model.
	eng := newPatternEngine(t, prov, nil)
	eng.config.ModelFast = "test-fast"
	if _, err := eng.RunMessageWithStream(context.Background(), api.Message{Role: "user", Content: "do it"}, nil, nil); err == nil {
		t.Fatal("a missing main model must fail the turn")
	}
	reqs := prov.requests()
	if len(reqs) != 1 || reqs[0].Model != "test-model" {
		t.Fatalf("requests %+v, want exactly one, to the main model", reqs)
	}
}
