package engine

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/liuzhixin405/cove-agent/internal/api"
	"github.com/liuzhixin405/cove-agent/internal/permission"
	"github.com/liuzhixin405/cove-agent/internal/session"
	"github.com/liuzhixin405/cove-agent/internal/tool"
)

// sideEffectTool is not read-only and answers Allowed itself, like the tools
// that used to run in plan mode because they never refused.
type sideEffectTool struct {
	name     string
	planSafe bool
	calls    int
}

func (s *sideEffectTool) Def() tool.Def {
	return tool.Def{Name: s.name, InputSchema: []byte(`{"type":"object"}`), PlanSafe: s.planSafe}
}
func (s *sideEffectTool) Validate(tool.Input) string { return "" }
func (s *sideEffectTool) CheckPermissions(tool.Input, tool.Context) tool.PermissionDecision {
	return tool.Allowed("says it is fine")
}
func (s *sideEffectTool) Call(context.Context, tool.Input, tool.Context) (tool.Result, error) {
	s.calls++
	return tool.Result{Data: "changed something"}, nil
}

func planEngine(t *testing.T, tools ...tool.Tool) *Engine {
	t.Helper()
	isolatedHome(t)
	t.Chdir(t.TempDir())
	return newTestEngine(&mockProvider{}, tools...)
}

func run1(t *testing.T, eng *Engine, name string, input map[string]any) (string, bool) {
	t.Helper()
	return eng.executeTool(context.Background(), api.ToolCall{ID: "c", Name: name, Input: input})
}

// A tool that is not read-only runs in plan mode no longer just because its
// CheckPermissions answers Allowed; a PlanSafe one still does.
func TestPlanModeAllowsReadOnlyAndPlanSafeToolsOnly(t *testing.T) {
	writer := &sideEffectTool{name: "sidefx"}
	safe := &sideEffectTool{name: "notes", planSafe: true}
	eng := planEngine(t, writer, safe)
	eng.SetPermissionMode(permission.Plan)

	if _, failed := run1(t, eng, "sidefx", nil); !failed || writer.calls != 0 {
		t.Fatalf("a non-read-only tool ran in plan mode (calls=%d)", writer.calls)
	}
	if out, failed := run1(t, eng, "notes", nil); failed || safe.calls != 1 {
		t.Fatalf("a PlanSafe tool was refused in plan mode: %s", out)
	}
}

// plan_mode told the model "Read-only operations only" and restricted
// nothing: the flag it set was read by nobody.
func TestModelEnteredPlanModeIsEnforced(t *testing.T) {
	eng := planEngine(t, tool.NewPlanModeTool(), tool.NewWriteTool(), tool.NewReadTool())
	eng.PermissionPrompt = func(string, map[string]any, string) bool { return true }
	if _, failed := run1(t, eng, "plan_mode", map[string]any{"reason": "think first"}); failed {
		t.Fatal("entering plan mode failed")
	}
	target, _ := filepath.Abs("a.txt")
	if out, failed := run1(t, eng, "write", map[string]any{"filePath": target, "content": "x"}); !failed {
		t.Fatalf("write ran in the model's plan mode: %s", out)
	}
	if _, err := os.Stat(target); err == nil {
		t.Fatal("the file was written in plan mode")
	}
	if err := os.WriteFile(target, []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	if out, failed := run1(t, eng, "read", map[string]any{"filePath": target}); failed {
		t.Fatalf("read was refused in plan mode: %s", out)
	}
}

// The model may leave the plan mode it entered (the user is asked); plan
// mode the user set is the user's to leave.
func TestExitPlanModeLeavesOnlyTheModelsOwnPlanMode(t *testing.T) {
	eng := planEngine(t, tool.NewPlanModeTool(), tool.NewExitPlanModeTool())
	asked := 0
	eng.PermissionPrompt = func(string, map[string]any, string) bool { asked++; return true }

	run1(t, eng, "plan_mode", nil)
	if out, failed := run1(t, eng, "exit_plan_mode", map[string]any{"summary": "plan ready"}); failed || asked != 1 {
		t.Fatalf("exit_plan_mode from the model's plan mode: failed=%v asked=%d %s", failed, asked, out)
	}
	if eng.effectiveMode() == permission.Plan {
		t.Fatal("still in plan mode after exit_plan_mode")
	}

	eng.SetPermissionMode(permission.Plan)
	out, failed := run1(t, eng, "exit_plan_mode", nil)
	if !failed || !strings.Contains(out, "/mode plan") {
		t.Fatalf("exit_plan_mode left the user's plan mode: failed=%v %s", failed, out)
	}
	if eng.PermissionMode() != permission.Plan {
		t.Fatal("the user's plan mode changed")
	}
}

// In auto mode build lines are pre-approved; in the model's plan mode they
// must not be, since a build writes.
func TestPlanModePreApprovesNoBuildLines(t *testing.T) {
	eng := planEngine(t, tool.NewPlanModeTool(), &mockTool{name: "bash", result: "built"})
	eng.classifier = permission.NewClassifier()
	eng.SetPermissionMode(permission.Auto)
	run1(t, eng, "plan_mode", nil)
	if out, failed := run1(t, eng, "bash", map[string]any{"command": "go build ./..."}); !failed {
		t.Fatalf("a build line ran in plan mode: %s", out)
	}
}

// Plan mode runs a read-only shell line, as the manual says and as
// planModeGate's shell branch intends. The engine pre-approved read-only
// lines in default mode only, so the shell tool saw "plan" in tctx and
// refused every line before the gate was reached: `git status` in plan mode
// was refused. Writes and builds stay refused.
func TestPlanModeRunsReadOnlyShellLines(t *testing.T) {
	eng := planEngine(t, tool.NewBashTool())
	eng.classifier = permission.NewClassifier()
	eng.SetPermissionMode(permission.Plan)
	out, failed := run1(t, eng, "bash", map[string]any{"command": "echo plan-ok"})
	if failed || !strings.Contains(out, "plan-ok") {
		t.Fatalf("read-only line refused in plan mode: failed=%v %s", failed, out)
	}
	for _, cmd := range []string{"go build ./...", "rm -rf build", "echo x > f.txt"} {
		if out, failed := run1(t, eng, "bash", map[string]any{"command": cmd}); !failed {
			t.Fatalf("%q ran in plan mode: %s", cmd, out)
		}
	}
}

func TestReviewWorkflowRequiresExplicitApproval(t *testing.T) {
	writer := &sideEffectTool{name: "sidefx"}
	eng := planEngine(t, writer, tool.NewExitPlanModeTool())
	eng.SetPermissionMode(permission.Bypass)
	if eng.WorkflowStatus().Mode != "direct" {
		t.Fatal("review enabled by default")
	}
	if err := eng.SetWorkflowMode("once"); err != nil {
		t.Fatal(err)
	}
	eng.beginReviewWorkflow(false)
	if !eng.reviewPending() || eng.WorkflowStatus().Mode != "direct" {
		t.Fatal("once did not arm just this task")
	}
	if _, failed := run1(t, eng, "sidefx", nil); !failed || writer.calls != 0 {
		t.Fatal("implementation ran without review")
	}
	if _, failed := run1(t, eng, "exit_plan_mode", map[string]any{"summary": "incomplete"}); !failed {
		t.Fatal("incomplete plan was approved")
	}
	input := map[string]any{"summary": "实现功能", "files": []string{"main.go"}, "decisions": []string{"不引入新依赖"}, "checks": []string{"go test ./..."}}
	asks := 0
	eng.PermissionPrompt = func(string, map[string]any, string) bool { asks++; return false }
	if _, failed := run1(t, eng, "exit_plan_mode", input); !failed || !eng.reviewPending() || asks != 1 {
		t.Fatal("rejected plan unlocked implementation")
	}
	eng.PermissionPrompt = func(string, map[string]any, string) bool { asks++; return true }
	if _, failed := run1(t, eng, "exit_plan_mode", input); failed || eng.reviewPending() || asks != 2 {
		t.Fatal("approved plan did not unlock implementation")
	}
	if _, failed := run1(t, eng, "sidefx", nil); failed || writer.calls != 1 {
		t.Fatal("approved implementation still blocked")
	}
	eng.beginReviewWorkflow(true)
	if eng.reviewPending() {
		t.Fatal("resume forgot approval")
	}
	eng.beginReviewWorkflow(false)
	if eng.reviewPending() {
		t.Fatal("once enabled review for another task")
	}
}

func TestReviewWorkflowRejectsFileDrift(t *testing.T) {
	writer := &sideEffectTool{name: "sidefx"}
	eng := planEngine(t, writer, tool.NewExitPlanModeTool())
	eng.SetPermissionMode(permission.Bypass)
	if err := eng.SetWorkflowMode("review"); err != nil {
		t.Fatal(err)
	}
	if err := eng.beginReviewWorkflow(false); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile("main.go", []byte("old"), 0600); err != nil {
		t.Fatal(err)
	}
	eng.PermissionPrompt = func(string, map[string]any, string) bool {
		if err := os.WriteFile("main.go", []byte("changed-by-user"), 0600); err != nil {
			t.Fatal(err)
		}
		return true
	}
	input := map[string]any{"summary": "修改返回值", "files": []any{"main.go"}, "decisions": []any{"保留接口"}, "checks": []any{"测试通过"}}
	if out, failed := run1(t, eng, "exit_plan_mode", input); !failed || !strings.Contains(out, "发生变化") || !eng.reviewPending() {
		t.Fatalf("file drift did not reject approval: %q", out)
	}
	if _, failed := run1(t, eng, "sidefx", nil); !failed || writer.calls != 0 {
		t.Fatal("implementation ran after stale approval")
	}
}

func TestReviewWorkflowPersistsPendingTask(t *testing.T) {
	eng := planEngine(t)
	if err := eng.SetWorkflowMode("once"); err != nil {
		t.Fatal(err)
	}
	msg := api.Message{Role: "user", Content: "实现棋盘"}
	if err := eng.beginReviewWorkflow(false, msg); err != nil {
		t.Fatal(err)
	}
	id := eng.WorkflowStatus().TaskID
	reloaded := &Engine{session: &session.Record{ID: eng.SessionID()}}
	if err := reloaded.beginReviewWorkflow(false, msg); err != nil {
		t.Fatal(err)
	}
	if state := reloaded.WorkflowStatus(); !state.Pending || state.TaskID != id || state.Mode != "direct" {
		t.Fatalf("pending once task lost on restart: %+v", state)
	}
	reloaded.session.ID = "different-session"
	if state := reloaded.WorkflowStatus(); state.Pending || state.Mode != "direct" {
		t.Fatal("workflow leaked across sessions")
	}
	path, err := workflowPath(eng.SessionID())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("broken"), 0600); err != nil {
		t.Fatal(err)
	}
	broken := &Engine{session: &session.Record{ID: eng.SessionID()}}
	if err := broken.beginReviewWorkflow(false, msg); err == nil {
		t.Fatal("corrupt workflow silently enabled implementation")
	}
}

func TestReviewWorkflowCannotDelegateBeforeApproval(t *testing.T) {
	delegate := &sideEffectTool{name: "agent", planSafe: true}
	eng := planEngine(t, delegate)
	eng.SetPermissionMode(permission.Bypass)
	if err := eng.SetWorkflowMode("once"); err != nil {
		t.Fatal(err)
	}
	if err := eng.beginReviewWorkflow(false); err != nil {
		t.Fatal(err)
	}
	if out, failed := run1(t, eng, "agent", nil); !failed || delegate.calls != 0 || !strings.Contains(out, "先确认") {
		t.Fatalf("delegate bypassed review: calls=%d out=%q", delegate.calls, out)
	}
}

func TestReviewWorkflowNoHandlerKeepsOfferedPlan(t *testing.T) {
	eng := planEngine(t, tool.NewExitPlanModeTool())
	eng.SetPermissionMode(permission.Bypass)
	if err := eng.SetWorkflowMode("review"); err != nil {
		t.Fatal(err)
	}
	if err := eng.beginReviewWorkflow(false); err != nil {
		t.Fatal(err)
	}
	input := map[string]any{"summary": "修改返回值", "files": []string{"main.go"}, "decisions": []string{"保留接口"}, "checks": []string{"go test ./..."}}
	if out, failed := run1(t, eng, "exit_plan_mode", input); !failed || !strings.Contains(out, "保持待确认") {
		t.Fatalf("missing handler enabled implementation: %q", out)
	}
	state := eng.WorkflowStatus()
	if !state.Pending || state.Plan == nil || state.Plan.Summary != "修改返回值" || state.Plan.Decision != "pending" {
		t.Fatal("unanswered plan was not saved as pending")
	}
}

func TestReviewWorkflowRunMessageApprovalThenImplementation(t *testing.T) {
	eng := planEngine(t, tool.NewWriteTool(), tool.NewExitPlanModeTool())
	eng.SetPermissionMode(permission.Bypass)
	if err := eng.SetWorkflowMode("once"); err != nil {
		t.Fatal(err)
	}
	target, err := filepath.Abs("main.go")
	if err != nil {
		t.Fatal(err)
	}
	plan := map[string]any{"summary": "写入实现", "files": []string{"main.go"}, "decisions": []string{"不引入依赖"}, "checks": []string{"检查内容"}}
	write := api.ToolCall{Name: "write", Input: map[string]any{"filePath": target, "content": "implementation"}}
	write.ID = "early"
	provider := &mockProvider{responses: []mockResponse{
		{toolCalls: []api.ToolCall{write}},
		{toolCalls: []api.ToolCall{{ID: "plan", Name: "exit_plan_mode", Input: plan}}},
		{toolCalls: []api.ToolCall{{ID: "approved-write", Name: "write", Input: write.Input}}},
		{content: "done"},
	}}
	eng.SetProvider(provider)
	asked := 0
	eng.PermissionPrompt = func(name string, _ map[string]any, reason string) bool {
		asked++
		if name != "exit_plan_mode" || !strings.Contains(reason, "不引入依赖") || !strings.Contains(reason, "检查内容") {
			t.Fatalf("incomplete approval prompt: %s %s", name, reason)
		}
		if _, err := os.Stat(target); !os.IsNotExist(err) {
			t.Fatal("file was already mutated before approval")
		}
		if !strings.Contains(provider.lastReq.SystemBase+provider.lastReq.System, "先确认后实现") {
			t.Fatal("review instructions missing")
		}
		return true
	}
	if _, err := eng.RunMessageWithStream(context.Background(), api.Message{Role: "user", Content: "实现功能"}, nil, nil); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(target)
	if err != nil || string(data) != "implementation" || asked != 1 || eng.WorkflowStatus().Pending || eng.WorkflowStatus().Plan.Decision != "approved" {
		t.Fatalf("approved task did not complete: data=%q err=%v asked=%d", data, err, asked)
	}
	eng.SetProvider(&mockProvider{responses: []mockResponse{{content: "next task"}}})
	if _, err := eng.RunMessageWithStream(context.Background(), api.Message{Role: "user", Content: "另一个任务"}, nil, nil); err != nil {
		t.Fatal(err)
	}
	if eng.WorkflowStatus().Active || eng.WorkflowStatus().Plan != nil || asked != 1 {
		t.Fatal("once applied to a second task")
	}
}
