package changeset

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/liuzhixin405/cove-agent/internal/filelock"
)

// ErrLocked is returned by WithProjectLock when another apply holds the project.
var ErrLocked = errors.New("another apply is in progress for this project")

// lockStale is the age past which a lock file is judged left by a crashed holder.
const lockStale = 10 * time.Minute

// projectMutexes maps a canonical project path to its in-process *sync.Mutex.
var projectMutexes sync.Map

// WithProjectLock serialises apply across sources for one project: a
// process-wide mutex keyed by the canonical project path plus the exclusive
// file <configDir>/changesets/<sha256(project)[:16]>.lock. fn runs with both
// held. A held lock returns ErrLocked without waiting; fn's error is returned
// unchanged.
func WithProjectLock(configDir, project string, fn func() error) error {
	key := canonicalProject(project)
	m, _ := projectMutexes.LoadOrStore(key, new(sync.Mutex))
	mu := m.(*sync.Mutex)
	if !mu.TryLock() {
		return ErrLocked
	}
	defer mu.Unlock()

	release, err := filelock.Acquire(lockPathForKey(configDir, key), 0, lockStale)
	if err != nil {
		if errors.Is(err, filelock.ErrTimeout) {
			return ErrLocked
		}
		return err
	}
	defer release()
	return fn()
}

// canonicalProject resolves project to an absolute, symlink-free path,
// lower-cased on Windows. A path that cannot be resolved falls back to the
// cleaned absolute form.
func canonicalProject(project string) string {
	p := project
	if abs, err := filepath.Abs(p); err == nil {
		p = abs
	}
	if real, err := filepath.EvalSymlinks(p); err == nil {
		p = real
	}
	if runtime.GOOS == "windows" {
		p = strings.ToLower(p)
	}
	return p
}

func lockPathForKey(configDir, key string) string {
	sum := sha256.Sum256([]byte(key))
	return filepath.Join(configDir, "changesets", hex.EncodeToString(sum[:])[:16]+".lock")
}

// projectLockPath is the lock file WithProjectLock uses for project.
func projectLockPath(configDir, project string) string {
	return lockPathForKey(configDir, canonicalProject(project))
}
