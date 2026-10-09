package automation

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func init() {
	mode := os.Getenv("COVE_AUTOMATION_TEST_CHILD")
	if mode == "" {
		return
	}
	if mode == "block" {
		timer := time.NewTimer(time.Hour)
		defer timer.Stop()
		<-timer.C
		os.Exit(8)
	}
	if mode == "runner-stdin" {
		stdin, _ := io.ReadAll(os.Stdin)
		data, err := json.Marshal(map[string]any{"args": os.Args[1:], "stdin": string(stdin)})
		if err != nil {
			os.Exit(11)
		}
		_, _ = os.Stdout.Write(data)
		os.Exit(0)
	}
	if mode != "runner" {
		os.Exit(9)
	}
	if err := os.WriteFile("fixture-child.txt", []byte("real subprocess, no model\n"), 0600); err != nil {
		os.Exit(10)
	}
	data, err := json.Marshal(os.Args[1:])
	if err != nil {
		os.Exit(11)
	}
	_, _ = os.Stdout.Write(data)
	os.Exit(0)
}

type fixtureRunner struct {
	calls   int
	fail    int
	budgets []float64
	cancel  context.CancelFunc
}

func (r *fixtureRunner) Run(ctx context.Context, dir string, spec Spec, budget float64) (Check, error) {
	r.calls++
	r.budgets = append(r.budgets, budget)
	if r.cancel != nil {
		if err := os.WriteFile(filepath.Join(dir, "partial.txt"), []byte("partial work\n"), 0600); err != nil {
			return Check{ExitCode: -1}, err
		}
		r.cancel()
		return Check{ExitCode: -1}, ctx.Err()
	}
	if err := os.WriteFile(filepath.Join(dir, "new.txt"), []byte("maintenance patch\n"), 0600); err != nil {
		return Check{ExitCode: -1}, err
	}
	if r.calls <= r.fail {
		return Check{Command: []string{"fixture"}, ExitCode: 7, Output: "fixture failed"}, errors.New("fixture failure")
	}
	return Check{Command: []string{"fixture"}, ExitCode: 0, Output: "fixture complete"}, nil
}

func gitProject(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	commands := [][]string{{"git", "init", dir}, {"git", "-C", dir, "-c", "user.name=Fixture", "-c", "user.email=fixture@example.invalid", "commit", "--allow-empty", "-m", "fixture"}}
	for _, argv := range commands {
		check, err := runCommand(context.Background(), dir, nil, argv)
		if err != nil || check.ExitCode != 0 {
			t.Fatalf("%v: %s %v", argv, check.Output, err)
		}
	}
	return dir
}

func TestIsolatedRunRetryBudgetPatchAndReview(t *testing.T) {
	project := gitProject(t)
	store, err := Open(t.TempDir(), project)
	if err != nil {
		t.Fatal(err)
	}
	spec := testSpec()
	spec.Retries = 1
	spec.Verify = [][]string{{"git", "status", "--short"}}
	if err := store.Add(spec, time.Now()); err != nil {
		t.Fatal(err)
	}
	runner := &fixtureRunner{fail: 1}
	executor := Executor{Store: store, Runner: runner}
	result, err := executor.Run(context.Background(), spec.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	if result.State != "succeeded" || result.Attempts != 2 || len(result.Checks) != 3 || result.Checks[0].ExitCode != 7 || result.Checks[2].ExitCode != 0 || !strings.Contains(result.Patch, "+maintenance patch") || !strings.Contains(result.Input, "committed HEAD only") {
		t.Fatalf("result = %+v", result)
	}
	if runner.budgets[0] != .5 || runner.budgets[1] != .5 {
		t.Fatalf("budgets = %v", runner.budgets)
	}
	if _, err := os.Stat(filepath.Join(project, "new.txt")); !os.IsNotExist(err) {
		t.Fatal("original workspace changed")
	}
	if err := store.Review(result.ID, "accepted"); err != nil {
		t.Fatal(err)
	}
	state, err := store.Read()
	if err != nil || state.Results[0].Review != "accepted" || state.Results[0].Patch != result.Patch {
		t.Fatalf("persisted review = %+v %v", state, err)
	}
	if _, err := os.Stat(filepath.Join(project, "new.txt")); !os.IsNotExist(err) {
		t.Fatal("review applied patch")
	}
}

func TestNoGitRefusesWithoutRunnerOrRootWrites(t *testing.T) {
	store := testStore(t)
	if err := store.Add(testSpec(), time.Now()); err != nil {
		t.Fatal(err)
	}
	runner := &fixtureRunner{}
	executor := Executor{Store: store, Runner: runner}
	result, err := executor.Run(context.Background(), "maintenance", "")
	if err == nil || !strings.Contains(err.Error(), "requires a Git repository") || result.State != "failed" || runner.calls != 0 {
		t.Fatalf("result=%+v calls=%d err=%v", result, runner.calls, err)
	}
	entries, err := os.ReadDir(store.project)
	if err != nil || len(entries) != 0 {
		t.Fatalf("original changed: %v %v", entries, err)
	}
}

func TestExpiredClaimUncertainNotRepeated(t *testing.T) {
	store := testStore(t)
	if err := store.Add(testSpec(), time.Now()); err != nil {
		t.Fatal(err)
	}
	_, claimed, err := store.claim("maintenance", "manual", "", "", time.Now().Add(-3*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Recover(time.Now()); err != nil {
		t.Fatal(err)
	}
	_, _, err = store.claim("maintenance", "manual", "", "", time.Now())
	if !errors.Is(err, ErrBusy) {
		t.Fatalf("uncertain was retried: %v", err)
	}
	state, err := store.Read()
	if err != nil || len(state.Results) != 1 || state.Results[0].State != "uncertain" {
		t.Fatalf("state = %+v %v", state, err)
	}
	if err := store.Review(claimed.ID, "rejected"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.claim("maintenance", "manual", "", "", time.Now()); err != nil {
		t.Fatal(err)
	}
}

func TestScheduledAndEventPathsDeduplicate(t *testing.T) {
	project := gitProject(t)
	store, err := Open(t.TempDir(), project)
	if err != nil {
		t.Fatal(err)
	}
	spec := testSpec()
	spec.EverySeconds, spec.Event = 60, "tests-failed"
	now := time.Now().UTC()
	if err := store.Add(spec, now.Add(-2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	runner := &fixtureRunner{}
	executor := Executor{Store: store, Runner: runner}
	results, err := executor.RunDue(context.Background(), now, "")
	if err != nil || len(results) != 1 || results[0].Trigger != "schedule" || results[0].State != "succeeded" {
		t.Fatalf("due=%+v err=%v", results, err)
	}
	results, err = executor.RunDue(context.Background(), now, "")
	if err != nil || len(results) != 0 || runner.calls != 1 {
		t.Fatalf("schedule duplicated: %+v %v calls=%d", results, err, runner.calls)
	}
	results, err = executor.Trigger(context.Background(), "tests-failed", "build-42", "")
	if err != nil || len(results) != 1 || results[0].EventKey != "build-42" {
		t.Fatalf("event=%+v err=%v", results, err)
	}
	results, err = executor.Trigger(context.Background(), "tests-failed", "build-42", "")
	if err == nil || len(results) != 0 || runner.calls != 2 {
		t.Fatalf("event duplicated: %+v %v calls=%d", results, err, runner.calls)
	}
}

func TestCancellationKeepsPatchAndDoesNotRetry(t *testing.T) {
	project := gitProject(t)
	store, err := Open(t.TempDir(), project)
	if err != nil {
		t.Fatal(err)
	}
	spec := testSpec()
	spec.Retries = 2
	if err := store.Add(spec, time.Now()); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runner := &fixtureRunner{cancel: cancel}
	executor := Executor{Store: store, Runner: runner}
	result, err := executor.Run(ctx, spec.ID, "")
	if !errors.Is(err, context.Canceled) || result.State != "interrupted" || result.Attempts != 1 || !strings.Contains(result.Patch, "+partial work") {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	state, err := store.Read()
	if err != nil || state.Results[0].State != "interrupted" {
		t.Fatalf("state=%+v err=%v", state, err)
	}
}

func TestInheritedGitPathsCannotRedirectIsolation(t *testing.T) {
	project := gitProject(t)
	t.Setenv("GIT_WORK_TREE", t.TempDir())
	t.Setenv("GIT_DIR", filepath.Join(t.TempDir(), "missing"))
	work, _, cleanup, err := isolatedWorktree(context.Background(), project)
	if err != nil {
		t.Fatal(err)
	}
	if work == project {
		t.Fatal("not isolated")
	}
	if err := cleanup(); err != nil {
		t.Fatal(err)
	}
}

func TestCommandRunnerExecutesFakeCoveAndBoundsProcess(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	work := t.TempDir()
	runner := CommandRunner{Executable: executable, Prepare: func(dir string, budget float64, spec Spec) ([]string, func(), error) {
		if dir != work || budget != .25 || spec.MaxTurns != 3 {
			t.Fatal("runner inputs changed")
		}
		return []string{"COVE_AUTOMATION_TEST_CHILD=runner"}, nil, nil
	}}
	check, err := runner.Run(context.Background(), work, testSpec(), .25)
	if err != nil || check.ExitCode != 0 {
		t.Fatalf("check=%+v err=%v", check, err)
	}
	var args []string
	if err := json.Unmarshal([]byte(check.Output), &args); err != nil {
		t.Fatal(err)
	}
	if len(args) != 5 || args[0] != "--no-auto" || args[1] != "--max-turns" || args[2] != "3" || args[3] != "-p" || !strings.HasPrefix(args[4], "fix tests") {
		t.Fatalf("args=%v", args)
	}
	if data, err := os.ReadFile(filepath.Join(work, "fixture-child.txt")); err != nil || string(data) != "real subprocess, no model\n" {
		t.Fatalf("fixture=%q err=%v", data, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	started := time.Now()
	_, err = runCommand(ctx, work, []string{"COVE_AUTOMATION_TEST_CHILD=block"}, []string{executable})
	if err == nil || ctx.Err() != context.DeadlineExceeded || time.Since(started) > 10*time.Second {
		t.Fatalf("timeout not bounded: %v duration=%v", err, time.Since(started))
	}
}

func TestSessionScopeAndConcurrentClaim(t *testing.T) {
	store := testStore(t)
	spec := testSpec()
	spec.SessionID = "session-one"
	if err := store.Add(spec, time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.claim(spec.ID, "manual", "", "other-session", time.Now()); err == nil {
		t.Fatal("wrong session claimed")
	}
	if _, _, err := store.claim(spec.ID, "manual", "", "session-one", time.Now()); err != nil {
		t.Fatal(err)
	}
	other, err := Open(filepath.Dir(store.dir), store.project)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := other.claim(spec.ID, "manual", "", "session-one", time.Now()); !errors.Is(err, ErrBusy) {
		t.Fatalf("second store claimed same job: %v", err)
	}
	state, err := store.Read()
	if err != nil || len(state.Results) != 1 || state.Results[0].LeaseUntil.Sub(state.Results[0].Started) <= time.Duration(spec.TimeoutSeconds)*time.Second {
		t.Fatalf("lease=%+v %v", state, err)
	}
}

func TestDirtyWorkspaceUsesCommittedHEADOnly(t *testing.T) {
	project := gitProject(t)
	check, err := runCommand(context.Background(), project, nil, []string{"git", "config", "core.autocrlf", "false"})
	if err != nil || check.ExitCode != 0 {
		t.Fatalf("git newline configuration: %s %v", check.Output, err)
	}
	tracked := filepath.Join(project, "tracked.txt")
	if err := os.WriteFile(tracked, []byte("committed content\n"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, argv := range [][]string{{"git", "add", "tracked.txt"}, {"git", "-c", "user.name=Fixture", "-c", "user.email=fixture@example.invalid", "commit", "-m", "tracked"}} {
		check, err := runCommand(context.Background(), project, nil, argv)
		if err != nil || check.ExitCode != 0 {
			t.Fatalf("git fixture: %s %v", check.Output, err)
		}
	}
	head, err := runCommand(context.Background(), project, nil, []string{"git", "rev-parse", "HEAD"})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(tracked, []byte("user uncommitted content\n"), 0600); err != nil {
		t.Fatal(err)
	}
	check, err = runCommand(context.Background(), project, nil, []string{"git", "add", "tracked.txt"})
	if err != nil || check.ExitCode != 0 {
		t.Fatalf("stage user change: %s %v", check.Output, err)
	}
	if err := os.WriteFile(filepath.Join(project, "untracked.txt"), []byte("private workspace input\n"), 0600); err != nil {
		t.Fatal(err)
	}
	work, base, cleanup, err := isolatedWorktree(context.Background(), project)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := cleanup(); err != nil {
			t.Error(err)
		}
	})
	if base != strings.TrimSpace(head.Output) {
		t.Fatalf("base=%s want=%s", base, head.Output)
	}
	if data, err := os.ReadFile(filepath.Join(work, "tracked.txt")); err != nil || string(data) != "committed content\n" {
		t.Fatalf("dirty input copied: %q %v", data, err)
	}
	if _, err := os.Stat(filepath.Join(work, "untracked.txt")); !os.IsNotExist(err) {
		t.Fatalf("untracked input copied: %v", err)
	}
	if data, err := os.ReadFile(tracked); err != nil || string(data) != "user uncommitted content\n" {
		t.Fatalf("original changed: %q %v", data, err)
	}
}

// A spec prompt may be 64 KiB but Windows caps a command line at 32767
// characters, so a long prompt travels on the child's stdin (which `cove -p`
// appends to the prompt argument) instead of failing every attempt with
// "The filename or extension is too long".
func TestLongPromptTravelsOnStdin(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	work := t.TempDir()
	runner := CommandRunner{Executable: executable, Prepare: func(string, float64, Spec) ([]string, func(), error) {
		return []string{"COVE_AUTOMATION_TEST_CHILD=runner-stdin"}, nil, nil
	}}
	spec := testSpec()
	spec.Prompt = strings.Repeat("长提示 long prompt ", 3000) // ~60 KiB
	check, err := runner.Run(context.Background(), work, spec, .25)
	if err != nil || check.ExitCode != 0 {
		t.Fatalf("check=%+v err=%v", check, err)
	}
	var echoed struct {
		Args  []string `json:"args"`
		Stdin string   `json:"stdin"`
	}
	if err := json.Unmarshal([]byte(check.Output), &echoed); err != nil {
		t.Fatal(err)
	}
	if echoed.Stdin != spec.Prompt {
		t.Fatalf("stdin carried %d bytes, want the %d-byte prompt", len(echoed.Stdin), len(spec.Prompt))
	}
	for _, arg := range echoed.Args {
		if len(arg) > maxArgPrompt {
			t.Fatalf("prompt still on the command line (%d bytes)", len(arg))
		}
	}
	if echoed.Args[len(echoed.Args)-2] != "-p" || !strings.Contains(echoed.Args[len(echoed.Args)-1], "isolated worktree") {
		t.Fatalf("args=%v", echoed.Args)
	}
}

// A worker that crashed leaves a running claim; once its lease is over the
// result is uncertain, and /automations remove must not be blocked by it
// until some unrelated run/tick happens to convert it.
func TestRemoveConvertsExpiredClaimToUncertain(t *testing.T) {
	store := testStore(t)
	spec := testSpec()
	if err := store.Add(spec, time.Now()); err != nil {
		t.Fatal(err)
	}
	past := time.Now().UTC().Add(-3 * time.Hour)
	if _, _, err := store.claim(spec.ID, "manual", "", "", past); err != nil {
		t.Fatal(err)
	}
	if err := store.Remove(spec.ID); err != nil {
		t.Fatalf("expired claim blocked removal: %v", err)
	}
	state, err := store.Read()
	if err != nil {
		t.Fatal(err)
	}
	if len(state.Specs) != 0 || len(state.Results) != 1 || state.Results[0].State != "uncertain" {
		t.Fatalf("state=%+v", state)
	}
	// A live claim still blocks removal (the uncertain result must be
	// reviewed before the task may be claimed again, as documented).
	if err := store.Review(state.Results[0].ID, "rejected"); err != nil {
		t.Fatal(err)
	}
	if err := store.Add(spec, time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.claim(spec.ID, "manual", "", "", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if err := store.Remove(spec.ID); err == nil {
		t.Fatal("removed an automation with a live running claim")
	}
}
