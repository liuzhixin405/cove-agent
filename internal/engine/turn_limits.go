package engine

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/liuzhixin405/cove-agent/internal/api"
	"github.com/liuzhixin405/cove-agent/internal/log"
)

// DefaultMaxIterations is how many model calls one turn may make before the
// engine stops (headless, -p) or asks whether to go on (interactive), when
// Config.MaxIterations is 0.
const DefaultMaxIterations = 200

// UnlimitedIterations as Config.MaxIterations (or SetMaxIterations) removes
// the per-turn cap: "cove -p --max-turns 0".
const UnlimitedIterations = -1

// Reasons a turn limit prompt is shown for (LimitStats.Reason).
const (
	LimitReasonIterations = "iterations" // the model-call cap was reached
	LimitReasonTime       = "time"       // the turn ran longer than max_turn_minutes
	LimitReasonStagnation = "stagnation" // many rounds without any file activity
	LimitReasonLoop       = "loop"       // loop detection hit a second time this turn
)

// LimitDecision is the user's answer to a turn limit prompt.
type LimitDecision int

const (
	// LimitStop ends the turn the way every interruption does: completed
	// tool rounds stay in history, and re-sending the message (/continue)
	// resumes it.
	LimitStop LimitDecision = iota
	// LimitContinue lets the turn go on for one more window of the same size.
	LimitContinue
)

// LimitStats describes the turn at the moment a limit is reached, for the
// prompt that asks whether to go on.
type LimitStats struct {
	Iterations  int           // model calls made this turn
	Elapsed     time.Duration // time since the turn started
	Cost        float64       // USD spent this turn
	RecentSteps []string      // the last few tool names, oldest first
	Reason      string        // LimitReasonIterations / Time / Stagnation
	// Window is what LimitContinue grants: model calls for Reason
	// "iterations", minutes for "time", 0 for "stagnation" and "loop".
	Window int
	// Detail is what the loop detector saw (Reason "loop"), "" otherwise.
	Detail string
}

// recentStepsKept is how many tool names LimitStats.RecentSteps carries.
const recentStepsKept = 5

// MaxIterations is the per-turn model-call cap in effect: DefaultMaxIterations
// when unset, UnlimitedIterations when there is none.
func (e *Engine) MaxIterations() int {
	switch n := e.config.MaxIterations; {
	case n == 0:
		return DefaultMaxIterations
	case n < 0:
		return UnlimitedIterations
	default:
		return n
	}
}

// SetMaxIterations changes the per-turn model-call cap (0 = default,
// UnlimitedIterations = none). "cove -p --max-turns N" uses it.
func (e *Engine) SetMaxIterations(n int) { e.config.MaxIterations = n }

// SetMaxTurnMinutes changes the per-turn time limit (0 = none).
func (e *Engine) SetMaxTurnMinutes(n int) {
	if n < 0 {
		n = 0
	}
	e.config.MaxTurnMinutes = n
}

// MaxTurnMinutes is the per-turn time limit in effect, 0 when off.
func (e *Engine) MaxTurnMinutes() int { return e.config.MaxTurnMinutes }

// turnLimits tracks one turn's soft limits. A zero window means that limit
// is off.
type turnLimits struct {
	start      time.Time
	startCost  float64
	iterWindow int
	iterCap    int
	timeWindow time.Duration
	deadline   time.Duration
	minutes    int
	stagnation bool // the stagnation prompt was already shown this turn
	steps      []string

	// Nudges shown this turn (caps in nudges.go).
	continuationNudges int  // announced-step and degenerate-ending nudges
	emptyRetries       int  // empty-reply retries
	doneChecks         int  // done-check prompts
	usedTools          bool // a tool that is not read-only ran this turn (noteWorkTool)
	// passedCmds are shell commands that passed since the last possible
	// file change this turn (noteVerifyEvidence).
	passedCmds map[string]bool
	// loopHits counts non-fatal loop detections this turn (loopPromptOnHit).
	loopHits int
	// iterNoticeCap and timeNoticeDeadline are the windows the budget notice
	// was already shown for (budgetNotice).
	iterNoticeCap      int
	timeNoticeDeadline time.Duration
}

func (e *Engine) newTurnLimits() *turnLimits {
	l := &turnLimits{start: time.Now(), startCost: e.costTracker.Totals().Cost}
	if n := e.MaxIterations(); n > 0 {
		l.iterWindow, l.iterCap = n, n
	}
	if m := e.config.MaxTurnMinutes; m > 0 {
		unit := e.turnTimeUnit
		if unit <= 0 {
			unit = time.Minute
		}
		l.minutes = m
		l.timeWindow = time.Duration(m) * unit
		l.deadline = l.timeWindow
	}
	return l
}

func (l *turnLimits) addStep(name string) {
	l.steps = append(l.steps, name)
	if len(l.steps) > recentStepsKept {
		l.steps = l.steps[len(l.steps)-recentStepsKept:]
	}
}

func (e *Engine) limitStats(l *turnLimits, iterations int, reason string, window int) LimitStats {
	return LimitStats{
		Iterations:  iterations,
		Elapsed:     time.Since(l.start),
		Cost:        e.costTracker.Totals().Cost - l.startCost,
		RecentSteps: append([]string(nil), l.steps...),
		Reason:      reason,
		Window:      window,
	}
}

// askLimit asks the front end whether the turn may go on. With no prompt
// installed (-p, headless) the answer is always stop.
func (e *Engine) askLimit(stats LimitStats) LimitDecision {
	if e.IterationLimitPrompt == nil {
		return LimitStop
	}
	// Pause, prompt and resume under one lock, like the permission prompt.
	e.promptMu.Lock()
	defer e.promptMu.Unlock()
	hooks := e.turnHooks()
	if hooks.PermissionPause != nil {
		hooks.PermissionPause()
	}
	d := e.IterationLimitPrompt(stats)
	if hooks.PermissionDone != nil {
		hooks.PermissionDone()
	}
	return d
}

// checkTurnLimits is called before each model call. When the turn must stop
// at its iteration cap or time limit it returns the interruption reason and
// the turn's error.
func (e *Engine) checkTurnLimits(l *turnLimits, iter int) (string, error) {
	if l.iterWindow > 0 && iter >= l.iterCap {
		if e.askLimit(e.limitStats(l, iter, LimitReasonIterations, l.iterWindow)) != LimitContinue {
			return "达到单轮最大迭代次数", &LimitError{Reason: LimitReasonIterations, Limit: iter}
		}
		l.iterCap += l.iterWindow
	}
	if l.timeWindow > 0 && time.Since(l.start) >= l.deadline {
		if e.askLimit(e.limitStats(l, iter, LimitReasonTime, l.minutes)) != LimitContinue {
			return "达到单轮时间上限", &LimitError{Reason: LimitReasonTime, Limit: l.minutes}
		}
		// One more window from now, not from the old deadline: the prompt
		// itself may have waited for minutes.
		l.deadline = time.Since(l.start) + l.timeWindow
	}
	return "", nil
}

// errStagnationStopped is the turn error when the user stops a turn at the
// stagnation prompt.
var errStagnationStopped = &LimitError{Reason: LimitReasonStagnation}

// errLoopStopped is the turn error when the user stops a turn at the loop
// prompt.
var errLoopStopped = &LimitError{Reason: LimitReasonLoop}

// LimitError is the error of a turn stopped at one of its limits. The turn
// is resumable (completed steps are kept). Its text names only the config
// key: a front end adds what it offers (/continue in the shell, --max-turns
// under -p).
type LimitError struct {
	Reason string // LimitReasonIterations / Time / Stagnation / Loop
	// Limit is the model calls made (iterations) or the minutes allowed
	// (time); 0 for stagnation.
	Limit int
}

func (e *LimitError) Error() string {
	switch e.Reason {
	case LimitReasonIterations:
		return fmt.Sprintf("已达到单轮最大迭代次数 %d（可配置 max_iterations 调整）", e.Limit)
	case LimitReasonTime:
		return fmt.Sprintf("已达到单轮时间上限 %d 分钟（可配置 max_turn_minutes 调整，0 为不限制）", e.Limit)
	case LimitReasonLoop:
		return "检测到操作循环，已按你的选择停止本轮"
	default:
		return "连续多轮没有新的文件读写，已按你的选择停止本轮"
	}
}

// wrapUpMaxTokens bounds the no-tool summary a stopped turn ends with.
const wrapUpMaxTokens = 1024

// wrapUpPromptFmt asks for that summary; %s is the stop reason.
const wrapUpPromptFmt = "[system: The run is stopping (%s). Without calling tools, summarize in the user's language, for the latest request only: what was completed, what remains, and the recommended next step. In a git repository, say whether the changes are committed and pushed.]"

// errWrapUpSkipped is wrapUpSummary's error when no call was made.
var errWrapUpSkipped = errors.New("wrap-up skipped")

// wrapUpSummary asks the turn's model, with no tools offered, for a short
// status report: what was completed, what remains, what to do next. It is
// one metered call through e.llm, skipped when the context is done, the
// budget is exhausted. The prompt is not kept in
// history.
func (e *Engine) wrapUpSummary(ctx context.Context, routedModel, reason string) (string, error) {
	if ctx.Err() != nil {
		return "", ctx.Err()
	}
	if e.costTracker.OverBudget() {
		return "", errWrapUpSkipped
	}
	model := routedModel
	if model == "" {
		model = e.currentModel()
	}
	msgs := append(append([]api.Message(nil), e.messages...), newSyntheticUserMsg(fmt.Sprintf(wrapUpPromptFmt, reason)))
	var tools []api.ToolDef
	if api.CapabilitiesOf(e.llm).ToolsWithToolHistory {
		// Anthropic rejects a history with tool_use blocks when the request
		// defines no tools; the prompt still says not to call any, and any
		// call in the reply is dropped.
		tools = e.buildAPIToolDefs()
		msgs = api.InjectCacheBreakpoints(msgs)
	}
	req := api.ChatRequest{
		Model:      model,
		Messages:   msgs,
		SystemBase: e.SystemPrompt(),
		Tools:      tools,
		MaxTokens:  wrapUpMaxTokens,
		// A short plain report: no thinking budget competing with the
		// 1024 output tokens.
		Thinking: "disabled",
	}
	act := e.beginActivity("生成收尾总结 " + model)
	resp, err := e.llm.Chat(ctx, req)
	e.endActivity(act)
	if err != nil {
		log.Debugf("wrap-up summary failed: %v", err)
		return "", err
	}
	return strings.TrimSpace(resp.Content), nil
}

// stopWithWrapUp ends a turn the model did not choose to end (a limit, a
// loop): the turn is interrupted as usual (resumable), then the model's
// no-tool summary is appended to history and shown — streamed through
// onDelta when there is one, else as engine output — and kept for -p
// (LastWrapUp). A failed or skipped summary is silently left out.
func (e *Engine) stopWithWrapUp(ctx context.Context, user api.Message, routedModel, reason string, onDelta func(string)) {
	e.interrupt(user, routedModel, reason)
	// After the summary, like a turn that ended on its own.
	defer e.reportGitWorkState(ctx)
	s, err := e.wrapUpSummary(ctx, routedModel, reason)
	if err != nil || s == "" {
		return
	}
	e.messages = append(e.messages, api.Message{Role: "assistant", Content: s})
	e.lastWrapUp = s
	if onDelta != nil {
		onDelta("\n\n" + s)
	} else {
		e.engineOutput(s)
	}
	e.saveSession()
}

// LastWrapUp is the summary the last stopped turn ended with, "" when there
// was none. A -p run prints it before the stop reason.
func (e *Engine) LastWrapUp() string { return e.lastWrapUp }

// budgetNoticeFmt tells the model how much of its window is left; %s is
// "N model calls", "M minutes" or both.
const budgetNoticeFmt = "[budget: about %s remain in this window. Prioritize converging on a result; do not stop solely because of this notice.]"

// budgetNotice returns the notice to append to the latest tool result once
// calls model calls (or the time since the window began) reach
// budgetNoticeRatio of the iteration (or time) window, and "" otherwise. It
// fires at most once per window of each kind.
func (e *Engine) budgetNotice(l *turnLimits, calls int) string {
	fire := false
	if l.iterWindow > 0 && l.iterNoticeCap != l.iterCap {
		used := calls - (l.iterCap - l.iterWindow)
		if float64(used) >= budgetNoticeRatio*float64(l.iterWindow) {
			l.iterNoticeCap = l.iterCap
			// At the cap itself the limit prompt comes next; "0 calls
			// remain" would tell the model nothing.
			fire = calls < l.iterCap
		}
	}
	elapsed := time.Since(l.start)
	if l.timeWindow > 0 && l.timeNoticeDeadline != l.deadline {
		used := elapsed - (l.deadline - l.timeWindow)
		if float64(used) >= budgetNoticeRatio*float64(l.timeWindow) {
			l.timeNoticeDeadline = l.deadline
			fire = true
		}
	}
	if !fire {
		return ""
	}
	var parts []string
	if l.iterWindow > 0 {
		parts = append(parts, fmt.Sprintf("%d model calls", max(l.iterCap-calls, 0)))
	}
	if l.timeWindow > 0 {
		unit := e.turnTimeUnit
		if unit <= 0 {
			unit = time.Minute
		}
		left := int(math.Ceil(float64(l.deadline-elapsed) / float64(unit)))
		parts = append(parts, fmt.Sprintf("%d minutes", max(left, 0)))
	}
	return fmt.Sprintf(budgetNoticeFmt, strings.Join(parts, " / "))
}

// loopHitAction is what the turn does after a non-fatal loop detection.
type loopHitAction int

const (
	loopHitGuide  loopHitAction = iota // inject guidance (the first hit, and -p)
	loopHitIgnore                      // the user chose to go on: detection is off
	loopHitStop                        // the user chose to stop
)

// onLoopHit counts a non-fatal loop detection. At the loopPromptOnHit-th hit
// of a turn an interactive front end is asked (Reason LimitReasonLoop)
// whether to turn loop detection off for the rest of the turn and go on, or
// stop. Without a prompt (-p, headless) every hit is handled as before.
func (e *Engine) onLoopHit(l *turnLimits, calls int, lr LoopResult) loopHitAction {
	l.loopHits++
	if e.IterationLimitPrompt == nil || l.loopHits != loopPromptOnHit {
		return loopHitGuide
	}
	stats := e.limitStats(l, calls, LimitReasonLoop, 0)
	stats.Detail = lr.Reason
	if e.askLimit(stats) == LimitContinue {
		e.loopDetector.DisableForTurn()
		return loopHitIgnore
	}
	return loopHitStop
}

// interruptMarkerReason turns an interruption reason (Chinese, shown to the
// user) into the English phrase the history marker carries.
func interruptMarkerReason(reason string) string {
	switch {
	case strings.Contains(reason, "取消"):
		return "user cancel"
	case strings.Contains(reason, "预算"):
		return "budget exhausted"
	case strings.Contains(reason, "模型调用失败"):
		return "API error"
	case strings.Contains(reason, "迭代"):
		return "iteration limit"
	case strings.Contains(reason, "时间上限"):
		return "time limit"
	case strings.Contains(reason, "循环"):
		return "loop detected"
	case strings.Contains(reason, "停滞"):
		return "stagnation"
	}
	return "stopped"
}
