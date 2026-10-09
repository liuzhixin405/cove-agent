package checkpoint

import (
	"context"
	"github.com/liuzhixin405/cove-agent/internal/proctree"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/liuzhixin405/cove-agent/internal/log"
)

// Retention: each project keeps its newest keepCheckpoints checkpoints. The
// chain is trimmed once it grows pruneSlack past that (rewriting the kept
// commits onto a new root costs one git call per commit, so it is batched),
// and every gcEvery created checkpoints the shared store is garbage-collected
// so trimmed snapshots eventually leave the disk. Both run, one after the
// other, in one background goroutine at a time, so the Create that triggers
// them returns at once. Trimming rewrites the ref and holds m.mu (a Create
// waits for it); gc does not: without --prune=now it is safe next to writes,
// and it may take long, so it runs unlocked and bounded by gcTimeout.
var (
	keepCheckpoints = 50
	pruneSlack      = 10
	gcEvery         = 20
	// gcArgs deliberately has no --prune=now: a checkpoint being written
	// (objects added, not yet referenced by update-ref) — in this process or
	// another one sharing the store — would lose its objects. git's default
	// two-week expiry is plenty for trimmed snapshots.
	gcArgs = []string{"gc", "--quiet"}
	// trimHook replaces the trim in tests (nil = trimLocked on the ref).
	trimHook func(m *Manager)
	// runGC is the background collection (a variable for tests).
	runGC = defaultRunGC
	// gcTimeout bounds one gc; past it the process is killed.
	gcTimeout = 2 * time.Minute
	// gcCommand builds the gc process (a variable for tests).
	gcCommand = func(ctx context.Context, storeDir string) *exec.Cmd {
		cmd := exec.CommandContext(ctx, "git", gcArgs...)
		cmd.Env = append(os.Environ(), "GIT_DIR="+storeDir)
		return cmd
	}
)

// defaultRunGC runs git gc on the store, killed after gcTimeout.
func defaultRunGC(storeDir string) {
	ctx, cancel := context.WithTimeout(context.Background(), gcTimeout)
	defer cancel()
	cmd := gcCommand(ctx, storeDir)
	// git gc runs repack/pack-objects as children that hold the pipes;
	// killing only gc left CombinedOutput waiting for them, with
	// m.maintaining stuck at true (no more trims for this Manager).
	proctree.Configure(ctx, cmd, 5*time.Second)
	out, err := cmd.CombinedOutput()
	switch {
	case ctx.Err() != nil:
		log.Warnf("[checkpoint] git gc killed after %v", gcTimeout)
	case err != nil:
		log.Warnf("[checkpoint] git gc: %v: %s", err, strings.TrimSpace(string(out)))
	}
}

// trimLocked keeps only the newest keepCheckpoints commits of ref once it has
// more than keepCheckpoints+pruneSlack. The kept commits are re-created (same
// tree and message) on a fresh root, so their hashes change; the newest one is
// returned so Create can hand it back. m.mu is held.
func (m *Manager) trimLocked(ref string) (string, error) {
	env := m.env()
	out, err := m.gitOutput(env, "rev-list", "--first-parent", ref)
	if err != nil {
		return "", err
	}
	hashes := strings.Fields(out) // newest first
	if keepCheckpoints <= 0 || len(hashes) <= keepCheckpoints+pruneSlack {
		return "", nil
	}
	kept := hashes[:keepCheckpoints]
	parent := ""
	for i := len(kept) - 1; i >= 0; i-- {
		tree := m.treeOf(kept[i])
		msg, err := m.gitOutput(env, "log", "-1", "--format=%B", kept[i])
		if err != nil {
			return "", err
		}
		args := []string{"-c", "user.name=cove", "-c", "user.email=cove@localhost", "commit-tree", tree, "-m", strings.TrimSpace(msg)}
		if parent != "" {
			args = append(args, "-p", parent)
		}
		h, err := m.gitOutput(env, args...)
		if err != nil {
			return "", err
		}
		parent = strings.TrimSpace(h)
	}
	if err := m.gitCmd(env, "update-ref", ref, parent); err != nil {
		return "", err
	}
	return parent, nil
}

// scheduleMaintenanceLocked counts a created checkpoint and, every
// max(pruneSlack,1) creations a trim and every gcEvery creations a gc, starts
// the background maintenance goroutine (or queues the work for the one
// already running). m.mu is held.
func (m *Manager) scheduleMaintenanceLocked() {
	m.created++
	if m.created%max(pruneSlack, 1) == 0 {
		m.pendingTrim = true
	}
	if gcEvery > 0 && m.created%gcEvery == 0 {
		m.pendingGC = true
	}
	if m.maintaining || (!m.pendingTrim && !m.pendingGC) {
		return
	}
	m.maintaining = true
	m.maintDone = make(chan struct{})
	go m.maintain(m.maintDone)
}

// maintain runs queued trims and gcs under m.mu until none is left, then
// closes done. (A WaitGroup here let a Create's Add race a WaitMaintenance
// that was just waking up: "WaitGroup is reused before previous Wait has
// returned", once regression_verify started sharing the engine's Manager.)
func (m *Manager) maintain(done chan struct{}) {
	defer close(done)
	m.mu.Lock()
	defer m.mu.Unlock()
	for m.pendingTrim || m.pendingGC {
		if m.pendingTrim {
			m.pendingTrim = false
			if trimHook != nil {
				trimHook(m)
			} else if _, err := m.trimLocked(m.refName); err != nil {
				log.Warnf("[checkpoint] trim: %v", err)
			}
		}
		if m.pendingGC {
			m.pendingGC = false
			// Unlocked: Create may run while gc does.
			m.mu.Unlock()
			runGC(m.storeDir)
			m.mu.Lock()
		}
	}
	m.maintaining = false
}

// WaitMaintenance waits for background trimming and gc (tests, shutdown).
func (m *Manager) WaitMaintenance() {
	m.mu.Lock()
	done, maintaining := m.maintDone, m.maintaining
	m.mu.Unlock()
	if maintaining && done != nil {
		<-done
	}
}
