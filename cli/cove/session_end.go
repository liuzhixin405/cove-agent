package main

import (
	"context"
	"fmt"
	"os"
	"sync/atomic"
	"time"

	"github.com/liuzhixin405/cove-agent/internal/dream"
	"github.com/liuzhixin405/cove-agent/internal/engine"
	"github.com/liuzhixin405/cove-agent/internal/hooks"
	"github.com/liuzhixin405/cove-agent/internal/log"
	"github.com/liuzhixin405/cove-agent/internal/memory"
)

// sessionEndTimeout bounds how long exit waits for SessionEnd hooks.
const sessionEndTimeout = 30 * time.Second

// sessionEndFired makes SessionEnd fire once per process: the TUI's exit path
// runs finishSession (via autoSaveSession) and then fireSessionEnd again for
// the empty-session case.
var sessionEndFired atomic.Bool

// sessionEndHooks builds the manager SessionEnd is fired on. The engine's own
// manager is not reachable from here, so the user's hooks.json is read again —
// the same user-level file the engine's hooks came from at startup (a
// variable so tests can substitute a manager).
var sessionEndHooks = func() *hooks.Manager {
	defs, err := loadUserHooks()
	if err != nil {
		log.Warnf("hooks config: %v", err)
	}
	m := hooks.NewManager()
	m.RegisterDefs(defs)
	return m
}

// sessionEndNotice prints the "running SessionEnd hooks" line; stderr, so
// the -p and headless stdout carries only answers (a variable for tests).
var sessionEndNotice = func(s string) { fmt.Fprint(os.Stderr, s) }

// fireSessionEnd is the exit work that is not about saving: it cancels a
// consolidation still running in the background (its lock is rolled back, so
// the run killed with the process is not taken as done), then runs the
// SessionEnd hooks once, waiting for them (async ones included) up to
// sessionEndTimeout — a fire-and-forget hook would die with the process.
//
// Automatic consolidation is switched off first: memory extraction may still
// finish during the (up to 30s) hook wait and call ExecuteAutoDream, which
// would start a run the exit then kills with the lock stamped as done. A run
// that got past the gate just before is cancelled again once the hooks return.
//
// On the first call it is also where the session_end dream trigger fires
// (startSessionEndDream), once the hooks have returned: a detached worker
// process, which outlives this one, so the suppression above does not apply
// to it. The turns it counts are the ones this process finished
// (noteTurnCompleted), not the saved history's.
func fireSessionEnd(eng *engine.Engine) {
	dream.SuppressAuto("进程退出中")
	dream.CancelActive()
	// A run that slipped past the suppression check before it was set may
	// register after the first cancel; cancel again on every return path.
	defer dream.CancelActive()
	if !sessionEndFired.CompareAndSwap(false, true) {
		return
	}
	if eng != nil {
		// After the hooks, on every return path below: a SessionEnd hook may
		// still write to the session or memory the worker is to read. A run
		// the extraction started during the hook wait is cancelled first.
		defer func() {
			dream.CancelActive()
			startSessionEndDream(completedTurns(), dream.Current())
		}()
	}
	m := sessionEndHooks()
	if m == nil || !m.Has(hooks.SessionEnd) {
		return
	}
	sessionEndNotice("正在运行 SessionEnd hook…\n")
	in := hooks.HookInput{Event: hooks.SessionEnd}
	if eng != nil {
		in.SessionID = eng.SessionID()
		if s := eng.Session(); s != nil {
			in.Model = s.Model
			in.Cwd = s.Cwd
		}
	}
	if in.Cwd == "" {
		in.Cwd, _ = os.Getwd()
	}
	ctx, cancel := context.WithTimeout(context.Background(), sessionEndTimeout)
	defer cancel()
	m.FireAndWait(ctx, hooks.SessionEnd, "", in)
}

// dreamSpawn starts the detached `cove --dream-worker` (a variable so tests
// never start a real process).
var dreamSpawn = dream.SpawnWorker

// dreamInlineBudget bounds the inline fallback run when the worker cannot be
// started: the user is waiting for the process to exit.
var dreamInlineBudget = 60 * time.Second

// dreamNotice prints the session-end dream lines on stderr, like
// sessionEndNotice (a variable for tests).
var dreamNotice = func(s string) { fmt.Fprint(os.Stderr, s) }

// startSessionEndDream is the session_end trigger: when the conversation had
// enough turns and there is something new to consolidate, it starts a
// detached worker process and returns at once (the consolidation survives
// this process). If the worker cannot be started it consolidates inline with
// runner, bounded by dreamInlineBudget, showing progress dots. --no-auto skips it.
func startSessionEndDream(turns int, runner *dream.Runner) {
	if noAuto {
		return
	}
	sessionsDir := dream.DefaultSessionsDir()
	if runner != nil && runner.SessionsDir() != "" {
		sessionsDir = runner.SessionsDir()
	}
	if due, why := dream.SessionEndDue(turns, sessionsDir); !due {
		log.Debugf("[autoDream] no session-end consolidation: %s", why)
		return
	}
	var args []string
	if profileName != "" {
		args = append(args, "--profile", profileName)
	}
	// The project root lets the worker consolidate this project's memory
	// directory as well as the global one.
	projectRoot := ""
	if cwd, err := os.Getwd(); err == nil {
		projectRoot = memory.ProjectRoot(cwd)
	}
	args = append(args, dream.WorkerArgs(sessionsDir, projectRoot)...)
	pid, err := dreamSpawn(args)
	if err == nil {
		log.Debugf("[autoDream] session-end worker started (PID %d)", pid)
		dreamNotice(fmt.Sprintf("已在后台启动记忆整理（约 1–3 分钟，至多约 %d 次后台模型调用，结果见 /dream）\n", dream.MaxDreamIterations))
		return
	}
	log.Warnf("[autoDream] cannot start the dream worker: %v", err)
	if runner == nil {
		return
	}
	runner.SetProjectRoot(projectRoot)
	dreamNotice(fmt.Sprintf("后台记忆整理启动失败，改在当前进程整理（最多 %d 秒）", int(dreamInlineBudget.Seconds())))
	err = runner.RunInline(context.Background(), dreamInlineBudget, time.Second, func() { dreamNotice(".") })
	if err != nil {
		dreamNotice(fmt.Sprintf("\n记忆整理未完成：%v\n", err))
		return
	}
	dreamNotice("\n记忆整理完成，结果见 /dream\n")
}

// turnsCompleted counts the turns this process finished (interactive,
// headless and -p alike). The session_end dream trigger compares it with
// min_turns: the saved history says nothing about this run — a resumed
// session carries old turns, a compacted one fewer than were run.
var turnsCompleted atomic.Int32

// noteTurnCompleted records one finished turn.
func noteTurnCompleted() { turnsCompleted.Add(1) }

// completedTurns is how many turns this process finished.
func completedTurns() int { return int(turnsCompleted.Load()) }

// resetTurnsCompleted zeroes the count (tests).
func resetTurnsCompleted() { turnsCompleted.Store(0) }
