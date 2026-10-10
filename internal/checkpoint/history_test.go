package checkpoint

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// isolatedGit runs a test against the package's shadow store, set up once by
// TestMain under a fresh HOME with every git config hidden, so no user
// identity is available — the state of a machine where the user never ran
// `git config --global user.name`. Each project has its own ref and index
// file in the store. Creating the store is a "git init" per test otherwise.
//
// The tests stay sequential: git processes sharing one store concurrently
// fail intermittently on Windows (file locks).
func isolatedGit(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skipf("git not available: %v", err)
	}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestCreateWorksWithoutGitIdentity(t *testing.T) {
	isolatedGit(t)
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "a.txt"), "a")
	mgr, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	if hash, err := mgr.Create("first"); err != nil || hash == "" {
		t.Fatalf("Create = %q, %v; want a checkpoint", hash, err)
	}
}

func TestHistoryIsPerProject(t *testing.T) {
	isolatedGit(t)
	dirA, dirB := t.TempDir(), t.TempDir()
	writeFile(t, filepath.Join(dirA, "a.txt"), "a")
	writeFile(t, filepath.Join(dirB, "b.txt"), "b")
	mgrA, _ := New(dirA)
	mgrB, _ := New(dirB)

	if _, err := mgrA.Create("a1"); err != nil {
		t.Fatal(err)
	}
	hashB, err := mgrB.Create("b1")
	if err != nil {
		t.Fatal(err)
	}

	listA := mgrA.List()
	if len(listA) != 1 || !strings.Contains(listA[0], "a1") {
		t.Fatalf("project A history = %q, want only its own checkpoint", listA)
	}

	// A checkpoint of project B must never be written into project A.
	if _, err := mgrA.Restore(hashB); err == nil {
		t.Fatal("Restore accepted a checkpoint from another project")
	}
	if _, err := os.Stat(filepath.Join(dirA, "b.txt")); err == nil {
		t.Fatal("project B's file was written into project A")
	}
}

func TestUnchangedTreeDoesNotAddCheckpoint(t *testing.T) {
	isolatedGit(t)
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "a.txt"), "a")
	mgr, _ := New(dir)
	h1, _ := mgr.Create("one")
	h2, _ := mgr.Create("two")
	if h1 != h2 || len(mgr.List()) != 1 {
		t.Fatalf("second Create of an unchanged tree = %q (first %q), list %q", h2, h1, mgr.List())
	}
}

func TestUndoRemovesNewFilesAndKeepsABackup(t *testing.T) {
	isolatedGit(t)
	dir := t.TempDir()
	orig := filepath.Join(dir, "main.go")
	created := filepath.Join(dir, "new.go")
	writeFile(t, orig, "v1")
	mgr, _ := New(dir)
	if _, err := mgr.Create("before write"); err != nil {
		t.Fatal(err)
	}

	// The model edits one file and creates another; no checkpoint follows.
	writeFile(t, orig, "v2")
	writeFile(t, created, "new")

	backup, err := mgr.Restore("")
	if err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, orig); got != "v1" {
		t.Fatalf("main.go = %q after undo, want v1", got)
	}
	if _, err := os.Stat(created); !os.IsNotExist(err) {
		t.Fatal("file created after the checkpoint survived the undo")
	}

	// The undo itself is reversible.
	if backup == "" {
		t.Fatal("Restore returned no backup of the pre-undo state")
	}
	if _, err := mgr.Restore(backup); err != nil {
		t.Fatalf("restoring the backup: %v", err)
	}
	if readFile(t, orig) != "v2" || readFile(t, created) != "new" {
		t.Fatal("backup did not bring back the pre-undo state")
	}
}

func TestRepeatedUndoStepsBack(t *testing.T) {
	isolatedGit(t)
	dir := t.TempDir()
	f := filepath.Join(dir, "f.txt")
	writeFile(t, f, "v1")
	mgr, _ := New(dir)
	mgr.Create("c1")
	writeFile(t, f, "v2")
	mgr.Create("c2")
	writeFile(t, f, "v3")

	if _, err := mgr.Restore(""); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, f); got != "v2" {
		t.Fatalf("first undo = %q, want v2", got)
	}
	if _, err := mgr.Restore(""); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, f); got != "v1" {
		t.Fatalf("second undo = %q, want v1 (it must step back, not redo)", got)
	}
}

// A checkpoint taken in an empty project (before its first file was written)
// has an empty tree. Restoring to it must delete the files written since;
// it used to fail with "pathspec '.' did not match any file(s) known to
// git", because `checkout <hash> -- .` has nothing to check out.
func TestUndoToEmptyTreeRemovesEverything(t *testing.T) {
	isolatedGit(t)
	dir := t.TempDir()
	mgr, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := mgr.Create("empty"); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "first.txt")
	writeFile(t, path, "v1")
	backup, err := mgr.Restore("")
	if err != nil {
		t.Fatalf("restore to the empty checkpoint: %v (backup %s)", err, backup)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("first.txt survived the restore: %v", err)
	}
	if backup == "" {
		t.Fatal("no backup of the pre-undo state")
	}
}
