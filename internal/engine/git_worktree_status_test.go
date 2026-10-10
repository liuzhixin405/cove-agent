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
	ctxt "github.com/liuzhixin405/cove-agent/internal/context"
	"github.com/liuzhixin405/cove-agent/internal/permission"
	"github.com/liuzhixin405/cove-agent/internal/tool"
)

func TestParseGitStatusAndLine(t *testing.T) {
	for _, tc := range []struct {
		name, out, want string
	}{
		{"dirty", "## main...origin/main\n M a.go\n?? b.go\n", "git：2 个文件有未提交的改动"},
		{"dirty and ahead", "## main...origin/main [ahead 2]\n M a.go\n", "git：1 个文件有未提交的改动，2 个提交未推送到 origin/main"},
		{"committed, not pushed", "## main...origin/main [ahead 3, behind 1]\n", "git：改动已提交，3 个提交未推送到 origin/main"},
		// A state, not "已提交并推送": the line also follows turns that
		// committed nothing.
		{"in sync", "## main...origin/main\n", "git：工作区干净，与 origin/main 同步（按本地记录）"},
		{"behind only has nothing unpushed", "## main...origin/main [behind 4]\n", "git：工作区干净，没有未推送的提交，落后 origin/main 4 个提交（按本地记录）"},
		{"no upstream", "## feature\n", "git：改动已提交，分支 feature 没有上游，未推送"},
		{"upstream deleted", "## feature...origin/feature [gone]\n", "git：改动已提交，分支 feature 没有上游，未推送"},
		{"no commits yet", "## No commits yet on main\n?? a.go\n", "git：1 个文件有未提交的改动，分支 main 没有上游，未推送"},
		{"detached", "## HEAD (no branch)\n M a.go\n", "git：1 个文件有未提交的改动"},
		{"crlf", "## main...origin/main\r\n M a.go\r\n", "git：1 个文件有未提交的改动"},
	} {
		if got := parseGitStatus(tc.out).Line(); got != tc.want {
			t.Errorf("%s: Line() = %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestGitCommandRe(t *testing.T) {
	for cmd, want := range map[string]bool{
		"git commit -m x":             true,
		"cd sub && git push":          true,
		"git.exe status":              true,
		"(git add -A)":                true,
		"echo digit test":             false,
		"cat .gitignore":              false,
		"go test ./internal/gitdir/":  false,
		"github-cli version":          false,
		"npm run lint; git status -s": true,
	} {
		if got := gitCommandRe.MatchString(cmd); got != want {
			t.Errorf("%q: %v, want %v", cmd, got, want)
		}
	}
}

// The line appears only after a turn that changed files or ran a
// repository-changing git command, in a repository.
func TestReportGitWorkState(t *testing.T) {
	eng := newTestEngine(&mockProvider{})
	var lines []string
	eng.SetOutput(LineSink(func(s string) { lines = append(lines, s) }))
	eng.projCtx = &ctxt.ProjectContext{Cwd: t.TempDir(), IsGitRepo: true}
	orig := readGitWorkStateFn
	defer func() { readGitWorkStateFn = orig }()
	readGitWorkStateFn = func(context.Context, string) (gitWorkState, error) {
		return gitWorkState{Branch: "main", Upstream: "origin/main", Changed: 1}, nil
	}

	eng.reportGitWorkState(context.Background())
	if len(lines) != 0 {
		t.Fatalf("a turn that changed nothing got a git line: %q", lines)
	}

	eng.turnFilesChanged = true
	eng.reportGitWorkState(context.Background())
	if len(lines) != 1 || !strings.Contains(lines[0], "1 个文件有未提交的改动") {
		t.Fatalf("lines = %q", lines)
	}

	lines = nil
	eng.turnFilesChanged, eng.turnRanGit = false, true
	eng.reportGitWorkState(context.Background())
	if len(lines) != 1 {
		t.Fatalf("a turn that ran git got no line: %q", lines)
	}

	lines = nil
	readGitWorkStateFn = func(context.Context, string) (gitWorkState, error) {
		return gitWorkState{}, errors.New("not a repository")
	}
	eng.reportGitWorkState(context.Background())
	if len(lines) != 0 {
		t.Fatalf("a failed git status printed %q", lines)
	}

	eng.projCtx.IsGitRepo = false
	eng.turnFilesChanged = true
	readGitWorkStateFn = orig
	eng.reportGitWorkState(context.Background())
	if len(lines) != 0 {
		t.Fatalf("outside a repository: %q", lines)
	}
}

// Against a real git: uncommitted, committed but not pushed, pushed.
func TestReadGitWorkStateRealRepo(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	root := t.TempDir()
	remote, work := filepath.Join(root, "remote.git"), filepath.Join(root, "work")
	git := func(dir string, args ...string) string {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-c", "user.name=t", "-c", "user.email=t@t", "-c", "commit.gpgsign=false"}, args...)...)
		cmd.Dir = dir
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return string(out)
	}
	git(root, "init", "--bare", "-b", "main", remote)
	git(root, "clone", remote, work)
	git(work, "checkout", "-b", "main")
	state := func() gitWorkState {
		t.Helper()
		s, err := readGitWorkState(context.Background(), work)
		if err != nil {
			t.Fatal(err)
		}
		return s
	}

	if err := os.WriteFile(filepath.Join(work, "a.txt"), []byte("a"), 0o644); err != nil {
		t.Fatal(err)
	}
	if s := state(); s.Changed != 1 || s.Upstream != "" {
		t.Fatalf("new file: %+v", s)
	}
	git(work, "add", "a.txt")
	t.Setenv("COVE_CONFIG_DIR", t.TempDir())
	t.Chdir(work)
	eng := newPatternEngine(t, &seqProvider{}, nil, tool.NewBashTool())
	eng.projCtx = &ctxt.ProjectContext{Cwd: work, IsGitRepo: true}
	eng.verifyGate = NewVerifyGate([]string{"build"}, work)
	eng.beginAcceptance(api.Message{Role: "user", Content: "release"})
	record := func(line, output string, failed bool) {
		eng.recordGitEvidence(context.Background(), api.ToolCall{Name: "bash", Input: map[string]any{"command": line}}, work, output, failed)
	}
	record("git commit -m a", git(work, "commit", "-m", "a"), false)
	record("git tag -a v-test -m v-test", git(work, "tag", "-a", "v-test", "-m", "v-test"), false)
	record("git push --atomic -u origin main v-test", git(work, "push", "--atomic", "-u", "origin", "main", "v-test"), false)
	output, failed := eng.executeTool(context.Background(), api.ToolCall{Name: "bash", Input: map[string]any{"command": "git ls-remote origin refs/heads/main refs/tags/v-test 'refs/tags/v-test^{}'"}})
	if failed {
		t.Fatalf("real shell query failed: %s", output)
	}
	eng.recordAcceptanceResults([]string{"build"}, []VerifyResult{{Command: "build", Passed: true}})
	eng.noteVerifyEvidence(eng.newTurnLimits(), "bash", map[string]any{"command": "git push"}, "ok", false)
	report, err := eng.LastAcceptance()
	if err != nil || len(report.Git) != 4 || report.Checks[0].Status != "unverified" {
		t.Fatalf("Git evidence missing or invalidation bypassed: %+v %v", report, err)
	}
	for _, evidence := range report.Git {
		if evidence.Status != "passed" || evidence.Commit == "" {
			t.Fatalf("real Git operation not recorded: %+v", evidence)
		}
	}
	if len(report.Git[3].RemoteRefs) != 3 || !strings.Contains(report.Summary(), "远端引用:") {
		t.Fatalf("remote hashes not archived: %+v", report.Git[3])
	}
	report.Git[3].RemoteRefs["refs/heads/main"] = "tampered"
	again, _ := eng.LastAcceptance()
	if again.Git[3].RemoteRefs["refs/heads/main"] == "tampered" {
		t.Fatal("caller mutated live Git evidence")
	}
	eng.finishAcceptance(nil)
	eng.acceptance = nil
	reloaded, err := eng.LastAcceptance()
	if err != nil || len(reloaded.Git) != 4 || reloaded.Git[3].Status != "passed" {
		t.Fatalf("persisted Git evidence lost: %+v %v", reloaded, err)
	}
	eng.beginAcceptance(api.Message{Role: "user", Content: "next task"})
	next, _ := eng.LastAcceptance()
	if len(next.Git) != 0 {
		t.Fatal("new task reused previous Git evidence")
	}
	if got := state().Line(); got != "git：工作区干净，与 origin/main 同步（按本地记录）" {
		t.Fatalf("after push: %q", got)
	}
	if err := os.WriteFile(filepath.Join(work, "a.txt"), []byte("b"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(work, "commit", "-am", "b")
	if got := state().Line(); got != "git：改动已提交，1 个提交未推送到 origin/main" {
		t.Fatalf("after commit: %q", got)
	}
}

func TestGitEvidenceRejectsUnprovenResults(t *testing.T) {
	dir := t.TempDir()
	eng := newPatternEngine(t, &seqProvider{}, nil)
	eng.verifyGate = NewVerifyGate(nil, dir)
	eng.beginAcceptance(api.Message{Role: "user", Content: "release"})
	for _, line := range []string{"echo 'git push'", "git push; echo done", "git tag -a v1 -m v1 && git status", "cd elsewhere; git push", "git push > output.txt", "bash -c 'git push'", "git push &", "git push & # background"} {
		eng.recordGitEvidence(context.Background(), api.ToolCall{Name: "bash", Input: map[string]any{"command": line}}, dir, "ok", false)
	}
	report, _ := eng.LastAcceptance()
	if len(report.Git) != 0 {
		t.Fatalf("ambiguous/quoted commands produced evidence: %+v", report.Git)
	}
	for _, output := range []string{"", "ok", "not-a-hash\trefs/heads/main", strings.Repeat("a", 40) + "\trefs/heads/main"} {
		eng.recordGitEvidence(context.Background(), api.ToolCall{Name: "bash", Input: map[string]any{"command": "git ls-remote origin refs/heads/main"}}, dir, output, false)
	}
	report, _ = eng.LastAcceptance()
	for _, evidence := range report.Git {
		if evidence.Status == "passed" {
			t.Fatalf("unproven remote state accepted: %+v", evidence)
		}
	}
	eng.recordGitEvidence(context.Background(), api.ToolCall{Name: "bash", Input: map[string]any{"command": "git push"}}, dir, "permission denied", true)
	report, _ = eng.LastAcceptance()
	if last := report.Git[len(report.Git)-1]; last.Status != "failed" || last.Reason != "permission denied" {
		t.Fatalf("failed push not recorded: %+v", last)
	}
}

func TestGitEvidenceRecordsDeniedToolWithoutExecuting(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	bash := &mockTool{name: "bash"}
	eng := newPatternEngine(t, &seqProvider{}, nil, bash)
	eng.projCtx = &ctxt.ProjectContext{Cwd: dir}
	eng.verifyGate = NewVerifyGate(nil, dir)
	eng.perm.SetMode(permission.Default)
	eng.PermissionPrompt = func(string, map[string]any, string) bool { return false }
	eng.beginAcceptance(api.Message{Role: "user", Content: "release"})
	output, failed := eng.executeTool(context.Background(), api.ToolCall{Name: "bash", Input: map[string]any{"command": "git push"}})
	report, err := eng.LastAcceptance()
	if !failed || bash.callCount != 0 || err != nil || len(report.Git) != 1 || report.Git[0].Status != "failed" || !strings.Contains(report.Git[0].Reason, "Error:") {
		t.Fatalf("denied push not safely recorded: failed=%v calls=%d report=%+v err=%v output=%s", failed, bash.callCount, report, err, output)
	}
}
