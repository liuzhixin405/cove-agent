package main

// Flow F3 (test design 5.3): a session's life — /new, the /history list and
// its sub-commands, -r at start-up, "继续" across sessions and the
// interrupted-turn draft. One home and one fake model are shared; the
// subtests build on the sessions the earlier ones saved, so their order
// matters (/history clear runs last).

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// f3Unauthorized is a 401 answer: it is not retried, so the turn fails at
// once and leaves a failed request (and a draft) behind.
var f3Unauthorized = fakeStep{Status: 401, Body: `{"error":{"message":"invalid api key","type":"authentication_error"}}`}

func TestFlowF3_SessionLifecycle(t *testing.T) {
	model := newFakeModel(t)
	home, project := e2eHome(t, model)
	cfgDir := filepath.Join(home, ".cove")
	draftPath := filepath.Join(cfgDir, "interrupted.json")
	indexPath := filepath.Join(cfgDir, "sessions", "index.json")
	other := filepath.Join(home, "other")
	if err := os.MkdirAll(other, 0o755); err != nil {
		t.Fatal(err)
	}

	start := func(t *testing.T, steps ...fakeStep) (*e2eSession, flow) {
		t.Helper()
		model.Reset(steps...)
		_ = os.Remove(draftPath)
		s := startREPL(t)
		return s, flow{t: t, s: s, model: model}
	}
	// saved runs one finished turn in a REPL of its own and returns the
	// session's ID; the REPL has exited, so the session is on disk.
	saved := func(t *testing.T, question, answer string) string {
		t.Helper()
		s, _ := start(t, textReply(answer))
		s.Type(question)
		s.WaitFor(answer, e2eTimeout)
		s.WaitIdle(e2eTimeout)
		id := s.app.eng.SessionID()
		s.Exit()
		return id
	}
	readFile := func(t *testing.T, path string) string {
		t.Helper()
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		return string(b)
	}
	// number is id's position in the list /history (all: /history all)
	// prints in the current directory.
	number := func(s *e2eSession, id string, all bool) string {
		records, _ := listHistoryRecords(s.app.eng.Store(), currentProjectDir(), all)
		for i, r := range records {
			if r.ID == id {
				return strconv.Itoa(i + 1)
			}
		}
		s.t.Fatalf("session %s not in the history list (all=%v)", id, all)
		return ""
	}

	// ---------- /new ----------

	t.Run("/new后请求不带旧消息且旧会话文件不变", func(t *testing.T) {
		s, f := start(t,
			textReply("第一件事已经处理完了，结果在上面，处理完成。"),
			textReply("第二件事也处理完了，与前面的对话无关，处理完成。"))
		s.Type("第一件事：旧对话里的暗号 zebra-f3")
		s.WaitFor("第一件事已经处理完了", e2eTimeout)
		s.WaitIdle(e2eTimeout)
		oldID := s.app.eng.SessionID()
		mark := s.Mark()
		s.Type("/new")
		s.WaitForSince(mark, "[新会话] 已开始新会话；上一个会话已保存（"+oldID+"）", e2eTimeout)
		oldFile := readFile(t, sessionPath(home, oldID))
		s.Type("第二件事：随便说点别的")
		s.WaitFor("第二件事也处理完了", e2eTimeout)
		s.WaitIdle(e2eTimeout)
		newID := s.app.eng.SessionID()
		if newID == oldID {
			f.Fatalf("/new kept session %s", oldID)
		}
		reqs := f.Requests(2)
		if n := countMessages(reqs[1], "", "zebra-f3"); n != 0 {
			f.Fatalf("the request after /new carries the old conversation: %q", requestTexts(reqs[1]))
		}
		if n := countMessages(reqs[1], "user", ""); n != 1 {
			f.Fatalf("the request after /new has %d user messages, want 1: %q", n, requestTexts(reqs[1]))
		}
		if got := readFile(t, sessionPath(home, oldID)); got != oldFile {
			f.Fatalf("the old session file changed after /new:\nbefore:\n%s\nafter:\n%s", oldFile, got)
		}
		rec := capturedFromRecord(sessionFile(t, home, newID))
		if countMessages(rec, "", "zebra-f3") != 0 || countMessages(rec, "user", "第二件事") != 1 {
			f.Fatalf("the new session file is wrong: %q", requestTexts(rec))
		}
	})

	// ---------- /history ----------

	var parserID string
	t.Run("/history列表后输入序号恢复", func(t *testing.T) {
		parserID = saved(t, "讲讲解析器的结构 marker-parser", "解析器分词法和语法两层，讲解完成。")
		s, f := start(t, textReply("接着解析器的话题，错误恢复靠同步点，补充完成。"))
		mark := s.Mark()
		s.Type("/history")
		s.WaitForSince(mark, "历史记录 (当前项目", e2eTimeout)
		s.WaitForSince(mark, "marker-parser", e2eTimeout)
		n := number(s, parserID, false)
		s.Type(n)
		s.WaitForSince(mark, "已成功拉回历史会话 #"+n, e2eTimeout)
		s.WaitForSince(mark, "解析器分词法和语法两层", e2eTimeout)
		if got := s.app.eng.SessionID(); got != parserID {
			f.Fatalf("the number resumed session %s, want %s", got, parserID)
		}
		s.Type("那错误恢复呢")
		s.WaitFor("补充完成。", e2eTimeout)
		s.WaitIdle(e2eTimeout)
		req := f.Requests(1)[0]
		if countMessages(req, "user", "marker-parser") != 1 || countMessages(req, "assistant", "解析器分词法和语法两层") != 1 {
			f.Fatalf("the resumed request lacks the old conversation: %q", requestTexts(req))
		}
		rec := capturedFromRecord(sessionFile(t, home, parserID))
		if countMessages(rec, "user", "那错误恢复呢") != 1 {
			f.Fatalf("the new exchange was not saved into the resumed session: %q", requestTexts(rec))
		}
	})

	t.Run("列表后输入非序号当普通消息且选号状态清除", func(t *testing.T) {
		s, f := start(t,
			textReply("这是一条普通消息，已经按普通问题回答，回答完成。"),
			textReply("数字 1 也按普通消息处理了，回答完成。"))
		id := s.app.eng.SessionID()
		mark := s.Mark()
		s.Type("/history")
		s.WaitForSince(mark, "历史记录 (当前项目", e2eTimeout)
		s.Type("这不是序号而是一个问题")
		s.WaitFor("已经按普通问题回答", e2eTimeout)
		s.WaitIdle(e2eTimeout)
		// The pick ended with that line: a number now is a message too.
		s.Type("1")
		s.WaitFor("数字 1 也按普通消息处理了", e2eTimeout)
		s.WaitIdle(e2eTimeout)
		f.Absent("已成功拉回历史会话")
		reqs := f.Requests(2)
		if last := lastMessage(reqs[0]); last.Role != "user" || !strings.HasPrefix(last.Text(), "这不是序号而是一个问题") {
			f.Fatalf("the line after the list was not sent as a message: %s %q", last.Role, last.Text())
		}
		if last := lastMessage(reqs[1]); last.Role != "user" || !strings.HasPrefix(last.Text(), "1") {
			f.Fatalf("\"1\" was not sent as a message: %s %q", last.Role, last.Text())
		}
		if got := s.app.eng.SessionID(); got != id {
			f.Fatalf("the session changed from %s to %s", id, got)
		}
		rec := capturedFromRecord(sessionFile(t, home, id))
		if countMessages(rec, "user", "这不是序号而是一个问题") != 1 {
			f.Fatalf("the message is not in the session file: %q", requestTexts(rec))
		}
	})

	t.Run("/history detail N显示会话详情且不恢复", func(t *testing.T) {
		s, f := start(t)
		id := s.app.eng.SessionID()
		before := readFile(t, sessionPath(home, parserID))
		mark := s.Mark()
		s.Type("/history detail " + number(s, parserID, false))
		s.WaitForSince(mark, "会话详情", e2eTimeout)
		s.WaitForSince(mark, "ID: "+parserID, e2eTimeout)
		s.WaitForSince(mark, "消息预览:", e2eTimeout)
		s.WaitForSince(mark, "marker-parser", e2eTimeout)
		if got := s.app.eng.SessionID(); got != id {
			f.Fatalf("/history detail resumed %s", got)
		}
		if got := readFile(t, sessionPath(home, parserID)); got != before {
			f.Fatalf("/history detail changed the session file")
		}
		f.Requests(0)
	})

	t.Run("/history delete N文件与索引都删", func(t *testing.T) {
		doomed := saved(t, "一个马上要删掉的会话 marker-doomed", "这个会话会被删掉，回答完成。")
		if !strings.Contains(readFile(t, indexPath), doomed) {
			t.Fatalf("index.json does not list %s before the delete", doomed)
		}
		s, f := start(t)
		mark := s.Mark()
		s.Type("/history")
		s.WaitForSince(mark, "marker-doomed", e2eTimeout)
		mark = s.Mark()
		s.Type("/history delete " + number(s, doomed, false))
		s.WaitForSince(mark, "已删除会话 "+doomed, e2eTimeout)
		if _, err := os.Stat(sessionPath(home, doomed)); !errors.Is(err, os.ErrNotExist) {
			f.Fatalf("the session file is still there: %v", err)
		}
		if strings.Contains(readFile(t, indexPath), doomed) {
			f.Fatalf("index.json still lists %s", doomed)
		}
		mark = s.Mark()
		s.Type("/history")
		s.WaitForSince(mark, "历史记录 (当前项目", e2eTimeout)
		s.WaitForSince(mark, "清空本项目历史", e2eTimeout)
		if strings.Contains(s.OutputSince(mark), "marker-doomed") {
			f.Fatalf("the deleted session is still listed")
		}
		if _, err := os.Stat(sessionPath(home, parserID)); err != nil {
			f.Fatalf("another session went with it: %v", err)
		}
		f.Requests(0)
	})

	// ---------- -r ----------

	t.Run("-r恢复后第一条请求带全部历史", func(t *testing.T) {
		s, _ := start(t,
			toolReply(bashCall("echo resume_tool_out")),
			textReply("第一轮：命令输出了 resume_tool_out，执行完成。"),
			textReply("第二轮：补充说明也写好了，回答完成。"))
		s.Type("跑一条命令 marker-resume")
		s.WaitFor("执行完成。", e2eTimeout)
		s.WaitIdle(e2eTimeout)
		s.Type("再补充一点说明")
		s.WaitFor("第二轮：", e2eTimeout)
		s.WaitIdle(e2eTimeout)
		id := s.app.eng.SessionID()
		s.Exit()
		rec := sessionFile(t, home, id)

		model.Reset(textReply("恢复后的第一问，接着前两轮继续，回答完成。"))
		s2 := startREPLResumed(t, id)
		f2 := flow{t: t, s: s2, model: model}
		s2.Type("恢复后的第一问")
		s2.WaitFor("接着前两轮继续", e2eTimeout)
		s2.WaitIdle(e2eTimeout)
		req := f2.Requests(1)[0]
		var sent []capturedMessage
		for _, m := range req.Messages {
			if m.Role != "system" {
				sent = append(sent, m)
			}
		}
		if len(sent) != len(rec.Messages)+1 {
			f2.Fatalf("the first request after -r carries %d messages, want the %d saved plus the new one: %q", len(sent), len(rec.Messages), requestTexts(req))
		}
		for i, m := range rec.Messages {
			if sent[i].Role != m.Role {
				f2.Fatalf("message %d is %s, saved as %s", i, sent[i].Role, m.Role)
			}
		}
		for _, want := range []string{"marker-resume", "resume_tool_out", "第一轮：", "再补充一点说明", "第二轮："} {
			if countMessages(req, "", want) == 0 {
				f2.Fatalf("the first request after -r lacks %q", want)
			}
		}
		if last := lastMessage(req); last.Role != "user" || !strings.HasPrefix(last.Text(), "恢复后的第一问") {
			f2.Fatalf("the new question is not last: %s %q", last.Role, last.Text())
		}
		if s2.app.eng.SessionID() != id {
			f2.Fatalf("-r continued %s, want %s", s2.app.eng.SessionID(), id)
		}
	})

	t.Run("-r不存在的ID报错且退出码非零", func(t *testing.T) {
		model.Reset()
		entries := func() int {
			es, _ := os.ReadDir(filepath.Join(cfgDir, "sessions"))
			return len(es)
		}
		before := entries()
		cmd := coveMainCommand(t, nil, "-r", "f3-no-such-session")
		cmd.Stdin = strings.NewReader("这一行不应被执行\n")
		done := make(chan struct{})
		var out []byte
		var err error
		go func() { out, err = cmd.CombinedOutput(); close(done) }()
		select {
		case <-done:
		case <-time.After(e2eTimeout):
			_ = cmd.Process.Kill()
			t.Fatalf("cove -r <missing> did not exit")
		}
		var exit *exec.ExitError
		if !errors.As(err, &exit) || exit.ExitCode() == 0 {
			t.Fatalf("exit = %v, want a non-zero exit code; output:\n%s", err, out)
		}
		if !strings.Contains(string(out), "无法恢复会话 f3-no-such-session: 没有这个会话") {
			t.Fatalf("no error naming the session; output:\n%s", out)
		}
		if n := entries(); n != before {
			t.Fatalf("the sessions directory went from %d to %d entries", before, n)
		}
		if n := len(model.Requests()); n != 0 {
			t.Fatalf("the model was called %d times\n%s", n, model.Summary())
		}
	})

	// ---------- "继续" across sessions ----------

	t.Run("跨会话继续不重发上一会话的失败请求", func(t *testing.T) {
		s, f := start(t,
			textReply("任务A已经完成，目录结构整理好了。"),
			f3Unauthorized,
			textReply("在任务A上接着整理，继续完成。"))
		s.Type("任务A：整理一下文档目录的结构")
		s.WaitFor("任务A已经完成", e2eTimeout)
		s.WaitIdle(e2eTimeout)
		idA := s.app.eng.SessionID()
		s.Type("/new")
		s.WaitFor("[新会话]", e2eTimeout)
		s.Type("任务B：修复登录页面的样式问题")
		s.WaitFor("API Key 无效或已过期", e2eTimeout)
		s.WaitIdle(e2eTimeout)
		mark := s.Mark()
		s.Type("/history")
		s.WaitForSince(mark, "历史记录 (当前项目", e2eTimeout)
		n := number(s, idA, false)
		s.Type(n)
		s.WaitForSince(mark, "已成功拉回历史会话 #"+n, e2eTimeout)
		s.Type("继续")
		s.WaitFor("继续完成。", e2eTimeout)
		s.WaitIdle(e2eTimeout)
		reqs := f.Requests(3)
		if countMessages(reqs[2], "", "任务B") != 0 {
			f.Fatalf("session B's failed request went into session A: %q", requestTexts(reqs[2]))
		}
		if countMessages(reqs[2], "user", "任务A") != 1 {
			f.Fatalf("\"继续\" did not go to session A: %q", requestTexts(reqs[2]))
		}
		rec := capturedFromRecord(sessionFile(t, home, idA))
		if countMessages(rec, "", "任务B") != 0 {
			f.Fatalf("session A's file carries session B's request: %q", requestTexts(rec))
		}
	})

	// ---------- the interrupted draft ----------

	t.Run("中断草稿不同cwd不串且回到原目录可恢复", func(t *testing.T) {
		s, f := start(t, f3Unauthorized)
		s.Type("草稿任务：把 README 翻译成英文 marker-draft")
		s.WaitFor("API Key 无效或已过期", e2eTimeout)
		s.WaitIdle(e2eTimeout)
		draftSession := s.app.eng.SessionID()
		s.Exit()
		if d := readFile(t, draftPath); !strings.Contains(d, "marker-draft") {
			f.Fatalf("no draft after the failed turn: %s", d)
		}

		// Another directory: not offered, and "继续" there does not take it.
		t.Chdir(other)
		model.Reset(textReply("另一个目录里没有可继续的任务，回答完成。"))
		s2 := startREPL(t)
		f2 := flow{t: t, s: s2, model: model}
		s2.WaitFor("❯", e2eTimeout)
		s2.Type("继续")
		s2.WaitIdle(e2eTimeout)
		f2.Absent("未完成的片段草稿", "marker-draft")
		for _, r := range model.Requests() {
			if countMessages(r, "", "marker-draft") != 0 {
				f2.Fatalf("the other directory's request carries the draft: %q", requestTexts(r))
			}
		}
		if d := readFile(t, draftPath); !strings.Contains(d, "marker-draft") {
			f2.Fatalf("the draft was consumed in another directory: %s", d)
		}
		s2.Exit()

		// Back in its own directory: offered, and "继续" resumes it.
		t.Chdir(project)
		model.Reset(textReply("README 已经翻译成英文，翻译完成。"))
		s3 := startREPL(t)
		f3 := flow{t: t, s: s3, model: model}
		s3.WaitFor("未完成的片段草稿", e2eTimeout)
		s3.Type("继续")
		s3.WaitFor("翻译完成。", e2eTimeout)
		s3.WaitIdle(e2eTimeout)
		req := f3.Requests(1)[0]
		if countMessages(req, "user", "marker-draft") != 1 {
			f3.Fatalf("the resumed request carries the draft %d times, want once: %q", countMessages(req, "user", "marker-draft"), requestTexts(req))
		}
		if got := s3.app.eng.SessionID(); got != draftSession {
			f3.Fatalf("the draft resumed in session %s, want its own %s", got, draftSession)
		}
		if _, err := os.Stat(draftPath); err == nil {
			f3.Fatalf("the draft is still there after the turn completed")
		}
	})

	t.Run("中断草稿在其他目录完成一轮后仍保留", func(t *testing.T) {
		s, f := start(t, f3Unauthorized)
		s.Type("另一份草稿：给配置加载补测试 marker-draft2")
		s.WaitFor("API Key 无效或已过期", e2eTimeout)
		s.WaitIdle(e2eTimeout)
		s.Exit()
		if d := readFile(t, draftPath); !strings.Contains(d, "marker-draft2") {
			f.Fatalf("no draft after the failed turn: %s", d)
		}
		t.Chdir(other)
		model.Reset(textReply("另一个目录里的问题已经回答，回答完成。"))
		s2 := startREPL(t)
		f2 := flow{t: t, s: s2, model: model}
		s2.Type("另一个目录里的问题")
		s2.WaitFor("回答完成。", e2eTimeout)
		s2.WaitIdle(e2eTimeout)
		f2.Requests(1)
		// The draft belongs to the project (it is offered only there); a
		// turn completed in another directory is not its completion.
		if b, err := os.ReadFile(draftPath); err != nil || !strings.Contains(string(b), "marker-draft2") {
			f2.Fatalf("a completed turn in another directory deleted the project's draft: %v %s", err, b)
		}
		s2.Exit()
		t.Chdir(project)
		model.Reset(textReply("配置加载的测试补好了，补充完成。"))
		s3 := startREPL(t)
		f3 := flow{t: t, s: s3, model: model}
		s3.WaitFor("未完成的片段草稿", e2eTimeout)
		s3.Type("继续")
		s3.WaitFor("补充完成。", e2eTimeout)
		if req := f3.Requests(1)[0]; countMessages(req, "user", "marker-draft2") != 1 {
			f3.Fatalf("the resumed request lacks the draft: %q", requestTexts(req))
		}
	})

	// killMidTask runs cove as a child process, sends it question and kills
	// it while the model is still answering: a crash or a power cut, with no
	// deferred save and no signal handler run.
	killMidTask := func(t *testing.T, question string) {
		t.Helper()
		model.Reset(fakeStep{Delay: 30 * time.Second, Content: "这条回答不会出现"})
		_ = os.Remove(draftPath)
		// COVE_TUI=1: the child takes the interactive shell (in the plain
		// reader, stdin being a pipe), as a person's cove does.
		cmd := coveMainCommand(t, []string{"COVE_TUI=1"})
		stdin, err := cmd.StdinPipe()
		if err != nil {
			t.Fatal(err)
		}
		// syncBuffer: tui_turn_output_test.go.
		var out syncBuffer
		cmd.Stdout, cmd.Stderr = &out, &out
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = cmd.Process.Kill() })
		_, _ = io.WriteString(stdin, question+"\n")
		model.WaitRequests(t, 1, e2eTimeout)
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		if !strings.Contains(out.String(), "思考中") {
			t.Fatalf("the child never started the task; output:\n%s", out.String())
		}
	}

	t.Run("进程被杀后请求已在会话中可从/history恢复", func(t *testing.T) {
		killMidTask(t, "崩溃任务：统计代码行数 marker-crash")
		model.Reset(textReply("代码行数统计好了，统计完成。"))
		s := startREPL(t)
		f := flow{t: t, s: s, model: model}
		// The request was saved when the turn began.
		records, _ := listHistoryRecords(s.app.eng.Store(), currentProjectDir(), false)
		killed := ""
		for _, r := range records {
			if strings.Contains(effectiveHistoryTitle(r), "marker-crash") {
				killed = r.ID
			}
		}
		if killed == "" {
			f.Fatalf("the killed task's session is not in the history list")
		}
		mark := s.Mark()
		s.Type("/history")
		s.WaitForSince(mark, "marker-crash", e2eTimeout)
		n := number(s, killed, false)
		s.Type(n)
		s.WaitForSince(mark, "已成功拉回历史会话 #"+n, e2eTimeout)
		s.Type("继续")
		s.WaitFor("统计完成。", e2eTimeout)
		s.WaitIdle(e2eTimeout)
		req := f.Requests(1)[0]
		if countMessages(req, "user", "marker-crash") != 1 {
			f.Fatalf("the resumed request does not carry the killed task once: %q", requestTexts(req))
		}
		rec := capturedFromRecord(sessionFile(t, home, killed))
		if countMessages(rec, "user", "marker-crash") != 1 || countMessages(rec, "assistant", "统计完成。") != 1 {
			f.Fatalf("the killed task's session does not hold the resumed exchange: %q", requestTexts(rec))
		}
	})

	t.Run("中断草稿进程被杀后下次启动提示", func(t *testing.T) {
		killMidTask(t, "崩溃任务：生成接口文档 marker-crash2")
		model.Reset(textReply("接口文档生成好了，生成完成。"))
		s := startREPL(t)
		f := flow{t: t, s: s, model: model}
		s.WaitFor("未完成的片段草稿", e2eTimeout)
		d := readFile(t, draftPath)
		if !strings.Contains(d, "marker-crash2") {
			f.Fatalf("the draft does not hold the killed request: %s", d)
		}
		var draft interruptedDraft
		if err := json.Unmarshal([]byte(d), &draft); err != nil || draft.SessionID == "" {
			f.Fatalf("the draft names no session: %v %s", err, d)
		}
		s.Type("继续")
		s.WaitFor("生成完成。", e2eTimeout)
		s.WaitIdle(e2eTimeout)
		if req := f.Requests(1)[0]; countMessages(req, "user", "marker-crash2") != 1 {
			f.Fatalf("the resumed request does not carry the killed task once: %q", requestTexts(req))
		}
		// "继续" goes back to the killed session, not the best-scored one,
		// and does not put the request in it a second time.
		if got := s.app.eng.SessionID(); got != draft.SessionID {
			f.Fatalf("\"继续\" resumed session %s, want the killed %s", got, draft.SessionID)
		}
		rec := capturedFromRecord(sessionFile(t, home, draft.SessionID))
		if countMessages(rec, "user", "marker-crash2") != 1 || countMessages(rec, "assistant", "生成完成。") != 1 {
			f.Fatalf("the killed session does not hold the resumed exchange once: %q", requestTexts(rec))
		}
		if _, err := os.Stat(draftPath); err == nil {
			f.Fatalf("the draft is still there after the resumed task completed")
		}
	})

	// ---------- all projects, clear ----------

	var otherID string
	t.Run("/history all跨项目列出并按all编号恢复", func(t *testing.T) {
		t.Chdir(other)
		otherID = saved(t, "另一个项目里的部署脚本 marker-other", "部署脚本放在 deploy 目录，说明完成。")
		t.Chdir(project)

		s, f := start(t, textReply("接着另一个项目的部署脚本继续，补充完成。"))
		mark := s.Mark()
		s.Type("/history")
		s.WaitForSince(mark, "个其他项目或旧版本的会话未显示，使用 /history all 查看全部", e2eTimeout)
		if strings.Contains(s.OutputSince(mark), "marker-other") {
			f.Fatalf("the project list shows another project's session")
		}
		mark = s.Mark()
		s.Type("/history all")
		s.WaitForSince(mark, "历史记录 (所有项目", e2eTimeout)
		s.WaitForSince(mark, "<other>", e2eTimeout)
		s.WaitForSince(mark, "marker-other", e2eTimeout)
		n := number(s, otherID, true)
		mark = s.Mark()
		s.Type(n)
		s.WaitForSince(mark, "已成功拉回历史会话 #"+n, e2eTimeout)
		s.WaitForSince(mark, "该会话属于其他项目目录", e2eTimeout)
		if got := s.app.eng.SessionID(); got != otherID {
			f.Fatalf("the number resumed %s, want the all list's #%s %s", got, n, otherID)
		}
		s.Type("部署脚本怎么回滚")
		s.WaitFor("补充完成。", e2eTimeout)
		s.WaitIdle(e2eTimeout)
		if req := f.Requests(1)[0]; countMessages(req, "user", "marker-other") != 1 {
			f.Fatalf("the resumed request lacks the other project's conversation: %q", requestTexts(req))
		}
		if rec := capturedFromRecord(sessionFile(t, home, otherID)); countMessages(rec, "user", "部署脚本怎么回滚") != 1 {
			f.Fatalf("the exchange was not saved into the other project's session")
		}
	})

	t.Run("/history clear需确认后清空本项目", func(t *testing.T) {
		s, f := start(t)
		records, _ := listHistoryRecords(s.app.eng.Store(), currentProjectDir(), false)
		if len(records) == 0 {
			t.Fatal("no sessions in the project to clear")
		}
		mark := s.Mark()
		s.Type("/history clear")
		s.WaitForSince(mark, "范围：当前项目", e2eTimeout)
		s.WaitForSince(mark, fmt.Sprintf("将删除 %d 个历史会话", len(records)), e2eTimeout)
		s.WaitForSince(mark, "[y] 确认", e2eTimeout)
		for _, r := range records {
			if _, err := os.Stat(sessionPath(home, r.ID)); err != nil {
				f.Fatalf("session %s deleted without confirmation: %v", r.ID, err)
			}
		}
		mark = s.Mark()
		s.Type("y")
		s.WaitForSince(mark, fmt.Sprintf("已删除 %d 个会话", len(records)), e2eTimeout)
		index := readFile(t, indexPath)
		for _, r := range records {
			if _, err := os.Stat(sessionPath(home, r.ID)); !errors.Is(err, os.ErrNotExist) {
				f.Fatalf("session %s survived the clear: %v", r.ID, err)
			}
			if strings.Contains(index, r.ID) {
				f.Fatalf("index.json still lists %s", r.ID)
			}
		}
		if _, err := os.Stat(sessionPath(home, otherID)); err != nil {
			f.Fatalf("the clear took another project's session: %v", err)
		}
		mark = s.Mark()
		s.Type("/history")
		s.WaitForSince(mark, "当前项目暂无历史", e2eTimeout)
		f.Requests(0)
	})
}
