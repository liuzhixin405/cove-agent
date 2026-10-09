package tool

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

type PlanModeTool struct{ baseTool }
type ExitPlanModeTool struct{ baseTool }
type EnterWorktreeTool struct{ baseTool }
type ExitWorktreeTool struct{ baseTool }

func NewPlanModeTool() Tool {
	return &PlanModeTool{baseTool{def: Def{
		Name: "plan_mode", Aliases: []string{"EnterPlanMode"},
		Description: "Enter plan mode: only read operations allowed. Use before complex changes.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"reason":{"type":"string"}}}`),
		IsReadOnly:  true, UserFacingName: "Plan Mode",
	}}}
}
func (t *PlanModeTool) Call(ctx context.Context, input Input, tctx Context) (Result, error) {
	reason, _ := input["reason"].(string)
	if tctx.Runtime != nil {
		tctx.Runtime.SetPlanMode(true)
	}
	return Result{Data: fmt.Sprintf("Plan mode active. Read-only operations only. Reason: %s", reason)}, nil
}
func (t *PlanModeTool) CheckPermissions(input Input, tctx Context) PermissionDecision {
	return Allowed("plan mode entry is safe")
}

func NewExitPlanModeTool() Tool {
	return &ExitPlanModeTool{baseTool{def: Def{
		Name: "exit_plan_mode", Aliases: []string{"ExitPlanMode"},
		Description: "Exit plan mode. Full tool access restored.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"summary":{"type":"string"}}}`),
		IsReadOnly:  false, UserFacingName: "Exit Plan Mode",
	}}}
}
func (t *ExitPlanModeTool) Call(ctx context.Context, input Input, tctx Context) (Result, error) {
	summary, _ := input["summary"].(string)
	if tctx.Runtime != nil {
		tctx.Runtime.SetPlanMode(false)
	}
	return Result{Data: fmt.Sprintf("Plan mode exited. Summary: %s", summary)}, nil
}
func (t *ExitPlanModeTool) CheckPermissions(input Input, tctx Context) PermissionDecision {
	return Asked("exiting plan mode requires confirmation")
}

func NewEnterWorktreeTool() Tool {
	return &EnterWorktreeTool{baseTool{def: Def{
		Name: "worktree", Aliases: []string{"EnterWorktree"},
		Description: "Create a git worktree for isolated work.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"branch":{"type":"string"}},"required":["branch"]}`),
		IsReadOnly:  false, UserFacingName: "Enter Worktree",
	}}}
}
func (t *EnterWorktreeTool) Call(ctx context.Context, input Input, tctx Context) (Result, error) {
	branch, _ := input["branch"].(string)
	if err := validWorktreeBranch(branch); err != nil {
		return Result{Data: "Error: " + err.Error(), IsError: true}, nil
	}
	cwd := tctx.Cwd
	if cwd == "" {
		cwd, _ = os.Getwd()
	}
	// Recorded as a cleaned absolute path; cwd + "/../" + branch gave
	// `D:\proj/../feat` on Windows.
	wtPath := filepath.Join(cwd, "..", filepath.FromSlash(branch))
	cmd := exec.CommandContext(ctx, "git", "worktree", "add", wtPath, "-b", branch)
	cmd.Dir = cwd
	out, err := cmd.CombinedOutput()
	if err != nil {
		return Result{Data: fmt.Sprintf("Error creating worktree: %v\n%s", err, string(out)), IsError: true}, nil
	}
	if tctx.Runtime != nil {
		tctx.Runtime.SetWorktreeMain(cwd)
		tctx.Runtime.SetWorktreeDir(wtPath)
	}
	return Result{Data: fmt.Sprintf("Worktree created at %s and entered: until exit_worktree, read/edit/write/glob/grep and shell commands run inside it (the main tree is not reachable; checkpoints and /undo cover the main tree only).\nGit output: %s", wtPath, string(out))}, nil
}
func (t *EnterWorktreeTool) CheckPermissions(input Input, tctx Context) PermissionDecision {
	return Asked("worktree creation requires confirmation")
}

func NewExitWorktreeTool() Tool {
	return &ExitWorktreeTool{baseTool{def: Def{
		Name: "exit_worktree", Aliases: []string{"ExitWorktree"},
		Description: "Exit the current worktree.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"merge":{"type":"boolean"}}}`),
		IsReadOnly:  false, UserFacingName: "Exit Worktree",
	}}}
}
func (t *ExitWorktreeTool) Call(ctx context.Context, input Input, tctx Context) (Result, error) {
	if tctx.Runtime == nil {
		return Result{Data: "No active worktree", IsError: true}, nil
	}
	wtPath := tctx.Runtime.GetWorktreeDir()
	if wtPath == "" {
		return Result{Data: "No active worktree", IsError: true}, nil
	}
	// Run git from the project the worktree was entered from: tctx.Cwd is
	// the worktree itself now, and git will not remove the tree it runs in.
	cwd := tctx.Runtime.GetWorktreeMain()
	if cwd == "" {
		cwd = tctx.Cwd
	}
	if cwd == "" || cwd == wtPath {
		cwd, _ = os.Getwd()
	}
	cmd := exec.CommandContext(ctx, "git", "worktree", "remove", wtPath)
	cmd.Dir = cwd
	out, err := cmd.CombinedOutput()
	if err != nil {
		// Keep tracking it: git refused (uncommitted changes, wrong repo),
		// so the worktree is still on disk. This used to report success and
		// forget it.
		return Result{Data: fmt.Sprintf("Error removing worktree %s: %v\n%s", wtPath, err, string(out)), IsError: true}, nil
	}
	tctx.Runtime.SetWorktreeDir("")
	tctx.Runtime.SetWorktreeMain("")
	return Result{Data: fmt.Sprintf("Worktree removed; tools run in the project directory again.\n%s", string(out))}, nil
}

// validWorktreeBranch rejects branch names that git would parse as an option
// ("-f") or that walk the new worktree out of the project's parent ("..").
func validWorktreeBranch(branch string) error {
	if strings.TrimSpace(branch) == "" || strings.HasPrefix(branch, "-") {
		return fmt.Errorf("invalid branch name %q", branch)
	}
	for _, part := range strings.FieldsFunc(branch, func(r rune) bool { return r == '/' || r == '\\' }) {
		if part == ".." {
			return fmt.Errorf("invalid branch name %q", branch)
		}
	}
	return nil
}
func (t *ExitWorktreeTool) CheckPermissions(input Input, tctx Context) PermissionDecision {
	return Asked("worktree removal requires confirmation")
}
