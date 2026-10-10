package plan

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/liuzhixin405/cove-agent/internal/api"
	"github.com/liuzhixin405/cove-agent/internal/delegate"
	"github.com/liuzhixin405/cove-agent/internal/tool"
)

// scriptedProvider is an api.Provider that answers every Chat with no tool
// calls (which ends a sub-agent immediately) and records the prompt it saw.
//
// Driving the real delegate.Delegator through this is deliberate: it exercises
// the actual Execute → runTask → Delegate → SubAgent.Run path rather than a
// hand-rolled stand-in, so the dependency and retry behaviour under test is
// the behaviour that ships.
type scriptedProvider struct {
	mu sync.Mutex
	// reply decides what to answer for a given user prompt.
	reply func(prompt string) (content string, err error)
	// prompts records every user prompt received, in order.
	prompts []string
	// inFlight/maxInFlight track concurrency so parallel execution can be
	// asserted without sleeping.
	inFlight, maxInFlight int
}

func (p *scriptedProvider) Name() string        { return "scripted" }
func (p *scriptedProvider) DisplayName() string { return "scripted" }
func (p *scriptedProvider) Validate() error     { return nil }

func (p *scriptedProvider) Chat(ctx context.Context, req api.ChatRequest) (*api.ChatResponse, error) {
	prompt := ""
	for _, m := range req.Messages {
		if m.Role == "user" {
			prompt = m.Content
		}
	}

	p.mu.Lock()
	p.prompts = append(p.prompts, prompt)
	p.inFlight++
	if p.inFlight > p.maxInFlight {
		p.maxInFlight = p.inFlight
	}
	p.mu.Unlock()
	defer func() {
		p.mu.Lock()
		p.inFlight--
		p.mu.Unlock()
	}()

	if err := ctx.Err(); err != nil {
		return nil, err
	}
	content, err := p.reply(prompt)
	if err != nil {
		return nil, err
	}
	return &api.ChatResponse{Content: content, StopReason: "end_turn"}, nil
}

func (p *scriptedProvider) ChatStream(ctx context.Context, req api.ChatRequest, h api.StreamHandler) (*api.ChatResponse, error) {
	return p.Chat(ctx, req)
}

func (p *scriptedProvider) seenPrompts() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.prompts...)
}

func (p *scriptedProvider) peakConcurrency() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.maxInFlight
}

// newExecutor wires a PlanExecutor onto a scripted provider and a runtime
// pre-populated with the plan's tasks.
func newExecutor(t *testing.T, p *scriptedProvider, taskIDs ...string) (*PlanExecutor, *tool.Runtime) {
	t.Helper()
	rt := &tool.Runtime{Tasks: map[string]*tool.TaskRecord{}}
	for _, id := range taskIDs {
		rt.Tasks[id] = &tool.TaskRecord{ID: id, Status: "pending"}
	}
	d := delegate.NewDelegator(p, "test-model", nil)
	pe := NewPlanExecutor(d, rt)
	pe.SetMaxRetries(0) // default is 1; tests opt in explicitly
	return pe, rt
}

func statusByID(res *ExecutionResult) map[string]string {
	out := map[string]string{}
	for _, t := range res.Tasks {
		out[t.ID] = t.Status
	}
	return out
}

// TestExecuteSerialRunsInDependencyOrder asserts the core contract: a task
// never starts before the task it depends on has finished.
func TestExecuteSerialRunsInDependencyOrder(t *testing.T) {
	p := &scriptedProvider{reply: func(prompt string) (string, error) {
		return "ok: " + prompt, nil
	}}
	pe, _ := newExecutor(t, p, "a", "b", "c")

	plan := &Plan{
		ID: "p",
		Tasks: []*Task{
			{ID: "a", Title: "A", Description: "A", Status: "pending"},
			{ID: "b", Title: "B", Description: "B", Status: "pending", DependsOn: []string{"a"}},
			{ID: "c", Title: "C", Description: "C", Status: "pending", DependsOn: []string{"b"}},
		},
	}

	res := pe.Execute(context.Background(), plan)
	if !res.Success {
		t.Fatalf("Execute failed: %+v", statusByID(res))
	}

	order := p.seenPrompts()
	if len(order) != 3 {
		t.Fatalf("got %d agent invocations, want 3: %v", len(order), order)
	}
	// Prompts start with the task Description.
	for i, want := range []string{"A", "B", "C"} {
		if !strings.HasPrefix(order[i], want) {
			t.Fatalf("invocation %d = %q, want it to start with %q (order: %v)", i, order[i], want, order)
		}
	}
	if got := p.peakConcurrency(); got != 1 {
		t.Fatalf("peak concurrency = %d in serial mode, want 1", got)
	}
}

// TestExecuteParallelNeverOverlapsDependencies is the regression test for the
// topologicalSort defect: it deleted from the `remaining` map while ranging
// over it, so level assignment depended on Go's randomized map iteration order
// and a task could land in the SAME level as its own dependency. With
// Plan.Parallel that means the two ran concurrently and the dependency graph
// was effectively ignored.
func TestExecuteParallelNeverOverlapsDependencies(t *testing.T) {
	// Run repeatedly: the old bug was order-dependent and only showed up on
	// some runs.
	for iter := 0; iter < 20; iter++ {
		var mu sync.Mutex
		running := map[string]bool{}
		var violations []string

		p := &scriptedProvider{}
		p.reply = func(prompt string) (string, error) {
			id := strings.Fields(prompt)[0]

			mu.Lock()
			// Record a violation if a task runs while its dependency is running.
			deps := map[string][]string{
				"b": {"a"}, "c": {"a"}, "d": {"b", "c"}, "e": {"d"},
			}
			for _, dep := range deps[id] {
				if running[dep] {
					violations = append(violations,
						fmt.Sprintf("%s ran while its dependency %s was still running", id, dep))
				}
			}
			running[id] = true
			mu.Unlock()

			mu.Lock()
			running[id] = false
			mu.Unlock()
			return "done", nil
		}

		pe, _ := newExecutor(t, p, "a", "b", "c", "d", "e")
		plan := &Plan{
			ID:       "p",
			Parallel: true,
			Tasks: []*Task{
				{ID: "a", Description: "a task", Status: "pending"},
				{ID: "b", Description: "b task", Status: "pending", DependsOn: []string{"a"}},
				{ID: "c", Description: "c task", Status: "pending", DependsOn: []string{"a"}},
				{ID: "d", Description: "d task", Status: "pending", DependsOn: []string{"b", "c"}},
				{ID: "e", Description: "e task", Status: "pending", DependsOn: []string{"d"}},
			},
		}

		res := pe.Execute(context.Background(), plan)
		if !res.Success {
			t.Fatalf("iter %d: Execute failed: %+v", iter, statusByID(res))
		}

		mu.Lock()
		v := append([]string(nil), violations...)
		mu.Unlock()
		if len(v) > 0 {
			t.Fatalf("iter %d: dependency order violated: %v", iter, v)
		}

		// Every task must have run exactly once.
		if got := len(p.seenPrompts()); got != 5 {
			t.Fatalf("iter %d: %d invocations, want 5", iter, got)
		}
	}
}

// TestExecuteParallelActuallyParallelizes guards the other direction: sibling
// tasks at the same level must be allowed to overlap, otherwise Parallel is
// pointless. Synchronization is by channel, never by sleeping.
func TestExecuteParallelActuallyParallelizes(t *testing.T) {
	const siblings = 3
	entered := make(chan struct{}, siblings)
	release := make(chan struct{})

	p := &scriptedProvider{}
	p.reply = func(prompt string) (string, error) {
		entered <- struct{}{}
		<-release // hold until every sibling has arrived
		return "done", nil
	}

	pe, _ := newExecutor(t, p, "a", "b", "c")
	plan := &Plan{
		ID:       "p",
		Parallel: true,
		Tasks: []*Task{
			{ID: "a", Description: "a", Status: "pending"},
			{ID: "b", Description: "b", Status: "pending"},
			{ID: "c", Description: "c", Status: "pending"},
		},
	}

	done := make(chan *ExecutionResult, 1)
	go func() { done <- pe.Execute(context.Background(), plan) }()

	// If the level were executed serially this would block forever.
	for i := 0; i < siblings; i++ {
		<-entered
	}
	close(release)

	res := <-done
	if !res.Success {
		t.Fatalf("Execute failed: %+v", statusByID(res))
	}
	if got := p.peakConcurrency(); got < siblings {
		t.Fatalf("peak concurrency = %d, want at least %d", got, siblings)
	}
}

// TestExecuteSkipsDownstreamOfFailure covers H-12: a task whose dependency
// failed used to be marked "skipped" and then executed anyway, because runTask
// reset Status to "running" as its first statement.
func TestExecuteSkipsDownstreamOfFailure(t *testing.T) {
	p := &scriptedProvider{}
	p.reply = func(prompt string) (string, error) {
		if strings.HasPrefix(prompt, "a") {
			return "", fmt.Errorf("boom")
		}
		return "ok", nil
	}

	pe, rt := newExecutor(t, p, "a", "b", "c", "independent")
	plan := &Plan{
		ID: "p",
		Tasks: []*Task{
			{ID: "a", Description: "a fails", Status: "pending"},
			{ID: "b", Description: "b needs a", Status: "pending", DependsOn: []string{"a"}},
			{ID: "c", Description: "c needs b", Status: "pending", DependsOn: []string{"b"}},
			{ID: "independent", Description: "independent runs anyway", Status: "pending"},
		},
	}

	res := pe.Execute(context.Background(), plan)
	if res.Success {
		t.Fatal("Execute reported success even though a task failed")
	}

	got := statusByID(res)
	if got["a"] != "failed" {
		t.Errorf("a = %q, want failed", got["a"])
	}
	// b is skipped directly; c must be skipped TRANSITIVELY (its dep was
	// skipped, not failed).
	if got["b"] != "skipped" {
		t.Errorf("b = %q, want skipped", got["b"])
	}
	if got["c"] != "skipped" {
		t.Errorf("c = %q, want skipped (transitively)", got["c"])
	}
	// An unrelated task must still run.
	if got["independent"] != "done" {
		t.Errorf("independent = %q, want done", got["independent"])
	}

	// The skipped tasks must never have reached the agent.
	for _, prompt := range p.seenPrompts() {
		if strings.HasPrefix(prompt, "b needs") || strings.HasPrefix(prompt, "c needs") {
			t.Fatalf("a skipped task was executed anyway: %q", prompt)
		}
	}

	// The skip must be mirrored into the shared runtime, which is what the UI
	// and the model observe.
	rt.Lock()
	defer rt.Unlock()
	for _, id := range []string{"b", "c"} {
		if rt.Tasks[id].Status != "skipped" {
			t.Errorf("runtime task %s = %q, want skipped", id, rt.Tasks[id].Status)
		}
	}
}

// TestExecutePassesDependencyOutputDownstream asserts the point of having
// dependencies at all: the downstream task receives the upstream output.
func TestExecutePassesDependencyOutputDownstream(t *testing.T) {
	p := &scriptedProvider{}
	p.reply = func(prompt string) (string, error) {
		if strings.HasPrefix(prompt, "produce") {
			return "THE-ARTIFACT", nil
		}
		return "consumed", nil
	}

	pe, _ := newExecutor(t, p, "up", "down")
	plan := &Plan{
		ID: "p",
		Tasks: []*Task{
			{ID: "up", Description: "produce it", Status: "pending"},
			{ID: "down", Description: "consume it", Status: "pending", DependsOn: []string{"up"}},
		},
	}

	res := pe.Execute(context.Background(), plan)
	if !res.Success {
		t.Fatalf("Execute failed: %+v", statusByID(res))
	}

	var downstream string
	for _, pr := range p.seenPrompts() {
		if strings.HasPrefix(pr, "consume it") {
			downstream = pr
		}
	}
	if downstream == "" {
		t.Fatal("the downstream task never ran")
	}
	if !strings.Contains(downstream, "THE-ARTIFACT") {
		t.Fatalf("downstream prompt did not carry the dependency output:\n%s", downstream)
	}
	if !strings.Contains(downstream, "up") {
		t.Fatalf("downstream prompt did not attribute the output to its source:\n%s", downstream)
	}
}

// TestExecuteRetriesWithFailureContext covers the supervisor retry: the second
// attempt must be told why the first one failed, otherwise the retry is just a
// blind repeat.
func TestExecuteRetriesWithFailureContext(t *testing.T) {
	var calls int
	p := &scriptedProvider{}
	p.reply = func(prompt string) (string, error) {
		calls++
		if calls == 1 {
			return "", fmt.Errorf("disk on fire")
		}
		return "ok", nil
	}

	pe, _ := newExecutor(t, p, "a")
	pe.SetMaxRetries(1)
	plan := &Plan{ID: "p", Tasks: []*Task{{ID: "a", Description: "work", Status: "pending"}}}

	res := pe.Execute(context.Background(), plan)
	if !res.Success {
		t.Fatalf("Execute did not recover on retry: %+v", statusByID(res))
	}
	if calls != 2 {
		t.Fatalf("provider called %d times, want 2 (one failure + one retry)", calls)
	}

	prompts := p.seenPrompts()
	if len(prompts) < 2 {
		t.Fatalf("got %d prompts, want 2", len(prompts))
	}
	if !strings.Contains(prompts[1], "disk on fire") {
		t.Fatalf("the retry prompt did not include the prior failure reason:\n%s", prompts[1])
	}
}

// TestExecuteMaxRetriesZeroDoesNotRetry pins SetMaxRetries(0).
func TestExecuteMaxRetriesZeroDoesNotRetry(t *testing.T) {
	var calls int
	p := &scriptedProvider{}
	p.reply = func(prompt string) (string, error) {
		calls++
		return "", fmt.Errorf("always fails")
	}

	pe, _ := newExecutor(t, p, "a")
	pe.SetMaxRetries(0)
	plan := &Plan{ID: "p", Tasks: []*Task{{ID: "a", Description: "work", Status: "pending"}}}

	res := pe.Execute(context.Background(), plan)
	if res.Success {
		t.Fatal("Execute reported success for an always-failing task")
	}
	if calls != 1 {
		t.Fatalf("provider called %d times with maxRetries=0, want 1", calls)
	}
	if got := statusByID(res)["a"]; got != "failed" {
		t.Fatalf("a = %q, want failed", got)
	}
	if res.Tasks[0].Error == "" {
		t.Fatal("the failure reason was not recorded on the task")
	}
}

// TestExecuteSetMaxRetriesClampsNegative pins the documented clamp.
func TestExecuteSetMaxRetriesClampsNegative(t *testing.T) {
	pe := &PlanExecutor{}
	pe.SetMaxRetries(-5)
	if pe.maxRetries != 0 {
		t.Fatalf("maxRetries = %d after SetMaxRetries(-5), want 0", pe.maxRetries)
	}
}

// TestExecuteCancellationStopsWork asserts a cancelled context ends the run
// instead of grinding through every remaining task.
func TestExecuteCancellationStopsWork(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())

	// Deterministic ordering: the first task parks until the test has actually
	// cancelled, so the downstream tasks are guaranteed to be dispatched with
	// an already-cancelled context. Without the park, the scripted provider
	// returns so fast that the whole plan can finish before cancel() lands.
	started := make(chan struct{})
	cancelled := make(chan struct{})
	var once sync.Once

	p := &scriptedProvider{}
	p.reply = func(prompt string) (string, error) {
		first := false
		once.Do(func() { first = true })
		if first {
			close(started)
			<-cancelled
		}
		return "ok", nil
	}

	pe, _ := newExecutor(t, p, "a", "b", "c")
	pe.SetMaxRetries(3) // would be a lot of retries if cancellation were ignored
	plan := &Plan{
		ID: "p",
		Tasks: []*Task{
			{ID: "a", Description: "a", Status: "pending"},
			{ID: "b", Description: "b", Status: "pending", DependsOn: []string{"a"}},
			{ID: "c", Description: "c", Status: "pending", DependsOn: []string{"b"}},
		},
	}

	done := make(chan *ExecutionResult, 1)
	go func() { done <- pe.Execute(ctx, plan) }()

	<-started
	cancel()
	close(cancelled)

	res := <-done // must return; the assertion is that this does not hang
	if res == nil {
		t.Fatal("Execute returned nil after cancellation")
	}
	if res.Success {
		t.Fatal("Execute reported success for a cancelled run")
	}

	// The downstream tasks must report the cancellation rather than silently
	// appearing done.
	got := statusByID(res)
	for _, id := range []string{"b", "c"} {
		if got[id] == "done" {
			t.Errorf("%s = done after cancellation, want failed/skipped", id)
		}
	}
}

// TestExecuteCycleMarksAllFailed covers the guard for a plan that reaches
// Execute with a cycle (FromRuntime rejects those, but Execute is exported and
// callers can build a Plan directly).
func TestExecuteCycleMarksAllFailed(t *testing.T) {
	p := &scriptedProvider{reply: func(string) (string, error) { return "ok", nil }}
	pe, _ := newExecutor(t, p, "a", "b")

	plan := &Plan{
		ID: "p",
		Tasks: []*Task{
			{ID: "a", Description: "a", Status: "pending", DependsOn: []string{"b"}},
			{ID: "b", Description: "b", Status: "pending", DependsOn: []string{"a"}},
		},
	}

	res := pe.Execute(context.Background(), plan)
	if res.Success {
		t.Fatal("Execute reported success for a cyclic plan")
	}
	for _, task := range res.Tasks {
		if task.Status != "failed" {
			t.Errorf("%s = %q, want failed", task.ID, task.Status)
		}
		if !strings.Contains(task.Error, "circular") {
			t.Errorf("%s error = %q, want it to mention the cycle", task.ID, task.Error)
		}
	}
	if len(p.seenPrompts()) != 0 {
		t.Fatalf("a cyclic plan reached the agent: %v", p.seenPrompts())
	}
}

// TestExecuteDeliversMessagesOnce covers the runtime message inbox: a message
// addressed to a task (or broadcast) must be injected into its prompt exactly
// once, not re-delivered on every subsequent task.
func TestExecuteDeliversMessagesOnce(t *testing.T) {
	p := &scriptedProvider{reply: func(string) (string, error) { return "ok", nil }}
	pe, rt := newExecutor(t, p, "a", "b")

	rt.Lock()
	rt.Messages = []tool.MessageRecord{
		{To: "a", Message: "FOR-A"},
		{To: "all", Message: "FOR-EVERYONE"},
	}
	rt.Unlock()

	plan := &Plan{
		ID: "p",
		Tasks: []*Task{
			{ID: "a", Description: "a", Status: "pending"},
			{ID: "b", Description: "b", Status: "pending", DependsOn: []string{"a"}},
		},
	}
	if res := pe.Execute(context.Background(), plan); !res.Success {
		t.Fatalf("Execute failed: %+v", statusByID(res))
	}

	prompts := p.seenPrompts()
	sort.Strings(prompts)
	joined := strings.Join(prompts, "\n---\n")

	if strings.Count(joined, "FOR-A") != 1 {
		t.Errorf("FOR-A delivered %d times, want exactly 1:\n%s", strings.Count(joined, "FOR-A"), joined)
	}
	if strings.Count(joined, "FOR-EVERYONE") != 1 {
		t.Errorf("FOR-EVERYONE delivered %d times, want exactly 1:\n%s",
			strings.Count(joined, "FOR-EVERYONE"), joined)
	}

	rt.Lock()
	defer rt.Unlock()
	for _, m := range rt.Messages {
		if !m.Delivered {
			t.Errorf("message %q was never marked delivered", m.Message)
		}
	}
}

// TestBlockingDepIgnoresDoneAndRunning pins which statuses actually block.
func TestBlockingDepIgnoresDoneAndRunning(t *testing.T) {
	tasks := map[string]*Task{
		"done":    {ID: "done", Status: "done"},
		"running": {ID: "running", Status: "running"},
		"pending": {ID: "pending", Status: "pending"},
	}
	for _, dep := range []string{"done", "running", "pending"} {
		task := &Task{ID: "x", DependsOn: []string{dep}}
		if got := blockingDep(task, tasks); got != "" {
			t.Errorf("blockingDep with a %q dependency = %q, want no block", dep, got)
		}
	}
}

// Two sibling tasks of a parallel plan writing the same file: the second
// writer is refused and told who owns the file. The sub-agents share one
// working tree, so the later write used to win silently. Serial plans and
// different files are not restricted.
func TestExecuteParallelRefusesSecondWriterOfSameFile(t *testing.T) {
	run := func(t *testing.T, parallel bool, paths map[string]string) map[string]string {
		t.Helper()
		var mu sync.Mutex
		results := map[string]string{}
		p := &scriptedProvider{}
		p.reply = func(prompt string) (string, error) { return "done", nil }
		pe, _ := newExecutor(t, p, "a", "b")
		// The executor stands in for the engine's: it claims the write path
		// for the task in ctx, as engine.go does before executeTool.
		pe.delegator.SetExecutor(func(ctx context.Context, tc api.ToolCall) string {
			return "unused"
		})
		// Drive the claim directly through the contexts runTask builds: the
		// scripted provider never issues tool calls, so the claim table is
		// exercised with the same ctx the executor would receive.
		var ctxs []context.Context
		var ctxMu sync.Mutex
		pe.onTaskContext = func(ctx context.Context) {
			ctxMu.Lock()
			ctxs = append(ctxs, ctx)
			ctxMu.Unlock()
		}
		plan := &Plan{ID: "p", Parallel: parallel, Tasks: []*Task{
			{ID: "a", Description: "a", Status: "pending"},
			{ID: "b", Description: "b", Status: "pending"},
		}}
		if res := pe.Execute(context.Background(), plan); !res.Success {
			t.Fatalf("Execute failed: %+v", statusByID(res))
		}
		ctxMu.Lock()
		defer ctxMu.Unlock()
		for _, ctx := range ctxs {
			id := taskIDFrom(ctx)
			ok, owner := ClaimPath(ctx, paths[id])
			mu.Lock()
			if ok {
				results[id] = "ok"
			} else {
				results[id] = "refused by " + owner
			}
			mu.Unlock()
		}
		return results
	}
	same := map[string]string{"a": "x.go", "b": "x.go"}
	got := run(t, true, same)
	refused := 0
	for id, r := range got {
		if strings.HasPrefix(r, "refused by ") {
			refused++
			other := map[string]string{"a": "b", "b": "a"}[id]
			if r != "refused by "+other {
				t.Fatalf("%s: %s", id, r)
			}
		}
	}
	if refused != 1 {
		t.Fatalf("parallel same file: want exactly one refusal, got %v", got)
	}
	if got := run(t, true, map[string]string{"a": "x.go", "b": "y.go"}); got["a"] != "ok" || got["b"] != "ok" {
		t.Fatalf("parallel different files: %v", got)
	}
	// Serial plans carry no task in ctx (nothing to coordinate), so every
	// claim through their contexts is open.
	for id, r := range run(t, false, same) {
		if r != "ok" {
			t.Fatalf("serial plan must not claim: %q -> %s", id, r)
		}
	}
}
