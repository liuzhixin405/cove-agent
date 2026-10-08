package delegate

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/liuzhixin405/cove-agent/internal/api"
	"github.com/liuzhixin405/cove-agent/internal/tool"
)

func TestRegressionEvidenceRequiresRealRedGreenTests(t *testing.T) {
	fail := `{"Action":"fail","Package":"example","Test":"TestRegression"}`
	pass := `{"Action":"pass","Package":"example","Test":"TestRegression"}`
	for _, test := range []struct {
		name          string
		before, after RegressionRun
		status        string
	}{
		{"red green", RegressionRun{ExitCode: 1, Output: fail}, RegressionRun{Output: pass}, "passed"},
		{"both green", RegressionRun{Output: pass}, RegressionRun{Output: pass}, "failed"},
		{"build failure", RegressionRun{ExitCode: 1, Output: `{"Action":"fail","Package":"example"}`}, RegressionRun{Output: pass}, "unverified"},
		{"different test", RegressionRun{ExitCode: 1, Output: fail}, RegressionRun{Output: `{"Action":"pass","Package":"example","Test":"TestOther"}`}, "unverified"},
		{"skipped", RegressionRun{ExitCode: 1, Output: fail}, RegressionRun{Output: `{"Action":"skip","Package":"example","Test":"TestRegression"}`}, "unverified"},
		{"cancelled", RegressionRun{ExitCode: 1, Output: fail, Error: "context cancelled"}, RegressionRun{Output: pass}, "unverified"},
		{"fixed fails", RegressionRun{ExitCode: 1, Output: fail}, RegressionRun{ExitCode: 1, Output: fail}, "failed"},
		{"invalid output", RegressionRun{ExitCode: 1, Output: "not json"}, RegressionRun{Output: pass}, "unverified"},
		{"Go timeout", RegressionRun{ExitCode: 1, Output: `{"Action":"output","Package":"example","Output":"panic: test timed out after 1m0s"}` + "\n" + fail}, RegressionRun{Output: pass}, "unverified"},
	} {
		t.Run(test.name, func(t *testing.T) {
			got := CompareRegression(test.before, test.after)
			if got.Status != test.status {
				t.Fatalf("evidence = %+v, want %s", got, test.status)
			}
			if got.Status == "passed" && (len(got.Tests) != 1 || got.Tests[0] != "example/TestRegression") {
				t.Fatalf("wrong passing evidence: %+v", got)
			}
		})
	}
}

// fakeProvider replays one scripted response per Chat call.
type fakeProvider struct {
	responses []*api.ChatResponse
	seen      int
	lastReq   api.ChatRequest
	onCall    func()
}

func (p *fakeProvider) Name() string        { return "fake" }
func (p *fakeProvider) DisplayName() string { return "Fake" }
func (p *fakeProvider) Validate() error     { return nil }

func (p *fakeProvider) Chat(ctx context.Context, req api.ChatRequest) (*api.ChatResponse, error) {
	p.lastReq = req
	if p.seen >= len(p.responses) {
		p.seen++
		return &api.ChatResponse{Content: "done"}, nil
	}
	r := p.responses[p.seen]
	p.seen++
	if p.onCall != nil {
		p.onCall()
	}
	return r, nil
}

func (p *fakeProvider) ChatStream(ctx context.Context, req api.ChatRequest, h api.StreamHandler) (*api.ChatResponse, error) {
	return p.Chat(ctx, req)
}

// probeTool records how it was invoked, so the test can see the tool.Context a
// sub-agent builds for its tool calls.
type probeTool struct {
	readOnly bool
	calls    int
	lastCtx  tool.Context
	lastIn   tool.Input
}

func (t *probeTool) Def() tool.Def {
	return tool.Def{
		Name:              "write",
		Description:       "probe",
		InputSchema:       json.RawMessage(`{"type":"object","properties":{"filePath":{"type":"string"}}}`),
		IsReadOnly:        t.readOnly,
		IsConcurrencySafe: false,
		UserFacingName:    "Write",
	}
}

func (t *probeTool) Validate(input tool.Input) string { return "" }

func (t *probeTool) CheckPermissions(input tool.Input, tctx tool.Context) tool.PermissionDecision {
	return tool.PermissionDecision{Decision: tool.Ask, Reason: "writes a file"}
}

func (t *probeTool) Call(ctx context.Context, input tool.Input, tctx tool.Context) (tool.Result, error) {
	t.calls++
	t.lastCtx = tctx
	t.lastIn = input
	return tool.Result{Data: "written"}, nil
}

func writeCall() *api.ChatResponse {
	return &api.ChatResponse{ToolCalls: []api.ToolCall{
		{ID: "tc1", Name: "write", Input: map[string]any{"filePath": "a.go"}},
	}}
}

func TestVerifierCannotPassOnModelClaimOrDeniedExecution(t *testing.T) {
	for _, output := range []string{"verified, all tests passed", `{"status":"passed"}`} {
		provider := &fakeProvider{responses: []*api.ChatResponse{{Content: output}}}
		sa := NewSubAgent(Config{Provider: provider, Model: "m", RequireRegression: true})
		result := sa.Run(context.Background(), "verify", "independent verifier")
		if result.Success || result.ExitReason != ExitUnverified {
			t.Fatalf("model claim accepted: %+v", result)
		}
	}
	provider := &fakeProvider{responses: []*api.ChatResponse{{ToolCalls: []api.ToolCall{{ID: "verify", Name: "regression_verify"}}}, {Content: "verified"}}}
	sa := NewSubAgent(Config{Provider: provider, Model: "m", RequireRegression: true})
	result := sa.Run(context.Background(), "verify", "independent verifier")
	if result.Success || result.ExitReason != ExitUnverified {
		t.Fatalf("missing/denied tool accepted: %+v", result)
	}
}

func TestVerifierFiltersWritableToolsEvenWithoutReadOnlyOption(t *testing.T) {
	write := &probeTool{}
	provider := &fakeProvider{responses: []*api.ChatResponse{{Content: "verified"}}}
	delegator := NewDelegator(provider, "m", []tool.Tool{write})
	result := delegator.DelegateWith(context.Background(), "verify", "verify", "independent verifier", Options{RequireRegression: true})
	if result.Success || len(provider.lastReq.Tools) != 0 || write.calls != 0 {
		t.Fatalf("verifier tools=%+v result=%+v calls=%d", provider.lastReq.Tools, result, write.calls)
	}
}

// A sub-agent used to call every tool directly, with PermissionMode hardcoded
// to "auto" and no permission check at all. A user who approved a plan in
// default mode then had sub-agents writing files with no further gate.
func TestSubAgentAsksTheGateBeforeRunningATool(t *testing.T) {
	probe := &probeTool{}
	prov := &fakeProvider{responses: []*api.ChatResponse{writeCall(), {Content: "ok"}}}

	var asked []string
	sa := NewSubAgent(Config{
		Provider: prov,
		Model:    "m",
		Tools:    []tool.Tool{probe},
		Authorize: func(ctx context.Context, tc api.ToolCall) error {
			asked = append(asked, tc.Name)
			return errors.New("permission denied for write: user rejected")
		},
	})

	res := sa.Run(context.Background(), "write a.go", "sys")
	if res == nil {
		t.Fatal("Run returned nil")
	}
	if len(asked) != 1 || asked[0] != "write" {
		t.Fatalf("the gate saw %v, want exactly one call for \"write\"", asked)
	}
	if probe.calls != 0 {
		t.Errorf("a tool the gate denied was executed anyway (%d calls)", probe.calls)
	}

	// The model has to be told, or it reports success for a write that never
	// happened.
	var told bool
	for _, m := range prov.lastReq.Messages {
		if m.Role == "tool" && strings.Contains(m.Content, "permission denied") {
			told = true
		}
	}
	if !told {
		t.Errorf("the denial never reached the model; messages were %+v", prov.lastReq.Messages)
	}
}

func TestSubAgentRunsAToolTheGateAllows(t *testing.T) {
	probe := &probeTool{}
	prov := &fakeProvider{responses: []*api.ChatResponse{writeCall(), {Content: "ok"}}}

	sa := NewSubAgent(Config{
		Provider:  prov,
		Model:     "m",
		Tools:     []tool.Tool{probe},
		Authorize: func(ctx context.Context, tc api.ToolCall) error { return nil },
	})

	res := sa.Run(context.Background(), "write a.go", "sys")
	if res == nil || !res.Success {
		t.Fatalf("Run = %+v, want success", res)
	}
	if probe.calls != 1 {
		t.Fatalf("allowed tool ran %d times, want 1", probe.calls)
	}
	if got := probe.lastIn["filePath"]; got != "a.go" {
		t.Errorf("tool input was %v, want filePath=a.go", probe.lastIn)
	}
}

// Tools resolve relative paths against Context.Cwd. The sub-agent left it empty,
// so a relative write from a sub-agent resolved against the process working
// directory instead of the project the user is in.
func TestSubAgentToolCallsCarryCwdAndRealPermissionMode(t *testing.T) {
	probe := &probeTool{}
	prov := &fakeProvider{responses: []*api.ChatResponse{writeCall(), {Content: "ok"}}}

	sa := NewSubAgent(Config{
		Provider:       prov,
		Model:          "m",
		Tools:          []tool.Tool{probe},
		Cwd:            "/work/project",
		PermissionMode: "default",
		Authorize:      func(ctx context.Context, tc api.ToolCall) error { return nil },
	})

	sa.Run(context.Background(), "write a.go", "sys")

	if probe.lastCtx.Cwd != "/work/project" {
		t.Errorf("tool saw Cwd %q, want /work/project", probe.lastCtx.Cwd)
	}
	if probe.lastCtx.PermissionMode != "default" {
		t.Errorf("tool saw PermissionMode %q, want the session's real mode", probe.lastCtx.PermissionMode)
	}
}

// A gate that was never wired must not leave the door open. Read-only tools
// stay usable so a sub-agent is still worth spawning; anything that can mutate
// the workspace is refused.
func TestSubAgentWithoutAGateOnlyRunsReadOnlyTools(t *testing.T) {
	readOnly := &probeTool{readOnly: true}
	write := &probeTool{readOnly: false}
	// Both are named "write" in Def(); give the read-only one its own name so
	// the registry keeps both distinct.
	readOnlyTool := &namedProbe{probeTool: readOnly, name: "read"}

	prov := &fakeProvider{responses: []*api.ChatResponse{
		{ToolCalls: []api.ToolCall{
			{ID: "tc1", Name: "read", Input: map[string]any{"filePath": "a.go"}},
			{ID: "tc2", Name: "write", Input: map[string]any{"filePath": "a.go"}},
		}},
		{Content: "ok"},
	}}

	sa := NewSubAgent(Config{Provider: prov, Model: "m", Tools: []tool.Tool{readOnlyTool, write}})
	sa.Run(context.Background(), "do it", "sys")

	if readOnly.calls != 1 {
		t.Errorf("read-only tool ran %d times, want 1", readOnly.calls)
	}
	if write.calls != 0 {
		t.Errorf("a mutating tool ran with no gate configured (%d calls)", write.calls)
	}
}

// namedProbe lets a probeTool register under a different name.
type namedProbe struct {
	*probeTool
	name string
}

func (n *namedProbe) Def() tool.Def {
	d := n.probeTool.Def()
	d.Name = n.name
	return d
}
