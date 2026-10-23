package automation

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/liuzhixin405/cove-agent/internal/changeset"
	"github.com/liuzhixin405/cove-agent/internal/filelock"
)

// appliedResult runs spec on a fresh git project with runner and returns the
// store, the project and the succeeded result.
func appliedResult(t *testing.T, spec Spec, runner *fixtureRunner) (*Store, string, Result) {
	t.Helper()
	project := gitProject(t)
	store, err := Open(t.TempDir(), project)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Add(spec, time.Now()); err != nil {
		t.Fatal(err)
	}
	executor := Executor{Store: store, Runner: runner}
	result, err := executor.Run(context.Background(), spec.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	if result.State != "succeeded" || result.PatchHash == "" {
		t.Fatalf("result = %+v", result)
	}
	return store, project, result
}

func verifyIn(project string) func(changeset.Changeset, []byte) error {
	return func(cs changeset.Changeset, patch []byte) error {
		return changeset.Verify(context.Background(), project, cs, patch)
	}
}

func verifiedSpec() Spec {
	spec := testSpec()
	spec.Verify = [][]string{{"git", "status", "--short"}}
	return spec
}

func newFileExists(t *testing.T, project string) bool {
	t.Helper()
	_, err := os.Stat(filepath.Join(project, "new.txt"))
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	return err == nil
}

func gitIn(t *testing.T, dir string, args ...string) {
	t.Helper()
	argv := append([]string{"git", "-C", dir}, args...)
	check, err := runCommand(context.Background(), dir, nil, argv)
	if err != nil || check.ExitCode != 0 {
		t.Fatalf("%v: %s %v", argv, check.Output, err)
	}
}

func TestApplyFourChecksEachRefuse(t *testing.T) {
	store, project, result := appliedResult(t, verifiedSpec(), &fixtureRunner{})
	noAfter := func() { t.Fatal("after called on a refused apply") }

	// a. dirty tree
	untracked := filepath.Join(project, "untracked.txt")
	if err := os.WriteFile(untracked, []byte("user work\n"), 0600); err != nil {
		t.Fatal(err)
	}
	err := store.Apply(context.Background(), result.ID, verifyIn(project), noAfter)
	if err == nil || !strings.Contains(err.Error(), "clean") || newFileExists(t, project) {
		t.Fatalf("dirty: err=%v", err)
	}
	if err := os.Remove(untracked); err != nil {
		t.Fatal(err)
	}

	// b. branch drift
	original := strings.TrimPrefix(result.Baseline.Branch, "refs/heads/")
	gitIn(t, project, "checkout", "-q", "-b", "other")
	err = store.Apply(context.Background(), result.ID, verifyIn(project), noAfter)
	if !errors.Is(err, changeset.ErrBaselineDrift) || newFileExists(t, project) {
		t.Fatalf("drift: err=%v", err)
	}
	gitIn(t, project, "checkout", "-q", original)

	// c. patch file altered by one byte
	path := store.PatchPath(result.ID)
	good, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	bad := append([]byte(nil), good...)
	bad[len(bad)-1] ^= 1
	if err := os.WriteFile(path, bad, 0600); err != nil {
		t.Fatal(err)
	}
	err = store.Apply(context.Background(), result.ID, verifyIn(project), noAfter)
	if !errors.Is(err, changeset.ErrPatchMismatch) || newFileExists(t, project) {
		t.Fatalf("mismatch: err=%v", err)
	}
	if err := os.WriteFile(path, good, 0600); err != nil {
		t.Fatal(err)
	}

	// d. a failing check with the same Attempt as the last one
	if err := store.update(func(state *Snapshot) error {
		for index := range state.Results {
			if state.Results[index].ID == result.ID {
				checks := state.Results[index].Checks
				last := checks[len(checks)-1]
				state.Results[index].Checks = append(checks, Check{Command: []string{"extra"}, ExitCode: 1, Attempt: last.Attempt})
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	err = store.Apply(context.Background(), result.ID, verifyIn(project), noAfter)
	if !errors.Is(err, changeset.ErrCheckFailed) || newFileExists(t, project) {
		t.Fatalf("check failed: err=%v", err)
	}
	state, err := store.Read()
	if err != nil || state.Results[0].Review == "applied" {
		t.Fatalf("refusal recorded applied: %+v %v", state.Results, err)
	}
}

func TestApplySucceedsAndRecordsApplied(t *testing.T) {
	store, project, result := appliedResult(t, verifiedSpec(), &fixtureRunner{})
	calls := 0
	if err := store.Apply(context.Background(), result.ID, verifyIn(project), func() { calls++ }); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("after calls = %d", calls)
	}
	data, err := os.ReadFile(filepath.Join(project, "new.txt"))
	// git apply may check out with core.autocrlf line endings on Windows.
	if err != nil || strings.ReplaceAll(string(data), "\r\n", "\n") != "maintenance patch\n" {
		t.Fatalf("new.txt = %q %v", data, err)
	}
	state, err := store.Read()
	if err != nil || state.Results[0].Review != "applied" || state.Results[0].ReviewedAt.IsZero() {
		t.Fatalf("state = %+v %v", state.Results, err)
	}
	cs := store.Changeset(state.Results[0])
	if cs.Applicable || cs.NotApplicable != "已应用" || cs.Review.Decision != "applied" || cs.Review.At == nil {
		t.Fatalf("changeset = %+v", cs)
	}
	// Clean the tree so the second apply is refused for being applied, not dirty.
	if err := os.Remove(filepath.Join(project, "new.txt")); err != nil {
		t.Fatal(err)
	}
	err = store.Apply(context.Background(), result.ID, verifyIn(project), func() { calls++ })
	if !errors.Is(err, changeset.ErrNotApplicable) || calls != 1 || newFileExists(t, project) {
		t.Fatalf("second apply: err=%v calls=%d", err, calls)
	}
	for _, decision := range []string{"accepted", "rejected"} {
		if err := store.Review(result.ID, decision); err == nil || !strings.Contains(err.Error(), "already applied") {
			t.Fatalf("review %s after apply: %v", decision, err)
		}
	}
}

func TestApplicableGating(t *testing.T) {
	store, _, result := appliedResult(t, verifiedSpec(), &fixtureRunner{})
	cs := store.Changeset(result)
	if !cs.Applicable || cs.NotApplicable != "" || cs.ID != "inbox:"+result.ID || cs.ID != ChangesetID(result.ID) ||
		cs.Kind != changeset.KindMaintenance || cs.Title != result.TaskID || !cs.Created.Equal(result.Started) ||
		cs.Review.Decision != "pending" || cs.Review.At != nil || cs.PatchHash != result.PatchHash ||
		cs.PatchBytes != result.PatchBytes || cs.Baseline != result.Baseline || len(cs.Checks) != len(result.Checks) {
		t.Fatalf("changeset = %+v", cs)
	}
	for index, check := range cs.Checks {
		want := "passed"
		if result.Checks[index].ExitCode != 0 {
			want = "failed"
		}
		if check.Status != want || check.Attempt != result.Checks[index].Attempt ||
			strings.Join(check.Argv, " ") != strings.Join(result.Checks[index].Command, " ") {
			t.Fatalf("check %d = %+v", index, check)
		}
	}

	for _, state := range []string{"interrupted", "failed"} {
		other := result
		other.State = state
		cs := store.Changeset(other)
		if cs.Applicable || !strings.Contains(cs.NotApplicable, "任务未成功") || !strings.Contains(cs.NotApplicable, state) {
			t.Fatalf("%s: %+v", state, cs)
		}
	}
	errored := result
	errored.Error = "something"
	if cs := store.Changeset(errored); cs.Applicable || cs.NotApplicable == "" {
		t.Fatalf("errored result applicable: %+v", cs)
	}

	if err := store.Review(result.ID, "rejected"); err != nil {
		t.Fatal(err)
	}
	state, err := store.Read()
	if err != nil {
		t.Fatal(err)
	}
	if state.Results[0].ReviewedAt.IsZero() {
		t.Fatal("rejected review has no ReviewedAt")
	}
	cs = store.Changeset(state.Results[0])
	if cs.Applicable || !strings.Contains(cs.NotApplicable, "rejected") || !strings.Contains(cs.NotApplicable, "review accepted") ||
		cs.Review.Decision != "rejected" || cs.Review.At == nil {
		t.Fatalf("rejected: %+v", cs)
	}
	if err := store.Review(result.ID, "accepted"); err != nil {
		t.Fatal(err)
	}
	state, err = store.Read()
	if err != nil {
		t.Fatal(err)
	}
	if cs := store.Changeset(state.Results[0]); !cs.Applicable || cs.Review.Decision != "accepted" {
		t.Fatalf("accepted: %+v", cs)
	}

	legacy := state.Results[0]
	legacy.Baseline.Commit = ""
	if cs := store.Changeset(legacy); cs.Applicable || !strings.Contains(cs.NotApplicable, "只能导出") || !strings.Contains(cs.NotApplicable, "支持应用之前") {
		t.Fatalf("no commit: %+v", cs)
	}
	legacy = state.Results[0]
	legacy.PatchHash = ""
	if cs := store.Changeset(legacy); cs.Applicable || !strings.Contains(cs.NotApplicable, "支持应用之前") {
		t.Fatalf("no hash: %+v", cs)
	}

	// A detached-HEAD/fallback baseline (Describe failed) is export-only.
	detached := state.Results[0]
	detached.Baseline.Branch, detached.Baseline.Root = "", ""
	if cs := store.Changeset(detached); cs.Applicable || !strings.Contains(cs.NotApplicable, "分离 HEAD") || !strings.Contains(cs.NotApplicable, "只能导出") {
		t.Fatalf("detached: %+v", cs)
	}
}

func TestApplicableWhenPatchFileMissing(t *testing.T) {
	store, project, result := appliedResult(t, verifiedSpec(), &fixtureRunner{})
	if err := os.Remove(store.PatchPath(result.ID)); err != nil {
		t.Fatal(err)
	}
	cs := store.Changeset(result)
	if cs.Applicable || !strings.Contains(cs.NotApplicable, "补丁文件缺失") {
		t.Fatalf("changeset = %+v", cs)
	}
	err := store.Apply(context.Background(), result.ID, verifyIn(project), func() { t.Fatal("after called") })
	if !errors.Is(err, changeset.ErrNotApplicable) || !strings.Contains(err.Error(), "补丁文件缺失") {
		t.Fatalf("apply: %v", err)
	}
	invalid := result
	invalid.ID = "../escape"
	if cs := store.Changeset(invalid); cs.Applicable {
		t.Fatalf("invalid id applicable: %+v", cs)
	}
	if err := store.Apply(context.Background(), "../escape", verifyIn(project), nil); err == nil {
		t.Fatal("invalid id accepted")
	}
	if err := store.Apply(context.Background(), "unknown-id", verifyIn(project), nil); err == nil || !strings.Contains(err.Error(), "unknown") {
		t.Fatalf("unknown id: %v", err)
	}
}

func TestApplyAuditFailureReportsApplied(t *testing.T) {
	store, project, result := appliedResult(t, verifiedSpec(), &fixtureRunner{})
	var release func()
	t.Cleanup(func() {
		if release != nil {
			release()
		}
	})
	// Apply's own Read has released state.lock before verify runs; taking it
	// here makes only the audit write fail.
	verify := func(cs changeset.Changeset, patch []byte) error {
		if err := changeset.Verify(context.Background(), project, cs, patch); err != nil {
			return err
		}
		var err error
		release, err = filelock.Acquire(filepath.Join(store.dir, "state.lock"), 0, time.Hour)
		return err
	}
	calls := 0
	started := time.Now()
	err := store.Apply(context.Background(), result.ID, verify, func() { calls++ })
	t.Logf("audit failure took %s", time.Since(started))
	if !errors.Is(err, ErrAuditNotSaved) || calls != 1 || !newFileExists(t, project) {
		t.Fatalf("err=%v calls=%d", err, calls)
	}
	release()
	release = nil
	state, err := store.Read()
	if err != nil || state.Results[0].Review == "applied" {
		t.Fatalf("state = %+v %v", state.Results, err)
	}
}

func TestApplyOnlyLastAttemptCounts(t *testing.T) {
	spec := verifiedSpec()
	spec.Retries = 1
	store, project, result := appliedResult(t, spec, &fixtureRunner{fail: 1})
	if result.Checks[0].ExitCode != 7 || result.Checks[0].Attempt != 1 {
		t.Fatalf("checks = %+v", result.Checks)
	}
	if cs := store.Changeset(result); !cs.Applicable || cs.Checks[0].Status != "failed" {
		t.Fatalf("changeset = %+v", cs)
	}
	if err := store.Apply(context.Background(), result.ID, verifyIn(project), nil); err != nil {
		t.Fatal(err)
	}
	if !newFileExists(t, project) {
		t.Fatal("patch not applied")
	}
}

func TestApplyWithoutVerifyCommandsIsApplicable(t *testing.T) {
	spec := testSpec()
	spec.Verify = nil
	store, project, result := appliedResult(t, spec, &fixtureRunner{})
	cs := store.Changeset(result)
	if !cs.Applicable || len(cs.Checks) != 1 || cs.Checks[0].Status != "passed" {
		t.Fatalf("changeset = %+v", cs)
	}
	if err := store.Apply(context.Background(), result.ID, verifyIn(project), nil); err != nil {
		t.Fatal(err)
	}
	if !newFileExists(t, project) {
		t.Fatal("patch not applied")
	}
}
