package command

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/liuzhixin405/cove-agent/internal/api"
	"github.com/liuzhixin405/cove-agent/internal/config"
	"github.com/liuzhixin405/cove-agent/internal/permission"
	"github.com/liuzhixin405/cove-agent/internal/session"
)

type commitTestVerifier struct {
	fakeEngine
	verify func([]string) error
}

func (v *commitTestVerifier) VerifyCommit(_ context.Context, paths []string) error {
	return v.verify(paths)
}

func commitTestWrite(t *testing.T, dir, name, text string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
}

func commitTestRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	runGit(t, dir, "init")
	runGit(t, dir, "config", "user.email", "commit@example.invalid")
	runGit(t, dir, "config", "user.name", "Commit Test")
	runGit(t, dir, "config", "commit.gpgsign", "false")
	runGit(t, dir, "config", "core.hooksPath", filepath.Join(dir, ".git", "hooks"))
	for _, name := range []string{"selected.txt", "other.txt"} {
		commitTestWrite(t, dir, name, "initial\n")
	}
	runGit(t, dir, "add", "-A")
	runGit(t, dir, "commit", "-m", "initial")
	commitTestWrite(t, dir, "selected.txt", "selected\n")
	runGit(t, dir, "add", "selected.txt")
	commitTestWrite(t, dir, "other.txt", "other\n")
	commitTestWrite(t, dir, "draft notes.txt", "draft\n")
	return dir
}

func commitTestGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	output, err := gitOutput(context.Background(), dir, args...)
	if err != nil {
		t.Fatal(err)
	}
	return output
}

func TestCommitScopeAndPreview(t *testing.T) {
	for _, mode := range []string{"staged", "all", "only", "preview"} {
		t.Run(mode, func(t *testing.T) {
			dir := commitTestRepo(t)
			head := commitTestGit(t, dir, "rev-parse", "HEAD")
			index := commitTestGit(t, dir, "write-tree")
			var args []string
			switch mode {
			case "all":
				args = []string{"--all", "all changes"}
			case "only":
				args = []string{"--only", "draft notes.txt", "--", "selected draft"}
			case "preview":
				args = []string{"--preview", "--all"}
			default:
				args = []string{"staged changes"}
			}
			out, err := NewCommitCmd().Execute(context.Background(), Input{Cwd: dir, Args: args})
			if err != nil {
				t.Fatal(err)
			}
			if mode == "preview" {
				if commitTestGit(t, dir, "rev-parse", "HEAD") != head || commitTestGit(t, dir, "write-tree") != index || !strings.Contains(out.Message, "draft notes.txt") {
					t.Fatalf("preview mutated repository or hid draft: %s", out.Message)
				}
				return
			}
			files := commitTestGit(t, dir, "diff-tree", "--no-commit-id", "--name-only", "-r", "HEAD")
			switch mode {
			case "staged":
				if files != "selected.txt\n" {
					t.Fatalf("staged commit included %q", files)
				}
			case "only":
				if files != "draft notes.txt\n" || commitTestGit(t, dir, "diff", "--cached", "--name-only") != "selected.txt\n" {
					t.Fatalf("only commit scope/index wrong: %q", files)
				}
			case "all":
				if !strings.Contains(files, "draft notes.txt") || !strings.Contains(files, "other.txt") || commitTestGit(t, dir, "status", "--porcelain") != "" {
					t.Fatalf("all commit incomplete: %q", files)
				}
			}
		})
	}
}

func TestCommitFailurePreservesIndex(t *testing.T) {
	for _, mode := range []string{"staged", "all", "only"} {
		for _, failure := range []string{"verification", "hook", "whitespace", "partial", "index-change"} {
			t.Run(mode+"/"+failure, func(t *testing.T) {
				dir := commitTestRepo(t)
				var args []string
				if mode == "all" {
					args = []string{"--all"}
				} else if mode == "only" {
					args = []string{"--only", "selected.txt", "--"}
				}
				verifier := &commitTestVerifier{verify: func([]string) error { return nil }}
				switch failure {
				case "verification":
					verifier.verify = func([]string) error { return errors.New("LAB_TEST_FAILED") }
				case "hook":
					if err := os.WriteFile(filepath.Join(dir, ".git", "hooks", "pre-commit"), []byte("#!/bin/sh\necho LAB_HOOK_REJECT >&2\nexit 1\n"), 0o755); err != nil {
						t.Fatal(err)
					}
				case "whitespace":
					commitTestWrite(t, dir, "selected.txt", "trailing space \n")
					runGit(t, dir, "add", "selected.txt")
				case "partial":
					if mode != "staged" {
						t.Skip("explicit scope selects worktree content")
					}
					commitTestWrite(t, dir, "selected.txt", "different worktree\n")
				case "index-change":
					if mode != "staged" {
						t.Skip("temporary index is isolated from real index")
					}
					verifier.verify = func([]string) error { runGit(t, dir, "add", "other.txt"); return nil }
				}
				head, index := commitTestGit(t, dir, "rev-parse", "HEAD"), commitTestGit(t, dir, "write-tree")
				_, err := NewCommitCmd().Execute(context.Background(), Input{Cwd: dir, Args: args, Engine: verifier})
				if err == nil || commitTestGit(t, dir, "rev-parse", "HEAD") != head {
					t.Fatalf("failure created commit or returned nil: %v", err)
				}
				if failure != "index-change" && commitTestGit(t, dir, "write-tree") != index {
					t.Fatal("failed commit changed original index")
				}
			})
		}
	}
}

func TestReviewAllChangeKinds(t *testing.T) {
	dir := commitTestRepo(t)
	out, err := NewReviewCmd().Execute(context.Background(), Input{Cwd: dir})
	if err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{"已暂存", "未暂存", "未跟踪", "selected.txt", "other.txt", "draft notes.txt"} {
		if !strings.Contains(out.Message, expected) {
			t.Fatalf("review missing %q: %s", expected, out.Message)
		}
	}
}

func TestCommitLiteralPathsAndInvalidScope(t *testing.T) {
	dir := commitTestRepo(t)
	commitTestWrite(t, dir, "file[1].txt", "literal name\n")
	_, err := NewCommitCmd().Execute(context.Background(), Input{Cwd: dir, Args: []string{"--only", "file[1].txt", "--", "literal filename"}})
	if err != nil {
		t.Fatal(err)
	}
	if files := commitTestGit(t, dir, "diff-tree", "--no-commit-id", "--name-only", "-r", "HEAD"); files != "file[1].txt\n" {
		t.Fatalf("path interpreted as glob: %q", files)
	}
	for _, args := range [][]string{{"--only"}, {"--only", "--"}, {"--only", "../outside.txt", "--"}, {"--unknown"}} {
		if _, err := NewCommitCmd().Execute(context.Background(), Input{Cwd: dir, Args: args}); err == nil {
			t.Errorf("invalid scope accepted: %v", args)
		}
	}
	runGit(t, dir, "reset", "-q", "HEAD", "--", "selected.txt")
	head := commitTestGit(t, dir, "rev-parse", "HEAD")
	if _, err := NewCommitCmd().Execute(context.Background(), Input{Cwd: dir}); err == nil || commitTestGit(t, dir, "rev-parse", "HEAD") != head {
		t.Fatalf("unstaged-only changes silently committed: %v", err)
	}
}

// liveEngine is a fakeEngine that also offers the optional live-update hooks
// the real engine adapter exposes.
type liveEngine struct {
	fakeEngine
	instructions []string
	workDirs     []string
	reloads      []string
	modes        []permission.Mode
	budgets      []float64
	resumed      []string
}

// /resume <id> must continue the saved session under its own ID; loading
// only its messages saved every resume as a new copy.
func TestResumeCmdContinuesSessionWhenEngineCan(t *testing.T) {
	store := &fakeSessionStore{records: map[string]session.Record{
		"abc": {ID: "abc", Title: "t", Messages: []api.Message{{Role: "user", Content: "hi"}}},
	}}
	eng := &liveEngine{}
	if _, err := NewResumeCmd().Execute(context.Background(), Input{Args: []string{"abc"}, SessionStore: store, Engine: eng}); err != nil {
		t.Fatal(err)
	}
	if len(eng.resumed) != 1 || eng.resumed[0] != "abc" {
		t.Fatalf("ResumeSession calls = %v, want [abc]", eng.resumed)
	}
	if eng.loaded != nil {
		t.Fatal("messages were loaded into a fresh session as well")
	}
}

func (l *liveEngine) SetCustomInstructions(ci string) bool {
	l.instructions = append(l.instructions, ci)
	return true
}
func (l *liveEngine) SetWorkingDir(dir string) bool {
	l.workDirs = append(l.workDirs, dir)
	return true
}
func (l *liveEngine) ReloadProvider(provider, model, baseURL, apiKey string) error {
	l.reloads = append(l.reloads, provider+"|"+model+"|"+baseURL)
	return nil
}
func (l *liveEngine) ResumeSession(r *session.Record) {
	l.resumed = append(l.resumed, r.ID)
	l.msgs = r.Messages
}
func (l *liveEngine) SetPermissionMode(m permission.Mode) { l.modes = append(l.modes, m) }
func (l *liveEngine) SetMaxBudget(b float64)              { l.budgets = append(l.budgets, b) }

type fakePermManager struct{ mode permission.Mode }

func (f *fakePermManager) Mode() permission.Mode        { return f.mode }
func (f *fakePermManager) SetMode(mode permission.Mode) { f.mode = mode }

func noSave(*config.Config) error { return nil }

// restoreWD puts the working directory back when the test ends. Call it after
// creating the temp dirs the test may cd into: cleanups run last-in first-out,
// and Windows cannot remove a directory that is still the process's cwd.
func restoreWD(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(wd) })
	return wd
}

// sameDir reports whether two paths name the same directory. It resolves
// symlinks first: macOS reports getcwd() under /private/var while t.TempDir()
// returns /var, so comparing the two as strings fails there.
func sameDir(a, b string) bool {
	ra, err := filepath.EvalSymlinks(a)
	if err != nil {
		return false
	}
	rb, err := filepath.EvalSymlinks(b)
	if err != nil {
		return false
	}
	return strings.EqualFold(ra, rb)
}

// A directory outside any repository: git's answer there is an error, which
// the git commands used to swallow and report as "no changes".
func nonRepoDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("GIT_CEILING_DIRECTORIES", filepath.Dir(dir))
	return dir
}

func TestCommitCmdOutsideRepoSaysSo(t *testing.T) {
	_, err := NewCommitCmd().Execute(context.Background(), Input{Cwd: nonRepoDir(t)})
	if err == nil || !strings.Contains(err.Error(), "git") {
		t.Fatalf("outside a repository /commit must return the git error, got %v", err)
	}
}

func TestReviewCmdOutsideRepoSaysSo(t *testing.T) {
	_, err := NewReviewCmd().Execute(context.Background(), Input{Cwd: nonRepoDir(t)})
	if err == nil || !strings.Contains(err.Error(), "git") {
		t.Fatalf("outside a repository /review must return the git error, got %v", err)
	}
}

func TestDiffCmdOutsideRepoSaysSo(t *testing.T) {
	out, _ := NewDiffCmd().Execute(context.Background(), Input{Cwd: nonRepoDir(t)})
	if out.Message == "无差异" || !strings.Contains(out.Message, "git") {
		t.Fatalf("outside a repository /diff must report the git error, got %q", out.Message)
	}
}

// /diff showed only the unstaged diff whenever there was one, so staged
// changes were invisible exactly when both kinds existed.
func TestDiffCmdShowsStagedAndUnstaged(t *testing.T) {
	dir := t.TempDir()
	runGit(t, dir, "init")
	runGit(t, dir, "config", "user.email", "test@example.com")
	runGit(t, dir, "config", "user.name", "Test User")
	for _, name := range []string{"a.txt", "b.txt"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("one\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	runGit(t, dir, "add", ".")
	runGit(t, dir, "commit", "-m", "init")
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("staged\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, dir, "add", "a.txt")
	if err := os.WriteFile(filepath.Join(dir, "b.txt"), []byte("unstaged\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	out, err := NewDiffCmd().Execute(context.Background(), Input{Cwd: dir})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.Data, "+staged") || !strings.Contains(out.Data, "+unstaged") {
		t.Fatalf("expected both staged and unstaged hunks, got:\n%s", out.Data)
	}
}

// /system used to replace the whole system prompt, dropping the role, tool
// rules and project context, while the saved value is appended to the
// built-in prompt on the next start.
func TestSystemCmdDoesNotReplaceWholePrompt(t *testing.T) {
	eng := &liveEngine{}
	cfg := config.DefaultConfig()
	_, err := NewSystemCmd().Execute(context.Background(), Input{Args: []string{"用中文回答"}, Config: cfg, SaveConfig: noSave, Engine: eng})
	if err != nil {
		t.Fatal(err)
	}
	if eng.override != "" {
		t.Fatalf("system prompt was overridden with %q", eng.override)
	}
	if len(eng.instructions) != 1 || eng.instructions[0] != "用中文回答" {
		t.Fatalf("custom instructions not applied live: %v", eng.instructions)
	}
	if cfg.SystemPrompt != "用中文回答" {
		t.Fatalf("config not updated: %q", cfg.SystemPrompt)
	}
}

func TestSystemCmdWithoutLiveEngineSaysNextSession(t *testing.T) {
	eng := &fakeEngine{}
	out, err := NewSystemCmd().Execute(context.Background(), Input{Args: []string{"x"}, Config: config.DefaultConfig(), SaveConfig: noSave, Engine: eng})
	if err != nil {
		t.Fatal(err)
	}
	if eng.override != "" {
		t.Fatalf("system prompt was overridden with %q", eng.override)
	}
	if !strings.Contains(out.Message, "下次") {
		t.Fatalf("message must say when it takes effect, got %q", out.Message)
	}
}

// Windows paths routinely contain spaces ("My Projects"), and the command
// line is split on whitespace before /cd sees it.
func TestCdCmdAcceptsPathWithSpaces(t *testing.T) {
	base := t.TempDir()
	restoreWD(t)
	target := filepath.Join(base, "My Projects")
	if err := os.Mkdir(target, 0o755); err != nil {
		t.Fatal(err)
	}
	out, err := NewCdCmd().Execute(context.Background(), Input{Args: []string{"My", "Projects"}, Cwd: base})
	if err != nil {
		t.Fatal(err)
	}
	wd, _ := os.Getwd()
	if !sameDir(wd, target) {
		t.Fatalf("cwd = %s, want %s (%s)", wd, target, out.Message)
	}
}

func TestCdCmdStripsQuotes(t *testing.T) {
	base := t.TempDir()
	restoreWD(t)
	target := filepath.Join(base, "a b")
	if err := os.Mkdir(target, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := NewCdCmd().Execute(context.Background(), Input{Args: []string{`"a`, `b"`}, Cwd: base}); err != nil {
		t.Fatal(err)
	}
	wd, _ := os.Getwd()
	if !sameDir(wd, target) {
		t.Fatalf("cwd = %s, want %s", wd, target)
	}
}

// The engine keeps its own idea of the project directory (checkpoints, the
// session's project); /cd has to tell it, or /undo restores the old project.
func TestCdCmdTellsEngineTheNewDir(t *testing.T) {
	target := t.TempDir()
	origWD := restoreWD(t)
	eng := &liveEngine{}
	if _, err := NewCdCmd().Execute(context.Background(), Input{Args: []string{target}, Cwd: origWD, Engine: eng}); err != nil {
		t.Fatal(err)
	}
	if len(eng.workDirs) != 1 || !sameDir(eng.workDirs[0], target) {
		t.Fatalf("engine working dir updates = %v, want [%s]", eng.workDirs, target)
	}
}

// Until the engine can follow a directory change, /cd must not pretend the
// whole session moved.
func TestCdCmdWarnsWhenEngineCannotFollow(t *testing.T) {
	target := t.TempDir()
	origWD := restoreWD(t)
	out, err := NewCdCmd().Execute(context.Background(), Input{Args: []string{target}, Cwd: origWD, Engine: &fakeEngine{}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.Message, "检查点") {
		t.Fatalf("expected a note that checkpoints stay on the old directory, got %q", out.Message)
	}
}

func TestCdCmdFailedChdirDoesNotTellEngine(t *testing.T) {
	eng := &liveEngine{}
	out, _ := NewCdCmd().Execute(context.Background(), Input{Args: []string{filepath.Join(t.TempDir(), "missing")}, Cwd: t.TempDir(), Engine: eng})
	if len(eng.workDirs) != 0 {
		t.Fatalf("engine told about a directory it never entered: %v", eng.workDirs)
	}
	if !strings.Contains(out.Message, "错误") {
		t.Fatalf("expected an error message, got %q", out.Message)
	}
}

// "/config provider deepseek-typo" used to be saved as-is and break the next
// start.
func TestConfigCmdRejectsUnknownProvider(t *testing.T) {
	cfg := config.DefaultConfig()
	before := cfg.Provider.Name
	_, err := NewConfigCmd().Execute(context.Background(), Input{Args: []string{"provider", "deepsek"}, Config: cfg, SaveConfig: noSave})
	if err == nil {
		t.Fatal("expected an error for an unknown provider")
	}
	if cfg.Provider.Name != before {
		t.Fatalf("provider changed to %q", cfg.Provider.Name)
	}
}

// "/config model x" said 已保存 but the running session kept the old model
// until restart; /model applies it immediately.
func TestConfigCmdAppliesModelToRunningEngine(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Provider.Name = "deepseek"
	eng := &liveEngine{}
	_, err := NewConfigCmd().Execute(context.Background(), Input{Args: []string{"model", "deepseek-v4-flash"}, Config: cfg, SaveConfig: noSave, Engine: eng})
	if err != nil {
		t.Fatal(err)
	}
	if len(eng.reloads) != 1 || !strings.HasPrefix(eng.reloads[0], "deepseek|deepseek-v4-flash|") {
		t.Fatalf("provider reloads = %v", eng.reloads)
	}
}

func TestConfigCmdAppliesModeToRunningSession(t *testing.T) {
	cfg := config.DefaultConfig()
	eng := &liveEngine{}
	pm := &fakePermManager{mode: permission.Default}
	_, err := NewConfigCmd().Execute(context.Background(), Input{Args: []string{"mode", "auto"}, Config: cfg, SaveConfig: noSave, Engine: eng, PermissionManager: pm})
	if err != nil {
		t.Fatal(err)
	}
	// The engine owns the manager tool calls are decided by; the view's
	// manager is that same one in the front ends, so it is not set twice.
	if len(eng.modes) != 1 || eng.modes[0] != permission.Mode("auto") {
		t.Fatalf("mode not applied to the engine: %v", eng.modes)
	}

	// An engine view that cannot take the mode still leaves the manager set.
	pm = &fakePermManager{mode: permission.Default}
	_, err = NewConfigCmd().Execute(context.Background(), Input{Args: []string{"mode", "plan"}, Config: cfg, SaveConfig: noSave, Engine: &fakeEngine{}, PermissionManager: pm})
	if err != nil {
		t.Fatal(err)
	}
	if pm.mode != permission.Plan {
		t.Fatalf("manager fallback not applied: %s", pm.mode)
	}
}

func TestConfigCmdAppliesBudgetToRunningEngine(t *testing.T) {
	eng := &liveEngine{}
	_, err := NewConfigCmd().Execute(context.Background(), Input{Args: []string{"budget", "3.5"}, Config: config.DefaultConfig(), SaveConfig: noSave, Engine: eng})
	if err != nil {
		t.Fatal(err)
	}
	if len(eng.budgets) != 1 || eng.budgets[0] != 3.5 {
		t.Fatalf("budget not applied live: %v", eng.budgets)
	}
}

func TestConfigCmdWithoutLiveEngineSaysRestart(t *testing.T) {
	out, err := NewConfigCmd().Execute(context.Background(), Input{Args: []string{"model", "gpt-4o"}, Config: config.DefaultConfig(), SaveConfig: noSave, Engine: &fakeEngine{}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.Message, "重启") {
		t.Fatalf("message must say the change needs a restart, got %q", out.Message)
	}
}

// /doctor's help promised a config check it never did; a missing API key is
// the most common first-run problem.
func TestDoctorCmdReportsConfig(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Provider.Name = "deepseek"
	cfg.Provider.APIKey = ""
	t.Setenv("DEEPSEEK_API_KEY", "")
	t.Setenv("LLM_API_KEY", "")
	out, _ := NewDoctorCmd().Execute(context.Background(), Input{Cwd: t.TempDir(), Config: cfg})
	if !strings.Contains(out.Message, "deepseek") || !strings.Contains(out.Message, "API key") {
		t.Fatalf("doctor output lacks provider/API key status:\n%s", out.Message)
	}
}
