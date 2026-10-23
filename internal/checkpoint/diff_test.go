package checkpoint

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func newDiffTestManager(t *testing.T) (*Manager, string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("GIT_AUTHOR_NAME", "cove test")
	t.Setenv("GIT_AUTHOR_EMAIL", "cove@example.test")
	t.Setenv("GIT_COMMITTER_NAME", "cove test")
	t.Setenv("GIT_COMMITTER_EMAIL", "cove@example.test")
	workDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(workDir, "keep.txt"), []byte("keep\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	mgr, err := New(workDir)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return mgr, workDir
}

func writeDiffFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestDiffToWorkTreeIncludesEveryChangeSinceHash(t *testing.T) {
	mgr, dir := newDiffTestManager(t)
	ctx := context.Background()

	one, err := mgr.Create("one")
	if err != nil {
		t.Fatalf("Create one: %v", err)
	}
	writeDiffFile(t, filepath.Join(dir, "a.txt"), "alpha\n")
	two, err := mgr.Create("two")
	if err != nil {
		t.Fatalf("Create two: %v", err)
	}
	if two == one {
		t.Fatal("second checkpoint should differ from the first")
	}
	writeDiffFile(t, filepath.Join(dir, "b.txt"), "beta\n")
	writeDiffFile(t, filepath.Join(dir, "node_modules", "x.js"), "ignored\n")

	fromOne, err := mgr.DiffToWorkTree(ctx, one)
	if err != nil {
		t.Fatalf("DiffToWorkTree(one): %v", err)
	}
	s := string(fromOne)
	if !strings.Contains(s, "b/a.txt") || !strings.Contains(s, "b/b.txt") {
		t.Fatalf("diff from one should hold a.txt and b.txt:\n%s", s)
	}
	if !strings.Contains(s, "+alpha") || !strings.Contains(s, "+beta") {
		t.Fatalf("diff from one should hold the added lines:\n%s", s)
	}
	if strings.Contains(s, "keep.txt") {
		t.Fatalf("unchanged file in diff:\n%s", s)
	}
	if strings.Contains(s, "node_modules") {
		t.Fatalf("excluded path in diff:\n%s", s)
	}

	fromTwo, err := mgr.DiffToWorkTree(ctx, two)
	if err != nil {
		t.Fatalf("DiffToWorkTree(two): %v", err)
	}
	s = string(fromTwo)
	if !strings.Contains(s, "b/b.txt") || strings.Contains(s, "b/a.txt") {
		t.Fatalf("diff from two should hold only b.txt:\n%s", s)
	}
	if strings.Contains(s, "node_modules") {
		t.Fatalf("excluded path in diff:\n%s", s)
	}

	// The diff leaves the per-project index as a snapshot leaves it: a Create
	// afterwards still records the new state, and one more Create with nothing
	// changed returns the same checkpoint.
	three, err := mgr.Create("three")
	if err != nil {
		t.Fatalf("Create three: %v", err)
	}
	if three == two {
		t.Fatal("Create after DiffToWorkTree did not record b.txt")
	}
	again, err := mgr.Create("again")
	if err != nil {
		t.Fatalf("Create again: %v", err)
	}
	if again != three {
		t.Fatalf("unchanged Create after diff made a new checkpoint: %s != %s", again, three)
	}
	empty, err := mgr.DiffToWorkTree(ctx, three)
	if err != nil {
		t.Fatalf("DiffToWorkTree(three): %v", err)
	}
	if len(strings.TrimSpace(string(empty))) != 0 {
		t.Fatalf("diff from the current checkpoint should be empty:\n%s", empty)
	}
	if mgr.Count() != 3 {
		t.Fatalf("Count = %d, want 3", mgr.Count())
	}
}

func TestDiffToWorkTreeRejectsUnknownOrForeignHash(t *testing.T) {
	mgr, _ := newDiffTestManager(t)
	ctx := context.Background()
	one, err := mgr.Create("one")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := mgr.DiffToWorkTree(ctx, ""); err == nil {
		t.Fatal("empty hash accepted")
	}
	if _, err := mgr.DiffToWorkTree(ctx, "0123456789abcdef0123456789abcdef01234567"); err == nil {
		t.Fatal("unknown hash accepted")
	}
	// A checkpoint of another project in the same shared store.
	other, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	writeDiffFile(t, filepath.Join(other.WorkDir(), "o.txt"), "o\n")
	foreign, err := other.Create("other")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := mgr.DiffToWorkTree(ctx, foreign); err == nil {
		t.Fatal("another project's checkpoint accepted")
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := mgr.DiffToWorkTree(cancelled, one); err == nil {
		t.Fatal("cancelled context did not stop the diff")
	}
}

// Text files diff as text even when the project marks them binary or names a
// diff driver; a file holding NUL bytes is still a binary patch.
func TestDiffToWorkTreeTextAndBinary(t *testing.T) {
	mgr, dir := newDiffTestManager(t)
	writeDiffFile(t, filepath.Join(dir, ".gitattributes"), "*.txt binary\n*.md diff=nosuchdriver\n")
	base, err := mgr.Create("base")
	if err != nil {
		t.Fatal(err)
	}
	writeDiffFile(t, filepath.Join(dir, "a.txt"), "alpha\n")
	writeDiffFile(t, filepath.Join(dir, "n.md"), "note\n")
	writeDiffFile(t, filepath.Join(dir, "blob.bin"), "x\x00y")
	out, err := mgr.DiffToWorkTree(context.Background(), base)
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	if !strings.Contains(s, "+alpha") || !strings.Contains(s, "+note") {
		t.Fatalf("text files not diffed as text:\n%s", s)
	}
	if !strings.Contains(s, "diff --git a/blob.bin b/blob.bin") || strings.Count(s, "GIT binary patch") != 1 {
		t.Fatalf("binary file not a single binary patch:\n%s", s)
	}
}

// orphanRef points the project's ref at a parentless copy of its head, the way
// a retention trim re-creates the kept commits: every earlier hash stops being
// an ancestor of the ref.
func orphanRef(t *testing.T, mgr *Manager) {
	t.Helper()
	env := mgr.env()
	tree, err := mgr.gitOutput(env, "rev-parse", mgr.refName+"^{tree}")
	if err != nil {
		t.Fatal(err)
	}
	commit, err := mgr.gitOutput(env, "-c", "user.name=t", "-c", "user.email=t@t", "commit-tree", strings.TrimSpace(tree), "-m", "trimmed")
	if err != nil {
		t.Fatal(err)
	}
	if err := mgr.gitCmd(env, "update-ref", mgr.refName, strings.TrimSpace(commit)); err != nil {
		t.Fatal(err)
	}
}

func TestDiffToWorkTreeAcceptsTreeAfterTrim(t *testing.T) {
	mgr, dir := newDiffTestManager(t)
	ctx := context.Background()
	a, err := mgr.Create("A")
	if err != nil {
		t.Fatal(err)
	}
	treeA, err := mgr.TreeOf(a)
	if err != nil || treeA == "" {
		t.Fatalf("TreeOf(A) = %q, %v", treeA, err)
	}
	if _, err := mgr.TreeOf("0123456789abcdef0123456789abcdef01234567"); err == nil {
		t.Fatal("TreeOf accepted an unknown hash")
	}
	writeDiffFile(t, filepath.Join(dir, "after.txt"), "after\n")
	if _, err := mgr.Create("B"); err != nil {
		t.Fatal(err)
	}
	writeDiffFile(t, filepath.Join(dir, "later.txt"), "later\n")
	orphanRef(t, mgr)

	if _, err := mgr.DiffToWorkTree(ctx, a); err == nil {
		t.Fatal("a commit no longer on the ref was accepted")
	}
	out, err := mgr.DiffToWorkTree(ctx, treeA)
	if err != nil {
		t.Fatalf("DiffToWorkTree(tree A): %v", err)
	}
	if s := string(out); !strings.Contains(s, "b/after.txt") || !strings.Contains(s, "b/later.txt") {
		t.Fatalf("diff from tree A lacks the later files:\n%s", s)
	}
}

// The user's global diff and colour settings must not reshape the output.
func TestDiffToWorkTreeIgnoresUserDiffConfig(t *testing.T) {
	mgr, dir := newDiffTestManager(t)
	cfg := filepath.Join(t.TempDir(), "gitconfig")
	writeDiffFile(t, cfg, "[diff]\n\tmnemonicPrefix = true\n\tnoprefix = true\n\trenames = copies\n[color]\n\tui = always\n\tdiff = always\n")
	t.Setenv("GIT_CONFIG_GLOBAL", cfg)
	writeDiffFile(t, filepath.Join(dir, "old.txt"), "same content here\nline two\nline three\n")
	base, err := mgr.Create("base")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(dir, "old.txt"), filepath.Join(dir, "new.txt")); err != nil {
		t.Fatal(err)
	}
	out, err := mgr.DiffToWorkTree(context.Background(), base)
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	if strings.Contains(s, "\x1b[") {
		t.Fatalf("colour codes in diff:\n%q", s)
	}
	for _, want := range []string{"diff --git a/new.txt b/new.txt", "+++ b/new.txt", "diff --git a/old.txt b/old.txt", "--- a/old.txt"} {
		if !strings.Contains(s, want) {
			t.Fatalf("diff lacks %q:\n%s", want, s)
		}
	}
	if strings.Contains(s, "rename from") {
		t.Fatalf("rename detection applied:\n%s", s)
	}
}
