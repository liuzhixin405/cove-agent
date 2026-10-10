package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/liuzhixin405/cove-agent/internal/checkpoint"
	"github.com/liuzhixin405/cove-agent/internal/config"
	"github.com/liuzhixin405/cove-agent/internal/repl"
	"github.com/liuzhixin405/cove-agent/internal/termui"
)

// interactiveUndoFixture is a project with one checkpoint ("before") and a
// file changed since, and an interactive front end over it.
func interactiveUndoFixture(t *testing.T) (fe *frontend, path, target string, out *lockedBuffer) {
	t.Helper()
	eng := steerTestEngine(t)
	cwd, _ := os.Getwd()
	path = filepath.Join(cwd, "undo_me.txt")
	if err := os.WriteFile(path, []byte("before"), 0600); err != nil {
		t.Fatal(err)
	}
	manager, err := checkpoint.New(cwd)
	if err != nil {
		t.Fatal(err)
	}
	target, err = manager.Create("before")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("after"), 0600); err != nil {
		t.Fatal(err)
	}
	out = &lockedBuffer{}
	termui.SetWriter(out)
	oldInteractive := replInteractive
	replInteractive = true
	t.Cleanup(func() { replInteractive = oldInteractive; repl.ClearPermInputCh(); termui.SetWriter(nil) })
	fe = &frontend{eng: eng, cfg: config.DefaultConfig(), tasks: &replTaskRunner{}, print: func(s string) { termui.PrintAbove(s + "\n") }}
	fe.install(registerAllCommands())
	return fe, path, target, out
}

// Interactive /undo asks first; "n" leaves the tree alone, "y" restores.
func TestUndoAsksBeforeWholeTreeRestore(t *testing.T) {
	fe, path, _, out := interactiveUndoFixture(t)

	fe.dispatch("/undo")
	if !strings.Contains(out.String(), "整树回退到检查点") || strings.Contains(out.String(), "已回退") {
		t.Fatalf("no confirmation box: %q", out.String())
	}
	if !fe.takePendingConfirm("n") || !strings.Contains(out.String(), "已取消") {
		t.Fatalf("cancel not reported: %q", out.String())
	}
	if data, _ := os.ReadFile(path); string(data) != "after" {
		t.Fatalf("n must not restore; file = %q", data)
	}

	fe.dispatch("/undo")
	if !fe.takePendingConfirm("y") {
		t.Fatal("y must resolve the confirmation")
	}
	if data, _ := os.ReadFile(path); string(data) != "before" {
		t.Fatalf("y must restore; file = %q; output: %s", data, out.String())
	}
	if !strings.Contains(out.String(), "已回退到最近检查点") {
		t.Fatalf("restore not reported: %q", out.String())
	}
}

// Interactive /undo files previews, then one key applies it — no preview ID
// to type back.
func TestUndoFilesConfirmsAndApplies(t *testing.T) {
	fe, path, target, out := interactiveUndoFixture(t)

	fe.dispatch("/undo files " + target + " undo_me.txt")
	if !strings.Contains(out.String(), "按预览回滚文件") || !strings.Contains(out.String(), "undo_me.txt") {
		t.Fatalf("preview box missing: %q", out.String())
	}
	if !fe.takePendingConfirm("y") {
		t.Fatal("y must resolve the confirmation")
	}
	if data, _ := os.ReadFile(path); string(data) != "before" {
		t.Fatalf("confirmed preview not applied; file = %q; output: %s", data, out.String())
	}
}

// A preview that fails (unknown checkpoint) is reported, not confirmed.
func TestUndoFilesPreviewFailureSkipsConfirmation(t *testing.T) {
	fe, _, _, out := interactiveUndoFixture(t)
	fe.dispatch("/undo files deadbeef undo_me.txt")
	if strings.Contains(out.String(), "需要确认") || !strings.Contains(out.String(), "预览失败") {
		t.Fatalf("output = %q", out.String())
	}
	if fe.pendingConfirm != nil {
		t.Fatal("a failed preview must not leave a confirmation pending")
	}
}
