package engine

import (
	"context"
	"os"
	"time"

	"github.com/liuzhixin405/cove-agent/internal/cost"
	"github.com/liuzhixin405/cove-agent/internal/guardrail"
	"github.com/liuzhixin405/cove-agent/internal/session"
)

// resumeBackgroundWait bounds how long ResumeSession waits for the previous
// conversation's turn-end background work.
const resumeBackgroundWait = 10 * time.Second

// NewSession ends the current conversation and starts an empty one in the
// same process (/new). The old session is saved first, so it stays in
// /history. It returns the ID of the session that was saved ("" when it had
// no messages and nothing was written).
//
// Everything the model was told in the old conversation is dropped with it:
// the interrupted turn, the shown skills, memories and hints, the loop and
// guardrail bookkeeping. What belongs to the process stays — provider, mode,
// permission rules, checkpoints (/undo still works), the cost tracker and its
// budget.
func (e *Engine) NewSession(ctx context.Context) (savedID string) {
	savedID = e.leaveSession(ctx)
	e.resetConversationState()
	e.LoadMessages(nil)
	e.sessionView = session.NewSessionView(e.messages, 0)
	e.setCostBase(nil)

	if e.store != nil {
		wd, _ := os.Getwd()
		e.enterSession(&session.Record{
			ID:        newSessionID(),
			CreatedAt: time.Now(),
			Title:     "New session",
			Model:     e.config.Model,
			Cwd:       session.NormalizeProjectDir(wd),
		})
	}
	return savedID
}

// ResumeSession continues a saved session: its messages are loaded and later
// turns are saved under its ID. Resuming used to load the messages into a new
// session, so every resume left a copy and the original never grew.
//
// It switches sessions the way NewSession does. It used to replace only the
// messages and the record: the previous conversation's interrupted turn
// survived (/continue then "resumed" a request the loaded history had never
// seen), the current session was not saved first, and the record's tokens and
// cost were overwritten with the process's totals.
func (e *Engine) ResumeSession(r *session.Record) {
	if r == nil {
		return
	}
	if e.session != nil && e.session.ID == r.ID && len(e.messages) > 0 {
		// Already the current session; r is the copy on disk and may be
		// older than what is in memory.
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), resumeBackgroundWait)
	e.leaveSession(ctx)
	cancel()
	e.resetConversationState()
	e.LoadMessages(r.Messages)
	e.sessionView = session.NewSessionView(e.messages, 0)
	if e.store == nil {
		return
	}
	if r.Cwd == "" {
		// A session saved before sessions recorded their project joins the
		// one it is continued in.
		wd, _ := os.Getwd()
		r.Cwd = session.NormalizeProjectDir(wd)
	}
	e.setCostBase(r)
	e.enterSession(r)
}

// leaveSession waits for the turn-end background work of the current
// conversation (it reads the messages about to be replaced; ctx bounds the
// wait) and saves it. It returns the saved session's ID, "" when there was
// nothing to save.
func (e *Engine) leaveSession(ctx context.Context) string {
	e.WaitBackground(ctx)
	if e.session == nil || len(e.messages) == 0 {
		return ""
	}
	e.saveSession()
	return e.session.ID
}

func (e *Engine) enterSession(r *session.Record) {
	e.session = r
	// The dream gate must not count the session in use as one to
	// consolidate.
	if e.dreamRunner != nil {
		e.dreamRunner.SetCurrentSession(r.ID)
	}
}

// resetConversationState drops what belongs to one conversation. Anything
// added to Engine that is tied to the conversation (not to the process or
// the project) is reset here, so /new and /resume cannot disagree.
func (e *Engine) resetConversationState() {
	e.conversation = conversation{}
	e.systemPrompt = ""
	// Plan mode the model entered belongs to its conversation; the user's
	// /mode plan is the permission mode and stays.
	if e.runtime != nil {
		e.runtime.SetPlanMode(false)
		// So is its task list: kept, the todo reminders of the old task
		// followed the user into the new conversation.
		e.runtime.ClearTodos()
	}
	if e.loopDetector != nil {
		e.loopDetector = NewLoopDetector()
	}
	e.guardrails = guardrail.New()

	e.steerMu.Lock()
	e.pendingSteer, e.pendingSteerN = "", 0
	e.steerMu.Unlock()
	e.acceptanceMu.Lock()
	e.acceptance = nil
	e.acceptanceMu.Unlock()

	e.fileMu.Lock()
	// Emptied, not nil: trackFileChanges assigns into it, and the nil map
	// this used to leave made every write or edit after /new or /resume
	// panic, reporting a file that had been written as a failed call.
	e.fileHistory = map[string]bool{}
	e.turnFilesChanged, e.turnCheckpointed, e.turnRanGit = false, false, false
	e.turnChangedFiles = nil
	e.fileMu.Unlock()

	// The skill review is throttled by the message count it last saw; kept
	// across a switch, a shorter conversation stayed below it and was never
	// reviewed.
	e.bgMu.Lock()
	e.conversationGen++
	e.lastReviewMsgCount = 0
	e.turnsSinceReview = 0
	e.turnUsedWork = false
	e.bgMu.Unlock()

	e.clearNewMemories()
	e.resetShownContext()
	e.repoMapMu.Lock()
	e.repoMapExcerpts = 0
	e.repoMapMu.Unlock()
}

// setCostBase makes the session record count what it spent: the tracker
// counts the whole process, so the base is the tracker's totals now minus
// what the session being entered (r, nil for a new one) had already spent.
func (e *Engine) setCostBase(r *session.Record) {
	if e.costTracker == nil {
		return
	}
	base := e.costTracker.Totals()
	if r != nil {
		base.Input -= r.TokensIn
		base.Output -= r.TokensOut
		base.Cost -= r.Cost
	}
	e.costBase = base
}

// sessionTotals is what the current session spent: the tracker's totals
// minus the base set when it was entered (setCostBase).
func (e *Engine) sessionTotals() cost.Totals {
	t := e.costTracker.Totals()
	t.Input -= e.costBase.Input
	t.Output -= e.costBase.Output
	t.Cost -= e.costBase.Cost
	return t
}
