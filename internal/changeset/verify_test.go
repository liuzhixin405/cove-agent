package changeset

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/liuzhixin405/cove-agent/internal/workspace"
)

func TestVerifyOrderAndFailures(t *testing.T) {
	ctx := context.Background()
	root := fixture(t)
	baseline, err := workspace.Capture(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	patch := candidatePatch(t, baseline, map[string][]byte{"value.txt": []byte("good\n")})
	valid := Changeset{
		ID: "c1", Kind: KindRace, Baseline: baseline,
		PatchHash: workspace.Hash(patch), PatchBytes: len(patch),
		Checks:     []Check{{Attempt: 1, Status: "passed"}},
		Applicable: true,
	}
	assertUntouched := func(t *testing.T) {
		t.Helper()
		data, err := os.ReadFile(filepath.Join(root, "value.txt"))
		if err != nil || string(data) != "base\n" {
			t.Fatalf("project modified before all checks passed: %q %v", data, err)
		}
	}

	// 1. Applicability.
	cs := valid
	cs.Applicable, cs.NotApplicable = false, "legacy result without base commit"
	err = Verify(ctx, root, cs, patch)
	if !errors.Is(err, ErrNotApplicable) || !strings.Contains(err.Error(), cs.NotApplicable) {
		t.Fatalf("not applicable: %v", err)
	}
	assertUntouched(t)

	// 2. Clean tree and baseline drift.
	untracked := filepath.Join(root, "untracked.txt")
	if err := os.WriteFile(untracked, []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := Verify(ctx, root, valid, patch); err == nil || !strings.Contains(err.Error(), "clean") || errors.Is(err, ErrBaselineDrift) {
		t.Fatalf("dirty tree: %v", err)
	}
	if err := os.Remove(untracked); err != nil {
		t.Fatal(err)
	}
	git(t, root, "checkout", "-q", "-b", "other")
	if err := Verify(ctx, root, valid, patch); !errors.Is(err, ErrBaselineDrift) {
		t.Fatalf("branch drift: %v", err)
	}
	git(t, root, "checkout", "-q", "main")
	cs = valid
	cs.Baseline.Commit = strings.Repeat("0", 40)
	if err := Verify(ctx, root, cs, patch); !errors.Is(err, ErrBaselineDrift) {
		t.Fatalf("commit drift: %v", err)
	}
	assertUntouched(t)

	// 3. Verification of the last attempt only.
	cs = valid
	cs.Checks = nil
	if err := Verify(ctx, root, cs, patch); !errors.Is(err, ErrNoChecks) {
		t.Fatalf("no checks: %v", err)
	}
	cs.Checks = []Check{{Attempt: 2, Status: "failed", ExitCode: 1}}
	if err := Verify(ctx, root, cs, patch); !errors.Is(err, ErrCheckFailed) {
		t.Fatalf("failed check: %v", err)
	}
	cs.Checks = []Check{{Attempt: 1, Status: "passed", ExitCode: 3}}
	if err := Verify(ctx, root, cs, patch); !errors.Is(err, ErrCheckFailed) {
		t.Fatalf("passed status with non-zero exit: %v", err)
	}
	// Retry history: attempt 1 failed, attempt 2 passed -> the check step
	// passes, so the next failure must come from step 4 (a corrupted patch).
	cs.Checks = []Check{{Attempt: 1, Status: "failed", ExitCode: 1}, {Attempt: 2, Status: "passed"}}
	corrupt := append([]byte(nil), patch...)
	corrupt[len(corrupt)/2] ^= 0x01
	if err := Verify(ctx, root, cs, corrupt); !errors.Is(err, ErrPatchMismatch) {
		t.Fatalf("last attempt passed should reach the patch step: %v", err)
	}
	assertUntouched(t)

	// 4. Patch bytes and hash.
	if err := Verify(ctx, root, valid, corrupt); !errors.Is(err, ErrPatchMismatch) {
		t.Fatalf("hash mismatch: %v", err)
	}
	cs = valid
	cs.PatchBytes--
	if err := Verify(ctx, root, cs, patch); !errors.Is(err, ErrPatchMismatch) {
		t.Fatalf("size mismatch: %v", err)
	}
	cs = valid
	cs.PatchHash = ""
	if err := Verify(ctx, root, cs, patch); !errors.Is(err, ErrPatchMismatch) {
		t.Fatalf("empty hash: %v", err)
	}
	assertUntouched(t)

	// 6. Dirty tree together with failed checks: the baseline step comes first.
	if err := os.WriteFile(untracked, []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	cs = valid
	cs.Checks = []Check{{Attempt: 1, Status: "failed", ExitCode: 1}}
	if err := Verify(ctx, root, cs, patch); err == nil || !strings.Contains(err.Error(), "clean") || errors.Is(err, ErrCheckFailed) {
		t.Fatalf("dirty tree must be reported before failed checks: %v", err)
	}
	if err := os.Remove(untracked); err != nil {
		t.Fatal(err)
	}

	// 5. Everything valid: the patch is applied.
	if err := Verify(ctx, root, valid, patch); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(root, "value.txt"))
	if err != nil || string(data) != "good\n" {
		t.Fatalf("value.txt = %q, %v", data, err)
	}
}

func TestSameBaseline(t *testing.T) {
	root := filepath.Join(t.TempDir(), "Project")
	a := workspace.Baseline{Root: root, Commit: "abc", Branch: "refs/heads/main"}
	b := a
	b.Root = root + string(filepath.Separator)
	if !sameBaseline(a, b) {
		t.Fatal("trailing separator should not count as drift")
	}
	b = a
	b.Root = strings.ToLower(root)
	if got, want := sameBaseline(a, b), runtime.GOOS == "windows"; got != want {
		t.Fatalf("case-different root: got %v want %v", got, want)
	}
	b = a
	b.Commit = "ABC"
	if sameBaseline(a, b) {
		t.Fatal("commit must compare exactly")
	}
	b = a
	b.Branch = "refs/heads/Main"
	if sameBaseline(a, b) {
		t.Fatal("branch must compare exactly")
	}
}
