package main

import (
	"strings"
	"testing"

	"github.com/liuzhixin405/cove-agent/internal/repl"
	"github.com/liuzhixin405/cove-agent/internal/termui"
)

func confirmTestFrontend(t *testing.T) (*frontend, *lockedBuffer) {
	t.Helper()
	out := &lockedBuffer{}
	termui.SetWriter(out)
	oldInteractive := replInteractive
	replInteractive = true
	t.Cleanup(func() { replInteractive = oldInteractive; repl.ClearPermInputCh(); termui.SetWriter(nil) })
	fe := &frontend{tasks: &replTaskRunner{}, print: func(s string) { termui.PrintAbove(s + "\n") }}
	return fe, out
}

// confirmThen shows the box and returns; the next line decides.
func TestConfirmThenRunsOnYesOnly(t *testing.T) {
	fe, out := confirmTestFrontend(t)
	ran := 0
	if !fe.confirmThen("整树回退到检查点", []string{"目标：最近检查点", "涉及 3 个文件"}, func() { ran++ }) {
		t.Fatal("interactive front end must arm the confirmation")
	}
	if s := out.String(); !strings.Contains(s, "需要确认") || !strings.Contains(s, "涉及 3 个文件") || !strings.Contains(s, "[y] 确认   [n] 取消") {
		t.Fatalf("box = %q", s)
	}
	if ran != 0 {
		t.Fatal("nothing may run before the answer")
	}
	if !fe.takePendingConfirm("n") || ran != 0 || !strings.Contains(out.String(), "已取消：整树回退到检查点") {
		t.Fatalf("n: ran=%d out=%q", ran, out.String())
	}
	if fe.takePendingConfirm("y") {
		t.Fatal("a second line must not resolve a confirmation that is over")
	}

	fe.confirmThen("x", []string{"p"}, func() { ran++ })
	if !fe.takePendingConfirm(" Y ") || ran != 1 {
		t.Fatalf("y: ran=%d", ran)
	}

	// Any other line cancels and is the person's next input.
	fe.confirmThen("x", []string{"p"}, func() { ran++ })
	if fe.takePendingConfirm("/help") || ran != 1 || !strings.Contains(out.String(), "按普通输入处理") {
		t.Fatalf("other input: ran=%d out=%q", ran, out.String())
	}

	// Ctrl+C cancels too.
	fe.confirmThen("x", []string{"p"}, func() { ran++ })
	fe.cancelPendingConfirm()
	if fe.takePendingConfirm("y") || ran != 1 {
		t.Fatal("cancelled confirmation must not run")
	}
}

func TestConfirmThenNotInteractive(t *testing.T) {
	oldInteractive := replInteractive
	replInteractive = false
	t.Cleanup(func() { replInteractive = oldInteractive })
	fe := &frontend{print: func(string) {}}
	if fe.confirmThen("x", nil, func() {}) {
		t.Fatal("headless must not arm a confirmation")
	}
}

// A confirmation is refused while a task runs: its y would otherwise be
// taken for a permission prompt's, and the action would run under the task.
func TestConfirmThenRefusesWhileATaskRuns(t *testing.T) {
	fe, out := confirmTestFrontend(t)
	fe.tasks.running = true
	ran := 0
	// Refusing is "handled" (true): the caller must not fall through to its
	// unconfirmed form, as it does when nothing can answer at all.
	if !fe.confirmThen("x", []string{"p"}, func() { ran++ }) {
		t.Fatal("a refusal while running must count as handled")
	}
	if !strings.Contains(out.String(), "任务结束后再试") || fe.pendingConfirm != nil {
		t.Fatalf("out=%q pending=%v", out.String(), fe.pendingConfirm)
	}
	fe.tasks.running = false
	fe.confirmThen("x", []string{"p"}, func() { ran++ })
	fe.tasks.running = true
	if !fe.takePendingConfirm("y") || ran != 0 || !strings.Contains(out.String(), "任务运行中") {
		t.Fatalf("y during a task must not run the action: ran=%d out=%q", ran, out.String())
	}
}
