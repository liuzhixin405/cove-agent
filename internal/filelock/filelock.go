// Package filelock is a small cross-process advisory lock built on a lock
// file created with O_CREATE|O_EXCL. It exists for the memory directory:
// memory.LockWrites is a sync.Mutex, so it only orders writers inside one
// process, while a detached dream worker and the interactive process's
// turn-end extraction write the same memory files from two processes. Their
// "read, check, rename" sequences interleaved, and a fact one of them had just
// appended could be replaced by the other's older copy.
//
// The lock is advisory: only code that takes it is excluded. It holds a PID
// and a timestamp for diagnostics; a lock older than the stale age is judged
// left behind by a crashed holder and taken over.
package filelock

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// MemoryLockName is the lock file every writer of a memory directory takes
// (dream's write/edit/stale marker, turn-end extraction). It is a dot file,
// so the memory store and the extraction prompt skip it like the
// consolidation lock.
const MemoryLockName = ".memory-write.lock"

const (
	// DefaultWait is how long Acquire retries before giving up. A memory
	// write holds the lock for milliseconds, so waiting longer than this
	// means the holder is stuck or crashed.
	DefaultWait = 2 * time.Second
	// DefaultStale is the age past which a lock is taken over.
	DefaultStale = 30 * time.Second
)

// errDeniedTransient marks an access-denied create, which on Windows is
// usually a lock file still being deleted; Acquire retries it.
var errDeniedTransient = errors.New("filelock: lock file busy")

// ErrTimeout is returned when the lock stayed held for the whole wait.
var ErrTimeout = errors.New("filelock: lock is held by another writer")

// retry is the backoff between attempts: short at first, since holders are
// quick, then capped.
func retry(attempt int) time.Duration {
	d := time.Duration(5<<min(attempt, 4)) * time.Millisecond // 5ms .. 80ms
	return d
}

// MemoryDir takes the memory write lock of the memory directory dir, with
// the default wait and stale age.
func MemoryDir(dir string) (release func(), err error) {
	return Acquire(filepath.Join(dir, MemoryLockName), DefaultWait, DefaultStale)
}

// Acquire creates the lock file at path, retrying for up to wait while
// another holder has it. A lock whose mtime is older than stale is removed
// and the create retried. release removes the lock if it is still this
// holder's; it is safe to call more than once.
func Acquire(path string, wait, stale time.Duration) (release func(), err error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	token := newToken()
	deadline := time.Now().Add(wait)
	var lastDenied error
	for attempt := 0; ; attempt++ {
		ok, err := tryCreate(path, token)
		if errors.Is(err, errDeniedTransient) {
			// Retried like a held lock; reported as the real error only if
			// it outlasts the wait (a directory we may not write to).
			lastDenied, ok, err = err, false, nil
		}
		if err != nil {
			return nil, err
		}
		if ok {
			done := false
			return func() {
				if done {
					return
				}
				done = true
				releaseIfOurs(path, token)
			}, nil
		}
		if breakStale(path, stale) {
			// The stale lock is gone: take it now instead of sleeping (or,
			// with wait 0, reporting a timeout the caller must retry past).
			continue
		}
		if !time.Now().Before(deadline) {
			if lastDenied != nil {
				return nil, lastDenied
			}
			return nil, fmt.Errorf("%w: %s", ErrTimeout, path)
		}
		time.Sleep(min(retry(attempt), time.Until(deadline)+time.Millisecond))
	}
}

// tryCreate makes one O_EXCL attempt; ok is false when the lock exists.
func tryCreate(path, token string) (ok bool, err error) {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		if os.IsExist(err) {
			return false, nil
		}
		// On Windows a lock file that is being deleted (delete pending while
		// another holder still has it open) fails the create with access
		// denied rather than "exists". It used to be treated as held only if
		// a following stat still saw the file, but the delete can complete
		// between the two calls: the stat then said "not found" and the
		// access-denied error went back to the caller, failing a writer that
		// only had to try again.
		if os.IsPermission(err) {
			return false, fmt.Errorf("%w: %w", errDeniedTransient, err)
		}
		return false, err
	}
	_, werr := fmt.Fprintf(f, "pid %d\ntime %s\ntoken %s\n", os.Getpid(), time.Now().UTC().Format(time.RFC3339Nano), token)
	cerr := f.Close()
	if err := errors.Join(werr, cerr); err != nil {
		_ = os.Remove(path)
		return false, err
	}
	return true, nil
}

// beforeStaleRename, when set, runs between judging a lock stale and moving
// it; tests swap in a fresh lock there.
var beforeStaleRename func()

// breakStale removes the lock at path when it is older than stale. The file
// is first renamed to a unique name, so of several processes that all judged
// it stale only one moves it; the moved file is then checked to still be the
// stale one, and a fresh one is put back with a hard link, which fails
// instead of replacing a lock created meanwhile.
//
// Between the stat and the rename the stale holder can release and a new
// holder create a fresh lock, which the rename then moves. Whether the moved
// file was that fresh lock used to be judged by its mtime: a fresh lock was
// discarded when it looked old enough (two writers then held the lock), or
// linked back after its owner had already released it (a lock nobody
// removes: every writer timed out for the stale age). The lock's body, with
// its unique token, is now read while the lock is being judged stale, and
// only a moved file with that same body is discarded; any other body is a
// fresh lock and is put back. A body that cannot be read gives no token to
// compare with, so the lock is not stale.
//
// The body is read only once a stat says the lock is stale, and a second
// stat confirms the file read is the one judged stale: on Windows a file
// open for reading cannot be removed by its holder (Go opens without
// FILE_SHARE_DELETE), so contenders reading the lock on every retry made the
// holder's release fail and the lock stay until the stale age.
func breakStale(path string, stale time.Duration) (removed bool) {
	info, err := os.Stat(path)
	if err != nil || time.Since(info.ModTime()) < stale {
		return false
	}
	body, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	again, err := os.Stat(path)
	if err != nil || time.Since(again.ModTime()) < stale || !again.ModTime().Equal(info.ModTime()) || again.Size() != info.Size() {
		return false // replaced between the stat and the read: not the file judged stale
	}
	if beforeStaleRename != nil {
		beforeStaleRename()
	}
	moved := fmt.Sprintf("%s.stale-%s", path, newToken())
	if err := os.Rename(path, moved); err != nil {
		return false // someone else broke it (or re-created it) first
	}
	if mb, err := os.ReadFile(moved); err != nil || string(mb) != string(body) {
		_ = os.Link(moved, path)
		_ = os.Remove(moved)
		return false
	}
	_ = os.Remove(moved)
	return true
}

// releaseIfOurs removes the lock when it still carries token: a lock taken
// over as stale (this holder ran past the stale age) belongs to someone else
// now and must stay.
func releaseIfOurs(path, token string) {
	data, err := os.ReadFile(path)
	if err != nil || !strings.Contains(string(data), "token "+token+"\n") {
		return
	}
	_ = os.Remove(path)
}

func newToken() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("%d-%d", os.Getpid(), time.Now().UnixNano())
	}
	return hex.EncodeToString(b[:])
}
