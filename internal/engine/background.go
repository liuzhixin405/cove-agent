package engine

import (
	"context"
	"sync/atomic"
	"time"

	"github.com/liuzhixin405/cove-agent/internal/api"
	"github.com/liuzhixin405/cove-agent/internal/dream"
	"github.com/liuzhixin405/cove-agent/internal/extract"
	"github.com/liuzhixin405/cove-agent/internal/log"
	"github.com/liuzhixin405/cove-agent/internal/memory"
)

// BackgroundSummary is what the turn-end background work did, for the one
// dim line an interactive front end shows after the answer
// (OnBackgroundSummary).
type BackgroundSummary struct {
	// MemoriesExtracted is how many memories this turn's extraction saved.
	MemoriesExtracted int
	// DreamStatus is the auto-dream state after this turn's check; zero when
	// there is no dream runner.
	DreamStatus dream.Status
	// DreamFired reports that this turn started a consolidation.
	DreamFired bool
	// DreamChanged reports that the dream gate moved since the last summary
	// (it fired, or the number of sessions it still waits for changed).
	DreamChanged bool
	// SessionSaved is false when saving the session at the end of the turn
	// failed.
	SessionSaved bool
	// SessionsPruned is how many old saved sessions this turn's max_sessions
	// pruning deleted; MaxSessions is the limit it applied.
	SessionsPruned int
	MaxSessions    int
	// NewSkills names the skills this turn's review learned (saved under
	// ~/.cove/skills/auto-*).
	NewSkills []string
	// UpdatedSkills names learned skills whose auto-generated file this
	// turn's review rewrote.
	UpdatedSkills []string
	// Extra holds further lines for the summary, in Chinese, ready to show
	// (the checkpoint hint, for one).
	Extra []string
}

// Notable reports whether the summary is worth a line: something was
// learned, the dream gate moved, old sessions were deleted, or the session
// could not be saved.
func (s BackgroundSummary) Notable() bool {
	return s.MemoriesExtracted > 0 || s.DreamChanged || s.SessionsPruned > 0 || !s.SessionSaved ||
		len(s.NewSkills) > 0 || len(s.UpdatedSkills) > 0 || len(s.Extra) > 0
}

// pruneWarned makes the first max_sessions deletion in a process a warning
// (the default of 200 deletes history on the first turn after an upgrade);
// pruneWarnf is log.Warnf, a variable for tests.
var (
	pruneWarned atomic.Bool
	pruneWarnf  = log.Warnf
)

// extractTimeout bounds one turn-end memory extraction.
const extractTimeout = 30 * time.Second

// setExtractRunner installs r as the turn-end memory extractor: its finished
// extractions are recorded in the memory store (so /memory stats and the
// store's cache see them) and the memories it saves are counted for the
// background summary.
func (e *Engine) setExtractRunner(r *extract.Runner) {
	e.extractRunner = r
	if r == nil {
		return
	}
	if e.memStore != nil {
		r.SetRecorder(e.memStore)
	}
	r.OnSave = func(n int) { e.extractSaved.Add(int64(n)) }
}

// WaitBackground waits for the background work started at the end of the
// last turn (memory extraction, the auto-dream check, session pruning), or
// until ctx ends. cove -p calls it before exiting, which otherwise killed the
// extraction every time. A consolidation the check started runs on its own
// detached context and is not waited for, and neither is the skill review
// (up to reviewTimeout of a model call cove -p has no use for).
func (e *Engine) WaitBackground(ctx context.Context) {
	done := make(chan struct{})
	go func() {
		e.bg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-ctx.Done():
	}
}

// BackgroundPending reports whether turn-end work WaitBackground would wait
// for (memory extraction, above all) is still running, so an exit can say
// why it pauses only when it does.
func (e *Engine) BackgroundPending() bool { return e.bgPending.Load() > 0 }

// waitReview waits for a running skill review (tests; WaitBackground does
// not).
func (e *Engine) waitReview() { e.reviewBg.Wait() }

// DreamStatus reports the auto-dream gates and last run of this session's
// runner (read from disk when there is none), for /diagnose and /doctor.
func (e *Engine) DreamStatus() dream.Status {
	if e.dreamRunner != nil {
		return e.dreamRunner.Status()
	}
	return dream.StatusFromDisk()
}

// backgroundJob is what one turn hands its background goroutine, captured on
// the turn's goroutine so the background never reads engine state a later
// turn is changing.
type backgroundJob struct {
	msgs      []api.Message // snapshot for memory extraction
	learn     bool          // background learning is on (not --no-auto)
	saved     bool          // the turn-end session save succeeded
	sessionID string        // the session pruning must keep, and a learned skill names
	cwd       string
	keep      int           // max_sessions
	review    []api.Message // snapshot for the skill review; nil = skip it
	// checkpointed: the turn wrote or edited files after a checkpoint was
	// taken, so /undo can take the changes back.
	checkpointed bool
}

// checkpointHint is the summary line of a turn that changed files under a
// fresh checkpoint.
const checkpointHint = "已建检查点，/undo 可回退"

// runBackgroundWork is the turn-end background goroutine: prune old
// sessions, extract memories, check the auto-dream gates, run the skill
// review, then report to OnBackgroundSummary when something is worth
// saying. What WaitBackground waits for (e.bg) ends before the review, which
// only an interactive session outlives.
func (e *Engine) runBackgroundWork(job backgroundJob) {
	waited := true
	releaseWaited := func() {
		if waited {
			waited = false
			e.bgPending.Add(-1)
			e.bg.Done()
		}
	}
	defer releaseWaited()
	// reviewMessages marked a review running for this job; runReview clears
	// the mark, but a panic before it would leave the review off for good.
	defer func() {
		if len(job.review) > 0 {
			e.bgMu.Lock()
			e.reviewRunning = false
			e.bgMu.Unlock()
		}
	}()
	defer func() {
		if r := recover(); r != nil {
			log.Warnf("[turn-end] background panic: %v", r)
		}
	}()

	pruned := 0
	if e.store != nil && job.sessionID != "" {
		n, err := e.store.AutoPrune(job.keep, job.sessionID)
		if err != nil {
			log.Warnf("[sessions] auto prune: %v", err)
		}
		if n > 0 {
			pruned = n
			if pruneWarned.CompareAndSwap(false, true) {
				pruneWarnf("[sessions] 已按 max_sessions=%d 自动删除 %d 个旧会话；在 config.json、.cove.json 或 profile 中把 max_sessions 设为负数可关闭自动清理", job.keep, n)
			} else {
				log.Debugf("[sessions] pruned %d old sessions (max_sessions=%d)", n, job.keep)
			}
		}
	}

	extracted := 0
	if job.learn && e.extractRunner != nil && len(job.msgs) > 0 {
		ctx, cancel := context.WithTimeout(context.Background(), extractTimeout)
		before := e.extractSaved.Load()
		mems := e.memorySnapshot()
		e.extractRunner.ExtractWithSource(ctx, job.msgs, memory.ProvenanceSource{SessionIDs: []string{job.sessionID}, Cwd: job.cwd})
		cancel()
		extracted = int(e.extractSaved.Load() - before)
		if extracted > 0 {
			e.recordNewMemories(mems)
		}
	}

	sum := BackgroundSummary{MemoriesExtracted: extracted, SessionSaved: job.saved, SessionsPruned: pruned}
	if pruned > 0 {
		sum.MaxSessions = job.keep
	}
	if job.checkpointed {
		sum.Extra = append(sum.Extra, checkpointHint)
	}
	report := e.OnBackgroundSummary
	if job.learn && e.dreamRunner != nil {
		wasRunning := dream.ActiveTask() != nil
		// The run's own 5-minute bound lives in ExecuteAutoDream's detached
		// context: this returns as soon as a consolidation is spawned.
		e.dreamRunner.ExecuteAutoDream(context.Background())
		if report != nil {
			sum.DreamStatus = e.dreamRunner.Status()
			sum.DreamFired = sum.DreamStatus.Running && !wasRunning
			sum.DreamChanged = e.dreamGateMoved(sum.DreamStatus, sum.DreamFired)
		}
	}
	if job.learn && len(job.review) > 0 {
		e.reviewBg.Add(1)
		releaseWaited()
		func() {
			defer e.reviewBg.Done()
			res := e.runReview(job.review, job.sessionID)
			sum.NewSkills, sum.UpdatedSkills = res.added, res.updated
		}()
	}
	if report != nil && sum.Notable() {
		report(sum)
	}
}

// dreamGateMoved reports whether st differs from what the last summary saw:
// a consolidation fired, or the sessions the gate waits for changed (the
// first observation is only a baseline). A
// disabled or suppressed dream never counts.
func (e *Engine) dreamGateMoved(st dream.Status, fired bool) bool {
	if !st.Enabled || st.Suppressed != "" {
		return false
	}
	needed := st.SessionsNeeded()
	e.bgMu.Lock()
	defer e.bgMu.Unlock()
	moved := fired || (e.dreamSeen && needed != e.lastDreamNeeded)
	e.dreamSeen, e.lastDreamNeeded = true, needed
	return moved
}
