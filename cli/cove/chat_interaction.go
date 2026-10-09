package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/liuzhixin405/cove-agent/internal/api"
	"github.com/liuzhixin405/cove-agent/internal/cost"
	"github.com/liuzhixin405/cove-agent/internal/engine"
	"github.com/liuzhixin405/cove-agent/internal/render"
	"github.com/liuzhixin405/cove-agent/internal/termui"
	"github.com/liuzhixin405/cove-agent/internal/textutil"
	"github.com/liuzhixin405/cove-agent/internal/uiout"
)

func isTransientRequestError(err error) bool {
	if err == nil {
		return false
	}
	s := strings.ToLower(err.Error())
	// Timeouts are deliberately absent: the request may have been served
	// and billed, so the manual promises not to resend it (the user can
	// /continue). Connection-level failures never reached the model.
	transientHints := []string{
		"connection reset", "broken pipe", "connection refused", "eof",
		"temporary", "temporarily unavailable", "server error 5", "bad gateway",
	}
	for _, h := range transientHints {
		if strings.Contains(s, h) {
			return true
		}
	}
	return false
}

func runChatInteraction(ctx context.Context, runner chatRunner, input string) (string, error) {
	return runChatInteractionMessage(ctx, runner, api.Message{Role: "user", Content: input})
}

func runChatInteractionMessage(ctx context.Context, runner chatRunner, userMsg api.Message) (string, error) {
	// A background summary arriving while this turn streams (the previous
	// turn's extraction outlived it) waits until the turn's output is done.
	bgSummaries.beginTurn()
	defer bgSummaries.endTurn()
	termui.BeginOutput()
	defer termui.EndOutput()
	var totalOutput strings.Builder
	var finalErr error
	var reply string

	maxAttempts := 3
	p := newTurnPrinter()
	p.compactTools = replInteractive
	defer p.stop()
	turnStart := time.Now()
	var usage *turnUsage
	if eng, ok := runner.(*engine.Engine); ok {
		usage = newTurnUsage(eng, turnStart)
		p.status = usage.live
	}
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		p.beginAttempt()

		if eng, ok := runner.(*engine.Engine); ok {
			eng.SetTurnHooks(&engine.TurnHooks{
				PermissionPause: p.permissionPause,
				PermissionDone:  p.permissionDone,
				// Live output from long-running tools (bash/powershell), so the
				// user can tell what a slow command is doing instead of only
				// seeing the stall warning.
				ToolProgress:       p.toolProgress,
				ToolStderrProgress: p.toolStderrProgress,
				ToolOutputStart:    p.toolOutputStart,
				TurnModel:          func(model string) { p.engineLine(turnModelLine(model)) },
			})
			// Engine diagnostic lines (tool blocks, stall warnings, memory
			// notices) go through the printer into the conversation area.
			// Tool blocks keep their "#id" and are remembered for /x.
			eng.SetOutput(uiout.NewFuncs(uiout.Funcs{
				OnBlock: func(b render.Block) {
					sessionBlocks.add(b)
					p.engineLine(engine.RenderBlock(b) + "\n")
				},
				OnLine: p.engineLine,
			}))
			defer func() {
				eng.SetTurnHooks(nil)
				eng.SetOutput(nil)
			}()
		}

		var err error
		onDelta := func(delta string) {
			p.delta(delta)
			totalOutput.WriteString(delta)
		}
		if richRunner, ok := runner.(interface {
			RunMessageWithStream(context.Context, api.Message, func(string), func(string)) (string, error)
		}); ok {
			reply, err = richRunner.RunMessageWithStream(ctx, userMsg, onDelta, p.reasoning)
		} else {
			if len(userMsg.Parts) > 0 {
				err = fmt.Errorf("当前运行器不支持附件消息")
			} else {
				reply, err = runner.RunWithStream(ctx, userMsg.Content, onDelta)
			}
		}
		p.stopSpinner()

		if err == nil {
			finalErr = nil
			break
		}
		finalErr = err
		// Retrying is safe even after part of the answer streamed: the engine
		// keeps the steps it completed and resumes when the same message is
		// sent again, instead of re-running the turn from the start.
		if attempt == maxAttempts || ctx.Err() != nil || !isTransientRequestError(err) {
			break
		}
		note := fmt.Sprintf("\n网络波动，自动重试中 (%d/%d)...\n", attempt, maxAttempts)
		if p.gotDelta() {
			note = fmt.Sprintf("\n网络波动，从中断处继续 (%d/%d)...\n", attempt, maxAttempts)
		}
		p.system(termui.Yellow + note + termui.Reset)
		totalOutput.WriteString(note)
		time.Sleep(time.Duration(attempt) * 1200 * time.Millisecond)
	}

	if finalErr == nil {
		noteTurnCompleted() // the session_end dream counts this process's turns
		if missing := missingStreamedSuffix(reply, totalOutput.String()); missing != "" {
			p.delta(missing)
			totalOutput.WriteString(missing)
		}
	}
	if finalErr != nil {
		errMsg, color := turnErrorLine(finalErr, p.errorText(finalErr))
		p.system(color + errMsg + termui.Reset)
		totalOutput.WriteString(errMsg)
	}
	// One dim line of what the turn took: time, tokens, cost, context use.
	if usage != nil {
		if line := usage.summary(); line != "" {
			p.stopSpinner()
			p.system("\n  " + termui.Styled(termui.Dim, line))
		}
	}
	totalOutput.WriteString("\r\n\r\n")
	return totalOutput.String(), finalErr
}

// turnErrorLine is the line that ends a failed turn, and its color. A turn
// stopped at its limit (the user answered "s", or the prompt timed out) did
// not fail, so it gets a neutral prefix instead of "请求失败".
func turnErrorLine(err error, text string) (string, string) {
	var le *engine.LimitError
	if errors.As(err, &le) {
		if le.Reason == engine.LimitReasonStagnation {
			return "\n" + text, termui.Yellow // the text says it was stopped
		}
		return "\n本轮已停止：" + text, termui.Yellow
	}
	return fmt.Sprintf("\n请求失败：%s", text), termui.Red
}

// turnPrinter owns the terminal while one turn streams: the spinner, the
// model's text and reasoning, live tool output and the engine's tool blocks
// all come through it, from the engine's goroutines.
//
// It exists because those four sources used to write independently and
// trampled each other:
//
//   - A tool block arriving while the "思考中" spinner animated was printed
//     after the spinner frame, on the same row ("⠋ 思考中...  ▸ bash ls").
//   - The model's "我来看看文件：" has no trailing newline, so the tool block
//     that follows was glued to it, and so was the first answer delta after a
//     shown reasoning trace.
//   - Everything was printed verbatim, so an escape sequence in the model's
//     reply or in a command's output was executed by the terminal (see
//     render.SanitizeStream).
//
// So it tracks which source printed last and whether the cursor is mid-row,
// breaks the row when the source changes, keeps the spinner off any row that
// holds text (a spinner frame starts with \r ESC[K and would erase it), and
// sanitises every untrusted chunk.
type turnPrinter struct {
	mu             sync.Mutex
	spinner        *termui.Spinner
	textStarted    bool // a text delta was printed in this attempt
	anyDelta       bool
	reasoningChars int
	last           outputKind
	atLineStart    bool
	compactTools   bool
	previewLines   int
	previewCells   int
	previewFolded  bool

	// One sanitiser per stream, so a sequence split across two chunks of
	// the same stream is reassembled rather than half-printed. A command's
	// stdout and stderr are separate streams (progress, progressErr): they
	// used to share one sanitiser, and bytes held from one were completed by
	// the other's next chunk.
	text, thought, progress, progressErr render.StreamSanitizer

	// md renders the answer's Markdown as it streams (headings, bold, code
	// blocks, bullets). It runs after the sanitiser, so the only escapes it
	// sees are the model's own colour codes.
	md *render.MarkdownStream

	// status, when set, is the live status after the spinner's message
	// (elapsed time, context use, cost: turnUsage.live).
	status func() string
}

// outputKind is the source of the last thing printed.
type outputKind int

const (
	outNone outputKind = iota
	outText
	outReasoning
	outProgress
	outEngine
	outSystem
)

// newTurnPrinter returns a printer for a turn. termui.BeginOutput has just
// moved to a fresh row, so the cursor starts at the beginning of one.
func newTurnPrinter() *turnPrinter {
	return &turnPrinter{atLineStart: true, md: render.NewMarkdownStream()}
}

// beginAttempt starts the "思考中" spinner for a new request attempt.
func (p *turnPrinter) beginAttempt() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.spinner != nil {
		p.spinner.Stop()
	}
	// A reply cut off inside a code fence (truncated, or failed and retried)
	// must not leave the next answer drawn as code: print what the renderer
	// still holds, end its row, and start the attempt with a fresh renderer.
	p.flushHeldTextLocked()
	// Likewise the reasoning sanitiser: it was flushed only in stop(), so
	// the bytes a failed attempt held back were completed by the retry's
	// first reasoning chunk.
	p.flushThoughtLocked()
	p.ensureLineStartLocked()
	p.md = render.NewMarkdownStream()
	p.spinner = termui.NewSpinner("思考中...")
	if p.status != nil {
		p.spinner.SetSuffix(p.status)
	}
	p.textStarted = false
	p.reasoningChars = 0
	p.startSpinnerLocked()
}

// startSpinnerLocked starts the spinner only on an empty row: each frame
// begins with \r ESC[K, which would wipe text already on the row.
func (p *turnPrinter) startSpinnerLocked() {
	if p.spinner != nil && p.atLineStart {
		p.spinner.Start()
	}
}

func (p *turnPrinter) stopSpinnerLocked() {
	if p.spinner != nil {
		p.spinner.Stop()
	}
}

func (p *turnPrinter) stopSpinner() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.stopSpinnerLocked()
}

// stop ends the turn's output.
func (p *turnPrinter) stop() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.stopSpinnerLocked()
	// Unconditionally: a reply that was only a held-back marker printed
	// nothing, so the printer never switched to text.
	p.flushHeldTextLocked()
	p.flushProgressLocked()
	p.flushThoughtLocked()
	if p.last == outReasoning {
		termui.StreamPrint(termui.Reset)
	}
}

// flushProgressLocked prints what the live-output sanitizer still holds (the
// start of a character the command's last chunk ended in).
func (p *turnPrinter) flushProgressLocked() {
	for _, z := range []*render.StreamSanitizer{&p.progress, &p.progressErr} {
		if rest := z.Flush(); rest != "" {
			p.switchToLocked(outProgress)
			p.printToolProgressLocked(rest)
		}
	}
}

// flushThoughtLocked prints what the reasoning sanitizer still holds.
func (p *turnPrinter) flushThoughtLocked() {
	if rest := p.thought.Flush(); rest != "" {
		p.switchToLocked(outReasoning)
		p.printLocked(termui.ReasoningStyle + rest + termui.Reset)
	}
}

// flushTextLocked prints what the Markdown renderer still holds back (a
// possible marker at the end of the last chunk) and closes the styles of the
// line it was on, so the next source does not inherit bold or reverse video.
func (p *turnPrinter) flushTextLocked() {
	p.printLocked(p.md.Flush())
}

// flushHeldTextLocked is flushTextLocked for the end of an attempt or turn,
// where the printer may not have switched to text yet: held-back text (a
// reply that was only "*", or its last marker) is text, so the printer
// switches to it first. Printed straight after a reasoning trace, it used to
// share the trace's row and, until the closing reset, its style.
func (p *turnPrinter) flushHeldTextLocked() {
	// The sanitizer holds back a character split across chunks until its
	// next bytes arrive; at the end of the stream they never will.
	held := p.md.Write(p.text.Flush()) + p.md.Flush()
	if held == "" {
		return
	}
	if render.StripControls(held) != "" {
		p.switchToLocked(outText)
	}
	p.printLocked(held)
}

func (p *turnPrinter) gotDelta() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.anyDelta
}

// printLocked writes s and records whether it left the cursor mid-row.
func (p *turnPrinter) printLocked(s string) {
	if s == "" {
		return
	}
	termui.StreamPrint(s)
	if plain := render.StripControls(s); plain != "" {
		p.atLineStart = strings.HasSuffix(plain, "\n")
	}
}

func (p *turnPrinter) ensureLineStartLocked() {
	if !p.atLineStart {
		termui.StreamPrint("\n")
		p.atLineStart = true
	}
}

// switchToLocked starts output from source k on a fresh row when a different
// source printed last. The answer after a reasoning trace also gets a blank
// line, so the two read as separate blocks.
func (p *turnPrinter) switchToLocked(k outputKind) {
	prev := p.last
	if prev == k {
		return
	}
	if prev == outText {
		p.flushTextLocked()
	}
	p.last = k
	if prev == outReasoning {
		termui.StreamPrint(termui.Reset)
	}
	p.ensureLineStartLocked()
	if prev == outReasoning && k == outText {
		termui.StreamPrint("\n")
	}
}

func (p *turnPrinter) delta(s string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.textStarted {
		p.stopSpinnerLocked()
	}
	p.textStarted = true
	p.anyDelta = true
	out := p.md.Write(p.text.Write(s))
	if out == "" {
		return
	}
	p.switchToLocked(outText)
	p.printLocked(out)
}

func (p *turnPrinter) reasoning(s string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !showReasoning {
		p.reasoningChars += utf8.RuneCountInString(s)
		if !p.textStarted && p.spinner != nil {
			p.spinner.SetMessage(reasoningStatus(p.reasoningChars))
		}
		return
	}
	if !p.textStarted {
		p.stopSpinnerLocked()
	}
	out := p.thought.Write(s)
	if out == "" {
		return
	}
	p.switchToLocked(outReasoning)
	p.printLocked(termui.ReasoningStyle + out + termui.Reset)
}

// toolOutputStart opens the live output of a tool call with a dim header
// naming the command. The engine's own block for the call (the "✓ Command:
// … · 共 5 行" line) arrives only when the call has finished, so the raw
// output used to appear first, as loose lines with nothing saying where
// they came from; now they read as the output of a named command, indented
// under its header.
func (p *turnPrinter) toolOutputStart(toolName, header string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.stopSpinnerLocked()
	// The previous command's held-back bytes belong under its own header.
	p.flushProgressLocked()
	p.previewLines, p.previewCells, p.previewFolded = 0, 0, false
	line := "  ▸ " + toolName
	if h := strings.TrimSpace(render.StripControls(strings.ReplaceAll(header, "\n", " "))); h != "" {
		line += " " + h
	}
	p.switchToLocked(outProgress)
	p.printLocked(termui.Dim + line + "  实时输出:" + termui.Reset + "\n")
}

// toolProgressIndent is what live tool output is indented by, under the
// header toolOutputStart printed.
const toolProgressIndent = "    "

const toolPreviewLines = 8
const toolPreviewCells = 160

func (p *turnPrinter) printToolProgressLocked(output string) {
	if p.compactTools {
		if p.previewFolded {
			return
		}
		var preview strings.Builder
		for _, part := range strings.SplitAfter(output, "\n") {
			if part == "" {
				continue
			}
			if p.previewLines >= toolPreviewLines {
				p.previewFolded = true
				break
			}
			line := strings.TrimSuffix(part, "\n")
			remaining := toolPreviewCells - p.previewCells
			visible := textutil.TruncateWidth(line, remaining, "")
			preview.WriteString(visible)
			p.previewCells += textutil.Width(visible)
			if textutil.Width(line) > remaining {
				p.previewFolded = true
				break
			}
			if strings.HasSuffix(part, "\n") {
				preview.WriteByte('\n')
				p.previewLines++
				p.previewCells = 0
			}
		}
		output = preview.String()
		if p.previewFolded {
			if !strings.HasSuffix(output, "\n") && (output != "" || !p.atLineStart) {
				output += "\n"
			}
			output += "后续实时输出已折叠\n"
		}
	}
	if output != "" {
		p.printLocked(termui.Dim + indentLines(output, toolProgressIndent, p.atLineStart) + termui.Reset)
	}
}

// toolProgress surfaces live output from long-running tools (bash,
// powershell) so the user can tell what a slow command is doing. The
// command's own colour survives; its control sequences do not.
func (p *turnPrinter) toolProgress(toolName, chunk string) {
	p.toolStreamProgress(&p.progress, chunk)
}

// toolStderrProgress is toolProgress for the command's stderr, through its
// own sanitiser.
func (p *turnPrinter) toolStderrProgress(toolName, chunk string) {
	p.toolStreamProgress(&p.progressErr, chunk)
}

func (p *turnPrinter) toolStreamProgress(z *render.StreamSanitizer, chunk string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.stopSpinnerLocked()
	out := z.Write(chunk)
	if out == "" {
		return
	}
	p.switchToLocked(outProgress)
	p.printToolProgressLocked(out)
}

// indentLines puts indent at the start of every line of s: at its beginning
// when the cursor is at the start of a row, and after every newline that
// more text follows. Chunks split lines anywhere, so the next chunk's
// leading indent is decided by where this one left the cursor.
func indentLines(s, indent string, atLineStart bool) string {
	var sb strings.Builder
	if atLineStart {
		sb.WriteString(indent)
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		sb.WriteByte(c)
		if c == '\n' && i+1 < len(s) {
			sb.WriteString(indent)
		}
	}
	return sb.String()
}

// engineLine prints one of the engine's lines (a tool block, a stall or
// safety notice). They carry untrusted text — the command, a file's first
// line — so they are sanitised too; a line without a trailing newline is
// given one so it does not swallow the next output. While the model has not
// started its answer, the spinner comes back underneath: the next step is the
// model thinking again.
func (p *turnPrinter) engineLine(line string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.stopSpinnerLocked()
	out := render.SanitizeStream(line)
	if out == "" {
		return
	}
	p.switchToLocked(outEngine)
	p.printLocked(out)
	p.ensureLineStartLocked()
	if !p.textStarted {
		p.startSpinnerLocked()
	}
}

// system prints cove's own status text (retry notes, the failure line).
func (p *turnPrinter) system(s string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.switchToLocked(outSystem)
	p.printLocked(s)
}

// errorText is an error for display. A provider's error body is echoed in
// it, so it is untrusted like any other text from the network.
func (p *turnPrinter) errorText(err error) string {
	return render.StripControls(interactiveErrorText(err))
}

func (p *turnPrinter) permissionPause() { p.stopSpinner() }

// permissionDone brings the indicator back once the prompt is answered, but
// only while nothing has been streamed yet: once the model has started
// talking, an animated spinner would overwrite the reply.
func (p *turnPrinter) permissionDone() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.textStarted {
		p.startSpinnerLocked()
	}
}

func missingStreamedSuffix(reply, streamed string) string {
	if reply == "" || reply == streamed {
		return ""
	}
	if strings.HasPrefix(reply, streamed) {
		return reply[len(streamed):]
	}
	maxOverlap := len(reply)
	if len(streamed) < maxOverlap {
		maxOverlap = len(streamed)
	}
	for i := maxOverlap; i > 0; i-- {
		if strings.HasSuffix(streamed, reply[:i]) {
			return reply[i:]
		}
	}
	return ""
}

func isBudgetExceededError(err error) bool {
	return errors.Is(err, engine.ErrBudgetExceeded)
}

func budgetExceededRetryHint(tr *cost.Tracker) string {
	if tr != nil {
		suggested := tr.SuggestedBudget()
		if suggested > 0 {
			return fmt.Sprintf("预算已超限，继续重试不会成功。可执行 /budget auto 一键提高到 $%.2f，或手动 /budget <金额>，然后再输入“继续”。", suggested)
		}
	}
	return "预算已超限，继续重试不会成功。请先执行 /budget auto 或 /budget <更大金额>，然后再输入“继续”。"
}

// showReasoning streams the model's full reasoning into the conversation when
// true (config show_reasoning). By default a thinking model's reasoning is
// summarised in the status line instead: it can run to pages of scratch work
// that bury the answer.
var showReasoning bool

// reasoningStatus is the status-line text shown while a model is reasoning.
func reasoningStatus(chars int) string {
	return fmt.Sprintf("思考中… 已推理 %d 字", chars)
}

// installBackgroundSummary shows, after a turn, one dim line about the
// background work that followed it (backgroundSummaryLine), only when stdout
// is a terminal: -p and headless runs never install it, and a redirected
// stdout must not collect status lines.
func installBackgroundSummary(eng *engine.Engine, stdoutIsTerminal bool) {
	if eng == nil || !stdoutIsTerminal {
		return
	}
	eng.OnBackgroundSummary = printBackgroundSummary
}

// printBackgroundSummary shows the summary line above the input line. It is
// called from the engine's background goroutine, usually seconds after the
// turn it describes: between turns it prints at once, during a turn it is held
// until that turn's output ends (bgSummaries), so it never lands inside a
// streaming answer.
func printBackgroundSummary(s engine.BackgroundSummary) {
	if line := backgroundSummaryLine(s); line != "" {
		bgSummaries.deliver("  " + termui.Styled(termui.Dim, line) + "\n")
	}
}

// summaryQueue holds background summary lines while a turn is printing.
type summaryQueue struct {
	mu      sync.Mutex
	active  int // turns printing (runChatInteractionMessage calls in flight)
	pending []string
}

// bgSummaries is the queue printBackgroundSummary goes through.
var bgSummaries = &summaryQueue{}

func (q *summaryQueue) deliver(line string) {
	q.mu.Lock()
	if q.active > 0 {
		q.pending = append(q.pending, line)
		q.mu.Unlock()
		return
	}
	q.mu.Unlock()
	termui.PrintAbove(line)
}

func (q *summaryQueue) beginTurn() {
	q.mu.Lock()
	q.active++
	q.mu.Unlock()
}

// endTurn prints what arrived during the turn, before the prompt returns.
func (q *summaryQueue) endTurn() {
	q.mu.Lock()
	if q.active > 0 {
		q.active--
	}
	var lines []string
	if q.active == 0 {
		lines, q.pending = q.pending, nil
	}
	q.mu.Unlock()
	for _, l := range lines {
		termui.PrintAbove(l)
	}
}

// turnModelLine is the dim status line naming the model a turn was routed
// to (shown only when routing chooses between a fast and a main model).
func turnModelLine(model string) string {
	return "  " + termui.Styled(termui.Dim, "模型："+model)
}

// backgroundSummaryLine is e.g. "已提取 2 条记忆 · dream 还差 2 个会话": the
// memories saved, the dream gate when it moved, old sessions max_sessions
// pruning deleted, a failed session save. ""
// when there is nothing to say.
func backgroundSummaryLine(s engine.BackgroundSummary) string {
	var parts []string
	if s.MemoriesExtracted > 0 {
		parts = append(parts, fmt.Sprintf("已提取 %d 条记忆", s.MemoriesExtracted))
	}
	if s.DreamChanged {
		st := s.DreamStatus
		switch {
		case s.DreamFired || st.Running:
			parts = append(parts, "dream 已开始整理记忆")
		case st.SessionsNeeded() > 0:
			parts = append(parts, fmt.Sprintf("dream 还差 %d 个会话", st.SessionsNeeded()))
		case st.HoursNeeded() > 0:
			parts = append(parts, fmt.Sprintf("dream 还差 %.1f 小时", st.HoursNeeded()))
		}
	}
	if s.SessionsPruned > 0 {
		parts = append(parts, fmt.Sprintf("已清理 %d 个旧会话（max_sessions=%d）", s.SessionsPruned, s.MaxSessions))
	}
	if len(s.NewSkills) > 0 {
		parts = append(parts, "新增技能 "+strings.Join(s.NewSkills, "、"))
	}
	if len(s.UpdatedSkills) > 0 {
		parts = append(parts, "更新技能 "+strings.Join(s.UpdatedSkills, "、"))
	}
	parts = append(parts, s.Extra...)
	if !s.SessionSaved {
		parts = append(parts, "会话保存失败（详见日志）")
	}
	return strings.Join(parts, " · ")
}
