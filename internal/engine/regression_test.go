package engine

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/liuzhixin405/cove-agent/internal/api"
	"github.com/liuzhixin405/cove-agent/internal/checkpoint"
	"github.com/liuzhixin405/cove-agent/internal/delegate"
	"github.com/liuzhixin405/cove-agent/internal/permission"
	"github.com/liuzhixin405/cove-agent/internal/tool"
)

func TestRegressionToolRunsRealOverlayWithoutRestoringWorkspace(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git unavailable")
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("GOTOOLCHAIN", "local")
	dir := t.TempDir()
	write := func(name, content string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
	}
	write("go.mod", "module example\n\ngo 1.25\n")
	write("code.go", "package example\nfunc Value() int { return 1 }\n")
	manager, err := checkpoint.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	baseline, err := manager.Create("before fix")
	if err != nil {
		t.Fatal(err)
	}
	defer manager.WaitMaintenance()
	fixed := "package example\nfunc Value() int { return 2 }\n"
	write("code.go", fixed)
	write("code_test.go", "package example\nimport \"testing\"\nfunc TestRegression(t *testing.T) { if Value() != 2 { t.Fatal(\"original bug\") } }\n")
	input := tool.Input{"baseline": baseline, "files": []string{"code.go"}, "package": ".", "run": "^TestRegression$"}
	evidence, err := runRegression(context.Background(), input, dir)
	if err != nil || evidence.Status != "passed" || evidence.Before.ExitCode != 1 || evidence.After.ExitCode != 0 || len(evidence.Tests) != 1 || evidence.Tests[0] != "example/TestRegression" {
		t.Fatalf("evidence = %+v, %v", evidence, err)
	}
	data, _ := os.ReadFile(filepath.Join(dir, "code.go"))
	if string(data) != fixed {
		t.Fatal("workspace source was restored")
	}
	input["run"] = "^TestDoesNotExist$"
	evidence, err = runRegression(context.Background(), input, dir)
	if err != nil || evidence.Status == "passed" {
		t.Fatalf("no tests counted as proof: %+v %v", evidence, err)
	}
	if !strings.Contains(evidence.Reason, "pass") {
		t.Fatalf("missing no-discrimination reason: %+v", evidence)
	}
	write("code_test.go", "package example\nimport (\"testing\"; \"os\")\nfunc TestMutation(t *testing.T) { if err := os.WriteFile(\"test-side-effect.txt\", []byte(\"changed\"), 0600); err != nil { t.Fatal(err) }; if Value() != 2 { t.Fatal(\"original bug\") } }\n")
	input["run"] = "^TestMutation$"
	evidence, err = runRegression(context.Background(), input, dir)
	if err != nil || evidence.Status != "unverified" || !strings.Contains(evidence.Reason, "workspace changed") {
		t.Fatalf("workspace drift accepted: %+v %v", evidence, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := runRegression(ctx, input, dir); err == nil {
		t.Fatal("cancelled execution accepted")
	}
	verifier := &regressionTool{}
	input["files"] = []string{"code_test.go"}
	if verifier.Validate(input) == "" {
		t.Fatal("test file accepted as old implementation")
	}
	input["files"] = []string{"code.go"}
	input["package"] = "./../outside"
	if verifier.Validate(input) == "" {
		t.Fatal("outside package accepted")
	}
	if verifier.CheckPermissions(input, tool.Context{}).Decision != tool.Ask {
		t.Fatal("test execution bypasses permission")
	}
}

func TestVerifierAgentUsesRealPipelineAndPersistsRegressionEvidence(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("COVE_CONFIG_DIR", t.TempDir())
	t.Setenv("GOTOOLCHAIN", "local")
	dir := t.TempDir()
	t.Chdir(dir)
	for name, body := range map[string]string{"go.mod": "module example\n\ngo 1.25\n", "code.go": "package example\nfunc Value() int { return 1 }\n"} {
		if err := os.WriteFile(name, []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}
	manager, err := checkpoint.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	baseline, err := manager.Create("before fix")
	if err != nil {
		t.Fatal(err)
	}
	defer manager.WaitMaintenance()
	if err := os.WriteFile("code.go", []byte("package example\nfunc Value() int { return 2 }\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile("code_test.go", []byte("package example\nimport \"testing\"\nfunc TestRegression(t *testing.T) { if Value() != 2 { t.Fatal(\"original bug\") } }\n"), 0600); err != nil {
		t.Fatal(err)
	}
	provider := &seqProvider{reply: func(ctx context.Context, index int, req api.ChatRequest) (*api.ChatResponse, error) {
		switch index {
		case 0:
			found := false
			for _, definition := range req.Tools {
				if definition.Name == "regression_verify" {
					found = true
				}
			}
			if !found {
				t.Error("main model did not receive regression_verify")
			}
			return toolCallResp("agent", "agent", map[string]any{"type": "verify", "prompt": "verify the Value regression against " + baseline}), nil
		case 1:
			for _, definition := range req.Tools {
				if definition.Name != "read" && definition.Name != "regression_verify" {
					t.Errorf("unsafe verifier tool: %s", definition.Name)
				}
			}
			if len(req.Tools) != 2 {
				t.Errorf("tools=%+v", req.Tools)
			}
			return toolCallResp("proof", "regression_verify", map[string]any{"baseline": baseline, "files": []any{"code.go"}, "package": ".", "run": "^TestRegression$"}), nil
		default:
			return &api.ChatResponse{Content: "verified selected regression"}, nil
		}
	}}
	eng := newPatternEngine(t, provider, nil, tool.NewAgentTool(), &mockTool{name: "read", readOnly: true}, &mockTool{name: "write"}, &mockTool{name: "bash"})
	eng.WirePlanExecutor()
	if _, err := run(t, eng, "independently verify the fix"); err != nil {
		t.Fatal(err)
	}
	var agentResult string
	for _, message := range eng.Messages() {
		if message.Name == "agent" {
			agentResult = message.Content
		}
	}
	if !strings.Contains(agentResult, "Success: true") {
		t.Fatalf("real agent failed: %s; verifier follow-up: %+v", agentResult, provider.requests()[2].Messages)
	}
	eng.acceptance = nil
	report, err := eng.LastAcceptance()
	if err != nil || report == nil || len(report.Regression) != 1 || report.Regression[0].Status != "passed" || report.Regression[0].Baseline != baseline {
		t.Fatalf("persisted evidence=%+v error=%v", report, err)
	}
	eng.acceptance = report
	clone, _ := eng.LastAcceptance()
	clone.Regression[0].Tests[0] = "modified"
	if report.Regression[0].Tests[0] != "example/TestRegression" {
		t.Fatal("report clone shares tests")
	}
	eng.invalidateAcceptance("")
	if eng.acceptance.Regression[0].Status != "unverified" {
		t.Fatal("later mutation kept passing evidence")
	}
}

func TestVerifierAgentRejectsUnsupportedCompletionThroughActualAdapter(t *testing.T) {
	provider := &seqProvider{reply: func(ctx context.Context, index int, req api.ChatRequest) (*api.ChatResponse, error) {
		return &api.ChatResponse{Content: "all regression tests passed"}, nil
	}}
	eng := newPatternEngine(t, provider, nil, tool.NewAgentTool())
	eng.WirePlanExecutor()
	eng.beginAcceptance(api.Message{Role: "user", Content: "verify without evidence"})
	result, err := eng.runtime.AgentRunner.(api.AgentRunner).Run(context.Background(), "verify", "verify this fix")
	if err != nil || result.Success || result.ExitReason != delegate.ExitUnverified {
		t.Fatalf("unsupported completion=%+v %v", result, err)
	}
	if len(eng.acceptance.Regression) != 1 || eng.acceptance.Regression[0].Status != "unverified" {
		t.Fatalf("unsupported verification missing from report: %+v", eng.acceptance)
	}
}

func TestRegressionToolPermissionDenialDoesNotExecuteTests(t *testing.T) {
	eng := newPatternEngine(t, &seqProvider{}, nil)
	eng.WirePlanExecutor()
	eng.perm.SetMode(permission.Default)
	eng.runtime.AskUser = func(prompt string) string { return "n" }
	output, failed := eng.executeTool(context.Background(), api.ToolCall{ID: "denied", Name: "regression_verify", Input: map[string]any{"baseline": "0123456789abcdef", "files": []any{"code.go"}, "package": ".", "run": "^TestRegression$"}})
	if !failed || strings.Contains(output, "regression baseline") || !strings.Contains(strings.ToLower(output), "denied") {
		t.Fatalf("permission gate did not stop execution: %s failed=%v", output, failed)
	}
}
