package main

import (
	"strings"

	"github.com/liuzhixin405/cove-agent/internal/repl"
	"github.com/liuzhixin405/cove-agent/internal/termui"
)

// pendingConfirm is a confirmation box waiting for the next line.
type pendingConfirm struct {
	title string
	onYes func()
	token uint64
}

// confirmThen is the one way a command asks "really?": the same gutter box
// as the approval prompt, the preview lines in it, and a one-key answer. It
// replaced three forms (an unconfirmed /undo, a typed "confirm" word, a
// preview ID to type back) with one the person learns once.
//
// It returns at once. Slash commands run on the input loop itself, so a
// confirmation cannot block waiting for that loop to read the answer (the
// permission prompt can: it is raised from the task goroutine). The next
// line — or the y/n key alone — is routed by takePendingConfirm, the way
// /history's number pick is. false when nothing can answer (headless, -p):
// the caller then keeps its typed-word or two-step form.
//
// It is refused while a task runs: the task's permission prompt and the box
// would share the relay, and a y meant for one would reach the other. The
// person is told to try again when the task is done.
func (fe *frontend) confirmThen(title string, preview []string, onYes func()) bool {
	if !fe.interactive() || !replInteractive {
		return false
	}
	if fe.running() {
		fe.print(termui.Styled(termui.Dim, "任务运行中，"+title+"需要确认，任务结束后再试。"))
		return true
	}
	token, ok := repl.ArmPrompt("等待确认", append([]string{"确认: " + title}, preview...), "等待确认：y 确认 / n 取消", "yn")
	if !ok {
		fe.print(termui.Styled(termui.Dim, "另一个提示正在等待回答，"+title+"未开始；回答它之后再试。"))
		return true
	}
	fe.pendingConfirm = &pendingConfirm{title: title, onYes: onYes, token: token}
	termui.PrintAbove(termui.GutterBox("需要确认", title, strings.Join(preview, "\n"), true) +
		termui.PromptContentIndent + termui.Styled(termui.Bold, "[y] 确认   [n] 取消") + "\n")
	return true
}

// takePendingConfirm resolves a waiting confirmation with the typed line:
// y runs it, n (or an empty line) cancels, anything else cancels and is the
// person's next input (false: the caller handles the line as usual). A task
// that started meanwhile cancels it too: the action would run under the
// task's edits.
func (fe *frontend) takePendingConfirm(input string) bool {
	p := fe.pendingConfirm
	if p == nil {
		return false
	}
	fe.pendingConfirm = nil
	repl.DisarmPrompt(p.token)
	switch strings.ToLower(strings.TrimSpace(input)) {
	case "y", "yes", "是", "确认":
		if fe.running() {
			fe.print(termui.Styled(termui.Dim, "任务运行中，已取消："+p.title+"，任务结束后再试。"))
			return true
		}
		p.onYes()
		return true
	case "n", "no", "否", "取消", "":
		fe.print(termui.Styled(termui.Dim, "已取消："+p.title))
		return true
	}
	fe.print(termui.Styled(termui.Dim, "已取消："+p.title+"（收到其他输入，按普通输入处理）"))
	return false
}

// cancelPendingConfirm drops a waiting confirmation (Ctrl+C, Esc).
func (fe *frontend) cancelPendingConfirm() {
	p := fe.pendingConfirm
	if p == nil {
		return
	}
	fe.pendingConfirm = nil
	repl.DisarmPrompt(p.token)
	fe.print(termui.Styled(termui.Dim, "已取消："+p.title))
}
