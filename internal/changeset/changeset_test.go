package changeset

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/liuzhixin405/cove-agent/internal/workspace"
)

// fixture builds a clean repository on branch main with value.txt = "base\n"
// (same shape as internal/race's fixture).
func fixture(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	git(t, root, "init", "-b", "main")
	git(t, root, "config", "core.autocrlf", "false")
	if err := os.WriteFile(filepath.Join(root, "value.txt"), []byte("base\n"), 0600); err != nil {
		t.Fatal(err)
	}
	git(t, root, "add", ".")
	git(t, root, "-c", "user.name=fixture", "-c", "user.email=fixture@example.invalid", "commit", "-m", "fixture")
	return root
}

func git(t *testing.T, dir string, args ...string) {
	t.Helper()
	if _, err := workspace.Git(context.Background(), dir, nil, args...); err != nil {
		t.Fatal(err)
	}
}

// candidatePatch checks out the baseline into a temporary worktree, writes
// files (name -> content) there and returns the resulting patch.
func candidatePatch(t *testing.T, baseline workspace.Baseline, files map[string][]byte) []byte {
	t.Helper()
	ctx := context.Background()
	work := filepath.Join(t.TempDir(), "work")
	if err := baseline.Add(ctx, work); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := baseline.Remove(context.Background(), work); err != nil {
			t.Errorf("remove worktree: %v", err)
		}
	})
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(work, name), content, 0600); err != nil {
			t.Fatal(err)
		}
	}
	patch, err := workspace.Patch(ctx, work, baseline.Commit)
	if err != nil {
		t.Fatal(err)
	}
	return patch
}

func TestLastAttemptChecks(t *testing.T) {
	checks := []Check{{Attempt: 1, ExitCode: 7, Status: "failed"}, {Attempt: 2, ExitCode: 0, Status: "passed"}, {Attempt: 2, ExitCode: 0, Status: "passed"}}
	if got := LastAttemptChecks(checks); len(got) != 2 || got[0].Attempt != 2 || got[1].Attempt != 2 {
		t.Fatalf("got %+v", got)
	}
	legacy := []Check{{ExitCode: 0, Status: "passed"}, {ExitCode: 1, Status: "failed"}}
	if got := LastAttemptChecks(legacy); len(got) != 2 {
		t.Fatalf("legacy got %+v", got)
	}
	if got := LastAttemptChecks(nil); len(got) != 0 {
		t.Fatalf("nil got %+v", got)
	}
}
