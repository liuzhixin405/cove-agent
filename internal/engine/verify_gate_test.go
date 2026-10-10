package engine

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/liuzhixin405/cove-agent/internal/api"
)

func TestVerifyCommitRechecksAndFailsClosed(t *testing.T) {
	gate, runs := countingVerifyGate("go test ./...")
	eng := &Engine{verifyGate: gate}
	var lines []string
	eng.SetOutput(LineSink(func(line string) { lines = append(lines, line) }))
	for count := 1; count <= 2; count++ {
		if err := eng.VerifyCommit(context.Background(), []string{"value.go"}); err != nil || *runs != count {
			t.Fatalf("verification reused stale evidence: runs=%d err=%v", *runs, err)
		}
	}
	if output := strings.Join(lines, "\n"); !strings.Contains(output, "提交前验证通过") || strings.Contains(output, "not accepted") {
		t.Fatalf("successful checks reported failure: %s", output)
	}
	gate.runner = func(context.Context, string, string) (string, int, error) { return "LAB_TEST_FAILED", 1, nil }
	if err := eng.VerifyCommit(context.Background(), nil); err == nil || !strings.Contains(err.Error(), "LAB_TEST_FAILED") {
		t.Fatalf("failed test did not reject commit: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := eng.VerifyCommit(ctx, nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled verification returned %v", err)
	}
}

func TestVerifyCommitHonorsProjectTrust(t *testing.T) {
	eng, ran, _ := trustGateEngine(t, t.TempDir())
	if err := eng.VerifyCommit(context.Background(), []string{"a.go"}); err == nil || !strings.Contains(err.Error(), "/trust") || len(ran()) != 0 {
		t.Fatalf("untrusted automatic checks ran or were silently skipped: %v %v", err, ran())
	}
}

func TestVerifyShellCommitRequiresSeparateCommands(t *testing.T) {
	gate, _ := countingVerifyGate("go test ./...")
	eng := &Engine{verifyGate: gate}
	for _, line := range []string{"git add -A; git commit -m fix", "git commit -m fix; git push", "git -C other commit -m fix", "bash -c 'git commit -m fix'", "git commit --all -m fix", "git commit -m fix changed.go"} {
		if err := eng.verifyShellCommit(context.Background(), line, t.TempDir(), ""); err == nil {
			t.Errorf("unsafe compound/alternate scope accepted: %s", line)
		}
	}
	for _, line := range []string{"git status", "echo 'git commit -m fix'", "git push"} {
		if err := eng.verifyShellCommit(context.Background(), line, t.TempDir(), ""); err != nil {
			t.Errorf("non-commit blocked: %s: %v", line, err)
		}
	}
}

func TestVerifyShellCommitRunsChecksAgainstStagedRepo(t *testing.T) {
	dir := t.TempDir()
	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if output, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %s: %v", args, output, err)
		}
	}
	git("init")
	if err := os.WriteFile(filepath.Join(dir, "value.go"), []byte("package sample\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git("add", "value.go")
	gate, runs := countingVerifyGate("go test ./...")
	gate.workDir = dir
	eng := &Engine{verifyGate: gate}
	if err := eng.verifyShellCommit(context.Background(), `git commit -m "--all"`, dir, ""); err != nil || *runs != 1 {
		t.Fatalf("valid standalone commit not checked: runs %d err %v", *runs, err)
	}
	gate.runner = func(context.Context, string, string) (string, int, error) { return "LAB_TEST_FAILED", 1, nil }
	if err := eng.verifyShellCommit(context.Background(), "git commit -m fix", dir, ""); err == nil || !strings.Contains(err.Error(), "LAB_TEST_FAILED") {
		t.Fatalf("failed verification allowed shell commit: %v", err)
	}
	t.Chdir(dir)
	bash := &mockTool{name: "bash"}
	toolEngine := newPatternEngine(t, &seqProvider{}, nil, bash)
	toolEngine.verifyGate = gate
	output, failed := toolEngine.executeTool(context.Background(), api.ToolCall{ID: "commit-test", Name: "bash", Input: map[string]any{"command": "git commit -m fix"}})
	if !failed || bash.callCount != 0 || !strings.Contains(output, "LAB_TEST_FAILED") {
		t.Fatalf("failed checks reached shell tool: failed=%v calls=%d output=%s", failed, bash.callCount, output)
	}
	if err := os.WriteFile(filepath.Join(dir, "value.go"), []byte("package changed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := eng.verifyShellCommit(context.Background(), "git commit -m fix", dir, ""); err == nil || !strings.Contains(err.Error(), "工作区不同") {
		t.Fatalf("partial staged snapshot accepted: %v", err)
	}
}

func TestVerifyGate_DisabledWhenNoCommands(t *testing.T) {
	g := NewVerifyGate(nil, "")
	if g.Enabled() {
		t.Fatalf("expected gate with no commands to be disabled")
	}
	results, passed := g.Run(context.Background(), nil)
	if !passed || results != nil {
		t.Fatalf("expected no-op Run() to pass trivially, got passed=%v results=%v", passed, results)
	}
}

func TestVerifyGate_AllPass(t *testing.T) {
	g := NewVerifyGate([]string{"exit 0", "exit 0"}, "")
	if !g.Enabled() {
		t.Fatalf("expected gate with commands to be enabled")
	}
	results, passed := g.Run(context.Background(), nil)
	if !passed {
		t.Fatalf("expected all-passing commands to pass, results=%v", results)
	}
	if len(results) != 2 {
		t.Fatalf("expected 2 results, got %d", len(results))
	}
}

func TestVerifyGate_FailFastStopsAtFirstFailure(t *testing.T) {
	g := NewVerifyGate([]string{"exit 1", "exit 0"}, "")
	results, passed := g.Run(context.Background(), nil)
	if passed {
		t.Fatalf("expected failure to be reported")
	}
	if len(results) != 1 {
		t.Fatalf("expected fail-fast to stop after the first command, got %d results", len(results))
	}
	if results[0].Passed {
		t.Fatalf("expected first result to be marked failed")
	}
	if results[0].ExitCode != 1 {
		t.Fatalf("expected exit code 1, got %d", results[0].ExitCode)
	}
}

func TestVerifyGate_MaxRetriesDefault(t *testing.T) {
	g := NewVerifyGate([]string{"exit 1"}, "")
	if g.MaxRetries() <= 0 {
		t.Fatalf("expected a positive default retry budget")
	}
}

func TestSummary_MentionsFailedCommand(t *testing.T) {
	results := []VerifyResult{
		{Command: "go build ./...", Passed: false, ExitCode: 2, Output: "undefined: foo"},
	}
	s := Summary(results)
	if !strings.Contains(s, "go build ./...") || !strings.Contains(s, "undefined: foo") {
		t.Fatalf("expected summary to mention command and output, got: %s", s)
	}
}
