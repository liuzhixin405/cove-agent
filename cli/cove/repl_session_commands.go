package main

import (
	"context"
	"fmt"
	"strings"

	"github.com/liuzhixin405/cove-agent/internal/engine"
)

func handleSessionCommand(input string, eng *engine.Engine, historyPickPending *bool, ask func(string, int, func())) bool {
	switch {
	case strings.HasPrefix(input, "/export"):
		handleExport(input, eng)
		return true
	case strings.HasPrefix(input, "/resume") || input == "/resume":
		sessionID := ""
		if strings.HasPrefix(input, "/resume ") {
			sessionID = strings.TrimPrefix(input, "/resume ")
		}
		withInterrupt(func(ctx context.Context) { handleResume(ctx, sessionID, eng) })
		return true
	case input == "/history":
		// Display the history list and set pick-pending state so the next numeric
		// input is treated as a session selection by the main loop.
		handleHistory(eng, false)
		*historyPickPending = true
		return true
	case strings.HasPrefix(input, "/history "):
		histID := strings.TrimSpace(strings.TrimPrefix(input, "/history "))
		if strings.EqualFold(histID, "clean") {
			handleHistoryClean()
			*historyPickPending = false
			return true
		}
		if all, confirm, ok := parseHistoryClear(histID); ok {
			handleHistoryClear(eng, all, confirm, ask)
			*historyPickPending = false
			return true
		}
		// "/history all ..." addresses the all-projects list; without it,
		// numbers index the current project's list.
		all := false
		if strings.EqualFold(histID, "all") {
			handleHistory(eng, true)
			*historyPickPending = true
			return true
		}
		if strings.HasPrefix(strings.ToLower(histID), "all ") {
			all = true
			histID = strings.TrimSpace(histID[len("all "):])
		}
		// With or without an argument: bare "/history detail" used to fall
		// through to resuming a session named "detail" instead of printing
		// the handler's usage line.
		if _, arg, ok := historySubcommand(histID, "detail"); ok {
			handleHistoryDetail(arg, eng, all)
			*historyPickPending = false
			return true
		}
		if _, arg, ok := historySubcommand(histID, "delete"); ok {
			handleHistoryDelete(arg, eng, all)
			*historyPickPending = false
			return true
		}
		handleHistoryResumeIn(histID, eng, all)
		*historyPickPending = false
		return true
	case input == "/compact":
		withInterrupt(func(ctx context.Context) {
			outln(compactReportLine(eng.Compact(ctx)))
		})
		return true
	default:
		return false
	}
}

// startNewSession is /new: the current conversation is saved and the engine
// starts an empty one. The REPL's own leftovers of the old conversation go
// with it — the failed request "继续" would retry, the interrupted-turn draft
// and the pending attachments. It returns the saved session's ID ("" when
// there was nothing to save).
func startNewSession(eng *engine.Engine, tasks *replTaskRunner, attachedFiles *[]string) string {
	ctx, cancel := context.WithTimeout(context.Background(), exitBackgroundWait)
	defer cancel()
	// The ID of the session being left, taken before NewSession replaces it:
	// only that session's draft goes with it. /new used to delete whatever
	// draft there was, another project's included.
	left := eng.SessionID()
	saved := eng.NewSession(ctx)
	if tasks != nil {
		tasks.ClearPendingFailed()
	}
	_ = clearInterruptedDraftFor(left)
	*attachedFiles = nil
	return saved
}

func newSessionNotice(saved string) string {
	if saved == "" {
		return "[新会话] 已开始新会话"
	}
	return fmt.Sprintf("[新会话] 已开始新会话；上一个会话已保存（%s），可用 /history 找回", saved)
}

// compactReportLine is what /compact prints: the token counts around a real
// compaction, or why nothing was compressed (it used to print "已压缩"
// whatever happened).
func compactReportLine(r engine.CompactReport) string {
	switch {
	case r.Summarized:
		return fmt.Sprintf("已压缩：压缩前 %d tokens → 压缩后 %d tokens。", r.BeforeTokens, r.AfterTokens)
	case r.Compressed:
		return fmt.Sprintf("仅部分压缩（%s）：压缩前 %d tokens → 压缩后 %d tokens。", r.Reason, r.BeforeTokens, r.AfterTokens)
	case r.Reason != "":
		return fmt.Sprintf("未压缩：%s（当前 %d tokens）。", r.Reason, r.BeforeTokens)
	default:
		return fmt.Sprintf("未压缩（当前 %d tokens）。", r.BeforeTokens)
	}
}

// historySubcommand reports whether rest is the history sub-command name
// (case-insensitive), alone or followed by an argument, and returns that
// argument trimmed.
func historySubcommand(rest, name string) (sub, arg string, ok bool) {
	lower := strings.ToLower(rest)
	switch {
	case lower == name:
		return name, "", true
	case strings.HasPrefix(lower, name+" "):
		return name, strings.TrimSpace(rest[len(name)+1:]), true
	}
	return "", "", false
}
