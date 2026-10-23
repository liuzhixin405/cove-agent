package changeset

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func lockFiles(t *testing.T, dir string) []string {
	t.Helper()
	m, err := filepath.Glob(filepath.Join(dir, "changesets", "*.lock"))
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func waitNoLockFiles(t *testing.T, dir string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for len(lockFiles(t, dir)) > 0 {
		if time.Now().After(deadline) {
			t.Fatalf("lock file still present: %v", lockFiles(t, dir))
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestWithProjectLockSerialisesAndReleases(t *testing.T) {
	dir := t.TempDir()
	project := t.TempDir()
	entered := make(chan struct{})
	release := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- WithProjectLock(dir, project, func() error {
			close(entered)
			<-release
			return nil
		})
	}()
	<-entered
	if n := len(lockFiles(t, dir)); n != 1 {
		t.Fatalf("lock files while held = %d, want 1", n)
	}
	ran := false
	if err := WithProjectLock(dir, project, func() error { ran = true; return nil }); !errors.Is(err, ErrLocked) {
		t.Fatalf("second holder: %v", err)
	}
	if ran {
		t.Fatal("fn ran without the lock")
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatalf("first holder: %v", err)
	}
	waitNoLockFiles(t, dir)
	if err := WithProjectLock(dir, project, func() error { return nil }); err != nil {
		t.Fatalf("after release: %v", err)
	}
}

func TestWithProjectLockIsPerProject(t *testing.T) {
	dir := t.TempDir()
	a, b := t.TempDir(), t.TempDir()
	entered := make(chan struct{})
	release := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- WithProjectLock(dir, a, func() error {
			close(entered)
			<-release
			return nil
		})
	}()
	<-entered
	ran := false
	if err := WithProjectLock(dir, b, func() error { ran = true; return nil }); err != nil || !ran {
		t.Fatalf("other project: err=%v ran=%v", err, ran)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestWithProjectLockPropagatesFnError(t *testing.T) {
	dir := t.TempDir()
	project := t.TempDir()
	sentinel := errors.New("boom")
	if err := WithProjectLock(dir, project, func() error { return sentinel }); err != sentinel {
		t.Fatalf("err = %v, want sentinel unchanged", err)
	}
	waitNoLockFiles(t, dir)
	if err := WithProjectLock(dir, project, func() error { return nil }); err != nil {
		t.Fatalf("lock not released: %v", err)
	}
}

func TestWithProjectLockHeldFileReturnsErrLocked(t *testing.T) {
	// Another process holds the lock file: no in-process mutex involved.
	dir := t.TempDir()
	project := t.TempDir()
	path := projectLockPath(dir, project)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("pid 1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	ran := false
	if err := WithProjectLock(dir, project, func() error { ran = true; return nil }); !errors.Is(err, ErrLocked) || ran {
		t.Fatalf("err=%v ran=%v, want ErrLocked", err, ran)
	}
	// The in-process mutex must have been released on that path.
	_ = os.Remove(path)
	if err := WithProjectLock(dir, project, func() error { return nil }); err != nil {
		t.Fatalf("mutex leaked: %v", err)
	}
}
