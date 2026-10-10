package main

import (
	"bytes"
	"context"
	"image"
	"image/png"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/liuzhixin405/cove-agent/internal/api"
)

// ---------- C: /skill create does not overwrite ----------

// InstallSkill writes its placeholder unconditionally, so "/skill create x"
// for an existing skill replaced the user's SKILL.md with the template.
func TestSkillCreateDoesNotOverwriteAnExistingSkill(t *testing.T) {
	fe, _ := headlessFrontend(t)
	out := captureOut(t)
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(home, ".cove", "skills", "mine", "SKILL.md")
	if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, []byte("my own instructions"), 0o600); err != nil {
		t.Fatal(err)
	}
	if !fe.dispatch("/skill create mine") {
		t.Fatal("/skill create not dispatched")
	}
	if data, _ := os.ReadFile(file); string(data) != "my own instructions" {
		t.Fatalf("SKILL.md overwritten with %q", data)
	}
	if !strings.Contains(out.String(), "已存在") {
		t.Fatalf("no explanation: %q", out.String())
	}
	if msg := skillCreateConflict("loaded", map[string]string{"loaded": "x"}); !strings.Contains(msg, "已存在") {
		t.Fatalf("a loaded skill is not a conflict: %q", msg)
	}
	if msg := skillCreateConflict("fresh", nil); msg != "" {
		t.Fatalf("a new name is a conflict: %q", msg)
	}
}

// ---------- D: SIGTERM during a headless slash command ----------

// deliverOnNotify makes withInterrupt receive sig as soon as it registers.
func deliverOnNotify(t *testing.T, sig os.Signal) {
	t.Helper()
	old := notifyInterrupt
	notifyInterrupt = func(c chan<- os.Signal) { c <- sig }
	t.Cleanup(func() { notifyInterrupt = old })
}

func TestWithInterruptReportsTheSignal(t *testing.T) {
	captureOut(t)
	if sig := withInterrupt(func(context.Context) {}); sig != nil {
		t.Fatalf("no signal, got %v", sig)
	}
	deliverOnNotify(t, syscall.SIGTERM)
	sig := withInterrupt(func(ctx context.Context) {
		select {
		case <-ctx.Done():
		case <-time.After(5 * time.Second):
			t.Error("the signal did not cancel the command")
		}
	})
	if sig != syscall.SIGTERM {
		t.Fatalf("withInterrupt = %v, want SIGTERM", sig)
	}
}

// headlessRun runs the headless front end on input against model and
// returns its result and what it printed.
func headlessRun(t *testing.T, model *fakeModel, input string) (failed bool, stdout, stderr string) {
	t.Helper()
	e2eHome(t, model)
	resetE2EGlobals()
	t.Cleanup(resetE2EGlobals)
	app, err := bootstrapApp(false, "", false)
	if err != nil {
		t.Fatal(err)
	}
	app.eng.SetAutoExtract(false)
	outR, outW, _ := os.Pipe()
	errR, errW, _ := os.Pipe()
	oldOut, oldErr := os.Stdout, os.Stderr
	os.Stdout, os.Stderr = outW, errW
	outC, errC := make(chan string), make(chan string)
	go func() { b, _ := io.ReadAll(outR); outC <- string(b) }()
	go func() { b, _ := io.ReadAll(errR); errC <- string(b) }()
	failed = runHeadlessFrom(strings.NewReader(input), app, registerAllCommands(), "")
	os.Stdout, os.Stderr = oldOut, oldErr
	_ = outW.Close()
	_ = errW.Close()
	return failed, <-outC, <-errC
}

// SIGTERM during a slash command only cancelled the command, and the run
// went on to the next line; it now ends the run as it does during a turn.
func TestHeadlessEndsOnSIGTERMDuringASlashCommand(t *testing.T) {
	model := newFakeModel(t, fakeStep{Content: "不该被调用"})
	deliverOnNotify(t, syscall.SIGTERM)
	failed, _, stderr := headlessRun(t, model, "/tasks\n这一行不应再执行\n")
	if n := len(model.Requests()); n != 0 {
		t.Fatalf("the run went on after SIGTERM: model called %d times", n)
	}
	if !strings.Contains(stderr, "[已终止]") || !failed {
		t.Fatalf("failed=%v stderr=%q", failed, stderr)
	}
}

// The REPL keeps its behavior: the signal cancels the command, nothing more.
func TestInteractiveDispatchOnlyRecordsSIGTERM(t *testing.T) {
	fe, _ := headlessFrontend(t)
	fe.tasks = newREPLTaskRunner(fe.eng)
	captureOut(t)
	deliverOnNotify(t, syscall.SIGTERM)
	fe.dispatch("/tasks")
	if fe.exitRequested {
		t.Fatal("SIGTERM during a REPL command asked the loop to leave")
	}
}

// ---------- E: headless exit status ----------

// Headless exited 0 even when every turn failed.
func TestHeadlessReportsAFailedTurn(t *testing.T) {
	model := newFakeModel(t,
		fakeStep{Status: 401, Body: `{"error":{"message":"invalid api key","type":"authentication_error"}}`},
		fakeStep{Content: "第二个问题的回答"},
	)
	failed, stdout, stderr := headlessRun(t, model, "第一个问题\n第二个问题\n")
	if !failed {
		t.Fatalf("a failed turn was not reported; stderr=%q", stderr)
	}
	if !strings.Contains(stdout, "第二个问题的回答") {
		t.Fatalf("the run stopped at the failure: stdout=%q", stdout)
	}
}

func TestHeadlessSucceedsWhenEveryTurnDoes(t *testing.T) {
	model := newFakeModel(t, fakeStep{Content: "一"}, fakeStep{Content: "二"})
	if failed, _, stderr := headlessRun(t, model, "问一\n问二\n"); failed {
		t.Fatalf("reported failure; stderr=%q", stderr)
	}
}

// ---------- F: "继续" after a completed turn ----------

// "继续" after a completed turn used to resume the saved session that
// scored highest, which could be another one, and continue that.
func TestE2E_ContinueAfterACompletedTurnStaysInTheSession(t *testing.T) {
	model := newFakeModel(t,
		fakeStep{Content: "第一步完成了。"}, fakeStep{Content: "第二步完成了。"}, fakeStep{Content: "第三步完成了。"},
	)
	e2eHome(t, model)
	first := startREPL(t)
	for i, q := range []string{"旧任务：把整个项目的配置文件结构整理一遍", "旧任务继续整理第二部分", "旧任务继续整理第三部分"} {
		first.Type(q)
		first.WaitForCount("完成了。", i+1, e2eTimeout)
	}
	first.Type("exit")
	first.Exit()

	model.Append(fakeStep{Content: "我是一个编程助手。"}, fakeStep{Content: "好的，接着刚才的话题。"})
	second := startREPL(t)
	second.Type("你好，请简单介绍一下你自己吧")
	second.WaitFor("我是一个编程助手。", e2eTimeout)
	second.Type("继续")
	second.WaitFor("好的，接着刚才的话题。", e2eTimeout)
	if strings.Contains(second.Output(), "已自动恢复") {
		t.Fatalf("\"继续\" switched to another session:\n%s", second.Output())
	}
	reqs := model.Requests()
	last := reqs[len(reqs)-1]
	var sawCurrent bool
	for _, m := range last.Messages {
		if strings.Contains(m.Text(), "旧任务") {
			t.Fatalf("\"继续\" was sent into the old session: %q", requestTexts(last))
		}
		sawCurrent = sawCurrent || strings.Contains(m.Text(), "介绍一下你自己")
	}
	if !sawCurrent {
		t.Fatalf("\"继续\" did not go to the current conversation: %q", requestTexts(last))
	}
}

// ---------- G: no model switch while a task runs ----------

// An image typed during a task reloaded the provider under the running
// turn (ReloadProvider mid-request).
func TestE2E_ImageDuringATaskDoesNotSwitchTheModel(t *testing.T) {
	if testing.Short() {
		t.Skip("slow (>3s): skipped under -short")
	}
	model := newFakeModel(t, fakeStep{Delay: 3 * time.Second, Content: "慢活做完了，结果都在上面。"})
	_, project := e2eHome(t, model)
	writeTestPNG(t, filepath.Join(project, "shot.png"))
	s := startREPL(t)
	s.Type("做一件慢活")
	time.Sleep(500 * time.Millisecond)
	s.Type("看看这张图 @shot.png")
	s.WaitFor("任务运行中不能切换模型", e2eTimeout)
	if got := s.app.cfg.Model; got != "qwen-test" {
		t.Fatalf("model switched to %q during the task", got)
	}
	s.WaitFor("慢活做完了", e2eTimeout)
	time.Sleep(300 * time.Millisecond)
	if n := len(model.Requests()); n != 1 {
		t.Fatalf("model called %d times, want 1 (the image message was not sent)", n)
	}
}

func writeTestPNG(t *testing.T, path string) {
	t.Helper()
	var buf bytes.Buffer
	if err := png.Encode(&buf, image.NewRGBA(image.Rect(0, 0, 4, 4))); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, buf.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
}

// ---------- H: every resuming /history form waits for the task ----------

func TestHistoryResumeFormsWaitForTheRunningTask(t *testing.T) {
	for _, in := range []string{"/history 2", "/history all 2", "/history abc-session-id", "/history all abc-session-id"} {
		if !commandMutatesEngine(in) {
			t.Errorf("%s resumes a session but may run during a task", in)
		}
	}
	for _, in := range []string{"/history", "/history all", "/history detail 2", "/history all detail 2",
		"/history clean", "/history clear", "/history clear all confirm", "/history all clear", "/history delete 3"} {
		if commandMutatesEngine(in) {
			t.Errorf("%s is read-only (or leaves the session in use alone) but is refused during a task", in)
		}
	}
}

// ---------- I: -p only parses the prompt argument ----------

// Piped stdin was searched for @attachments: a log naming @babel/core failed
// the run, and a matching file would have been attached unasked.
func TestPrintModeDoesNotTakeAttachmentsFromStdin(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("file body"), 0o600); err != nil {
		t.Fatal(err)
	}
	piped := "npm ERR! peer @babel/core@7.0.0\nsee @a.txt\n\tindented"
	prompt := combinePromptAndStdin("解释 @a.txt", piped)
	msg, _, err := buildPrintModeMessage("解释 @a.txt", prompt, dir, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(msg.Parts) != 1 {
		t.Fatalf("parts = %d, want only the argument's @a.txt", len(msg.Parts))
	}
	if want := "解释\n\n" + strings.TrimSpace(piped); msg.Content != want {
		t.Fatalf("content = %q, want %q", msg.Content, want)
	}
	// Stdin alone: nothing is parsed.
	msg, _, err = buildPrintModeMessage("", combinePromptAndStdin("", piped), dir, nil, "")
	if err != nil || len(msg.Parts) != 0 || !strings.Contains(msg.Content, "@a.txt") {
		t.Fatalf("stdin-only: parts=%d content=%q err=%v", len(msg.Parts), msg.Content, err)
	}
}

// ---------- K: retry state and drafts stay with their session ----------

// The failed request "继续" retries survived /history <id>, so "继续" sent
// session B's failed request into session A.
func TestE2E_ContinueAfterSwitchingSessionsDoesNotCarryTheFailedRequest(t *testing.T) {
	model := newFakeModel(t, fakeStep{Content: "任务A已经完成。"})
	e2eHome(t, model)
	first := startREPL(t)
	first.Type("任务A：整理一下文档目录的结构")
	first.WaitFor("任务A已经完成。", e2eTimeout)
	idA := first.app.eng.SessionID()
	first.Type("exit")
	first.Exit()

	model.Append(
		fakeStep{Status: 401, Body: `{"error":{"message":"invalid api key","type":"authentication_error"}}`},
		fakeStep{Content: "在任务A上继续。"},
	)
	second := startREPL(t)
	second.Type("任务B：修复登录页面的样式问题")
	second.WaitFor("请求失败", e2eTimeout)
	time.Sleep(300 * time.Millisecond)
	second.Type("/history " + idA)
	second.WaitFor("已成功拉回历史会话", e2eTimeout)
	second.Type("继续")
	second.WaitFor("在任务A上继续。", e2eTimeout)
	reqs := model.Requests()
	last := reqs[len(reqs)-1]
	for _, m := range last.Messages {
		if strings.Contains(m.Text(), "任务B") {
			t.Fatalf("session B's failed request went into session A: %q", requestTexts(last))
		}
	}
}

// requestTexts is the non-system message texts of req, for failure output.
func requestTexts(req capturedRequest) []string {
	var out []string
	for _, m := range req.Messages {
		if m.Role != "system" {
			out = append(out, m.Role+": "+m.Text())
		}
	}
	return out
}

type fakeSessionState struct {
	has bool
	id  string
}

func (f fakeSessionState) HasMessages() bool { return f.has }
func (f fakeSessionState) SessionID() string { return f.id }

// The draft file is one per user; it is offered only in its own project,
// and in a conversation only when it is that conversation's.
func TestInterruptedDraftStaysInItsProjectAndSession(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	proj := t.TempDir()
	t.Chdir(proj)
	if err := saveInterruptedDraftFor(api.Message{Role: "user", Content: "重构配置加载"}, os.ErrDeadlineExceeded, "s1"); err != nil {
		t.Fatal(err)
	}
	if usableInterruptedDraft(fakeSessionState{has: true, id: "s1"}) == nil {
		t.Error("its own session did not get the draft")
	}
	if usableInterruptedDraft(fakeSessionState{has: false, id: "new"}) == nil {
		t.Error("an empty conversation (after a restart) did not get the draft")
	}
	if usableInterruptedDraft(fakeSessionState{has: true, id: "s2"}) != nil {
		t.Error("another session's conversation got the draft")
	}
	t.Chdir(t.TempDir())
	if usableInterruptedDraft(fakeSessionState{}) != nil {
		t.Error("another project got the draft")
	}
}

// A completed turn settles only its own session's draft in its own project;
// it used to delete the draft whatever it was. A task starting elsewhere does
// not overwrite another project's draft either.
func TestInterruptedDraftIsClearedOnlyByItsOwner(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	proj, other := t.TempDir(), t.TempDir()
	t.Chdir(proj)
	if err := saveInterruptedDraftFor(api.Message{Role: "user", Content: "重构配置加载"}, os.ErrDeadlineExceeded, "s1"); err != nil {
		t.Fatal(err)
	}
	has := func() bool { d, _ := loadInterruptedDraft(); return d != nil && d.UserContent == "重构配置加载" }

	_ = clearInterruptedDraftFor("s2")
	if !has() {
		t.Fatal("another session's completed turn cleared the draft")
	}
	t.Chdir(other)
	saveTaskStartDraft(api.Message{Role: "user", Content: "别的项目里的问题"}, "s3")
	if !has() {
		t.Fatal("a task starting in another project replaced the draft")
	}
	_ = clearInterruptedDraftFor("s1")
	if !has() {
		t.Fatal("a completed turn in another project cleared the draft")
	}
	t.Chdir(proj)
	_ = clearInterruptedDraftFor("s1")
	if d, _ := loadInterruptedDraft(); d != nil {
		t.Fatalf("its own session's completed turn left the draft: %+v", d)
	}

	// Task start: written with the session and the unfinished reason.
	saveTaskStartDraft(api.Message{Role: "user", Content: "生成接口文档"}, "s1")
	d, _ := loadInterruptedDraft()
	if d == nil || d.SessionID != "s1" || d.Error != taskUnfinishedReason {
		t.Fatalf("task-start draft = %+v", d)
	}
}

// ---------- L: secrets stay out of the input history ----------

func TestHistoryLineKeepsSecretsOut(t *testing.T) {
	for _, in := range []string{"/api-key sk-abc", "/API-KEY sk-abc", "/config provider.api_key sk-abc",
		"/config api_key sk", "/config web_search.api_key k", "/base-url https://u:p@host/v1"} {
		if historyLineKeeps(in) {
			t.Errorf("%q would be kept in the input history", in)
		}
	}
	for _, in := range []string{"/api-key", "/config model gpt-4o", "/base-url https://api.x.com/v1", "hello", "/model x"} {
		if !historyLineKeeps(in) {
			t.Errorf("%q would be dropped from the input history", in)
		}
	}
}
