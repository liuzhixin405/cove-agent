package filelock

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeLock(t *testing.T, path, token string, mtime time.Time) {
	t.Helper()
	if err := os.WriteFile(path, []byte("pid 1\ntoken "+token+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, mtime, mtime); err != nil {
		t.Fatal(err)
	}
}

// Between the stat that judged a lock stale and the rename that removes it,
// the stale holder can release and a new holder create a fresh lock. The
// rename then moved the fresh lock: it was discarded (two writers held the
// lock) or, when its age was checked, linked back after its owner had
// released (a lock nobody removes: 30s of timeouts). The lock's token is now
// read before it is judged stale and compared with the moved file's: a
// different token is a fresh lock, which is put back untouched.
func TestBreakStalePutsBackFreshLockCreatedMeanwhile(t *testing.T) {
	path := filepath.Join(t.TempDir(), MemoryLockName)
	writeLock(t, path, "stale", time.Now().Add(-time.Hour))
	swapped := false
	beforeStaleRename = func() {
		// The stale holder releases and a new holder takes the lock.
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
		writeLock(t, path, "fresh", time.Now())
		swapped = true
	}
	t.Cleanup(func() { beforeStaleRename = nil })

	breakStale(path, 30*time.Second)

	if !swapped {
		t.Fatal("hook did not run")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("the fresh lock was discarded: %v", err)
	}
	if !strings.Contains(string(data), "token fresh\n") {
		t.Fatalf("lock body = %q, want the fresh holder's", data)
	}
	if m, _ := filepath.Glob(path + ".stale-*"); len(m) != 0 {
		t.Fatalf("moved lock left behind: %v", m)
	}
	// The fresh holder still owns it: its release removes it.
	releaseIfOurs(path, "fresh")
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("fresh holder could not release its lock (stat err %v)", err)
	}
}

// A fresh lock whose mtime is old (a clock that jumped, or a holder that
// touched the file) was linked back only by age; the token tells it apart.
func TestBreakStalePutsBackFreshLockEvenIfItLooksOld(t *testing.T) {
	path := filepath.Join(t.TempDir(), MemoryLockName)
	writeLock(t, path, "stale", time.Now().Add(-time.Hour))
	beforeStaleRename = func() {
		_ = os.Remove(path)
		writeLock(t, path, "fresh", time.Now().Add(-time.Hour))
	}
	t.Cleanup(func() { beforeStaleRename = nil })

	breakStale(path, 30*time.Second)

	if data, err := os.ReadFile(path); err != nil || !strings.Contains(string(data), "token fresh\n") {
		t.Fatalf("lock = %q, %v; want the fresh holder's lock put back", data, err)
	}
}

// The stale lock itself is still removed.
func TestBreakStaleRemovesTheStaleLock(t *testing.T) {
	path := filepath.Join(t.TempDir(), MemoryLockName)
	writeLock(t, path, "stale", time.Now().Add(-time.Hour))
	breakStale(path, 30*time.Second)
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("stale lock not removed (stat err %v)", err)
	}
	if m, _ := filepath.Glob(path + ".stale-*"); len(m) != 0 {
		t.Fatalf("moved lock left behind: %v", m)
	}
}

// A lock whose body cannot be read gives no token to compare, so it is not
// judged stale (a directory at the lock path stands in for an unreadable
// file).
func TestBreakStaleLeavesUnreadableLock(t *testing.T) {
	path := filepath.Join(t.TempDir(), MemoryLockName)
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-time.Hour)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}
	breakStale(path, 30*time.Second)
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("unreadable lock was moved away: %v", err)
	}
	if m, _ := filepath.Glob(path + ".stale-*"); len(m) != 0 {
		t.Fatalf("moved: %v", m)
	}
}

// Acquire with no wait (the automation job lock) used to break a stale lock
// and then report a timeout in the same call: the first run/tick after a
// worker crash always failed and only the next one got the lock.
func TestAcquireTakesTheLockItJustBrokeWithoutWaiting(t *testing.T) {
	path := filepath.Join(t.TempDir(), "job.lock")
	writeLock(t, path, "crashed", time.Now().Add(-3*time.Hour))
	release, err := Acquire(path, 0, 2*time.Hour)
	if err != nil {
		t.Fatalf("Acquire after a stale lock: %v", err)
	}
	defer release()
	data, err := os.ReadFile(path)
	if err != nil || strings.Contains(string(data), "token crashed\n") {
		t.Fatalf("lock not taken over: %q %v", data, err)
	}
}
