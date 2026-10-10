package main

// Flow F6 (test design 5.3): the unattended front ends — cove -p (stdout for
// the answer, stderr for the rest, the exit code), its stdin sources, the
// headless line script and --max-turns. -p and the exit codes run cove as a
// child process (runCove); the line-script branches run the headless front
// end in process (headlessRunHere). One home and one fake model are shared.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestFlowF6_Headless(t *testing.T) {
	model := newFakeModel(t)
	home, project := e2eHome(t, model)
	sessionsDir := filepath.Join(home, ".cove", "sessions")

	reset := func(t *testing.T, steps ...fakeStep) flow {
		t.Helper()
		model.Reset(steps...)
		return flow{t: t, model: model}
	}
	// savedWith reports whether some saved session file contains sub.
	savedWith := func(sub string) bool {
		entries, _ := os.ReadDir(sessionsDir)
		for _, e := range entries {
			if !strings.HasSuffix(e.Name(), ".jsonl") {
				continue
			}
			if b, err := os.ReadFile(filepath.Join(sessionsDir, e.Name())); err == nil && strings.Contains(string(b), sub) {
				return true
			}
		}
		return false
	}
	userText := func(req capturedRequest) string {
		if m := lastMessage(req); m.Role == "user" {
			return m.Text()
		}
		return ""
	}

	// ---------- -p ----------

	t.Run("-p答案到stdout进度到stderr退出码0", func(t *testing.T) {
		const answer = "最终答复：目录里一切正常，echo 的输出也符合预期，没有需要处理的内容。"
		f := reset(t, toolReply(bashCall("echo f6-progress")), textReply(answer))
		r := runCove(t, nil, nil, "-p", "检查一下 marker-f6-print")
		if r.code != 0 {
			f.Fatalf("exit code %d, want 0; stderr:\n%s", r.code, r.stderr)
		}
		if got := strings.TrimSpace(r.stdout); got != answer {
			f.Fatalf("stdout = %q, want the answer alone; stderr:\n%s", got, r.stderr)
		}
		if !strings.Contains(r.stderr, "echo f6-progress") {
			f.Fatalf("stderr lacks the tool progress line:\n%s", r.stderr)
		}
		if got := lastMessage(f.Requests(2)[1]); got.Role != "tool" || !strings.Contains(got.Text(), "f6-progress") {
			f.Fatalf("second request's tool result = %s %q", got.Role, got.Text())
		}
		if !savedWith("marker-f6-print") {
			f.Fatalf("the -p session was not saved")
		}
	})

	t.Run("-p工具被拒时stderr说明且模型收到拒绝", func(t *testing.T) {
		const answer = "目录没有建成：这次运行不能授权 mkdir，需要先在交互模式里允许它再运行。"
		f := reset(t, toolReply(bashCall("mkdir f6_refused")), textReply(answer))
		r := runCove(t, nil, nil, "-p", "建一个目录 f6_refused")
		if !strings.Contains(r.stderr, "mkdir f6_refused") || !strings.Contains(r.stderr, "permission denied") {
			f.Fatalf("stderr does not name the refused call:\n%s", r.stderr)
		}
		if got := lastMessage(f.Requests(2)[1]); got.Role != "tool" || !strings.Contains(got.Text(), "permission denied for bash") {
			f.Fatalf("the model was not told the call was refused: %s %q", got.Role, got.Text())
		}
		if _, err := os.Stat(filepath.Join(project, "f6_refused")); !os.IsNotExist(err) {
			f.Fatalf("the refused mkdir ran (stat err %v)", err)
		}
		// The manual: exit 1 is for a failed run (API error, attachment,
		// iteration limit); a refused call the model answered is not one.
		if r.code != 0 || strings.TrimSpace(r.stdout) != answer {
			f.Fatalf("code %d stdout %q; stderr:\n%s", r.code, r.stdout, r.stderr)
		}
	})

	// ---------- -p stdin ----------

	t.Run("-p管道有数据时附在提示后", func(t *testing.T) {
		f := reset(t, textReply("日志的意思是服务启动成功。"))
		r := runCove(t, strings.NewReader("INFO f6-piped-line started\n"), nil, "-p", "解释这段日志")
		if r.code != 0 || strings.TrimSpace(r.stdout) != "日志的意思是服务启动成功。" {
			f.Fatalf("code %d stdout %q stderr:\n%s", r.code, r.stdout, r.stderr)
		}
		if strings.Contains(r.stderr, "未从 stdin 读到数据") {
			f.Fatalf("a pipe with data was reported empty:\n%s", r.stderr)
		}
		got := userText(f.Requests(1)[0])
		if !strings.Contains(got, "解释这段日志") || !strings.Contains(got, "f6-piped-line") {
			f.Fatalf("user message = %q, want the prompt and the piped text", got)
		}
		if !savedWith("f6-piped-line") {
			f.Fatalf("the piped text is not in the saved session")
		}
	})

	t.Run("-p管道无数据3秒后提示并只发提示", func(t *testing.T) {
		if testing.Short() {
			t.Skip("slow (>3s): skipped under -short")
		}
		pr, pw, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		// The writer stays open and silent, like a launcher's stdin.
		defer func() { _ = pw.Close(); _ = pr.Close() }()
		f := reset(t, textReply("没有管道输入，只按提示回答。"))
		began := time.Now()
		r := runCove(t, pr, nil, "-p", "只有提示 marker-f6-nodata")
		if el := time.Since(began); el < stdinFirstDataTimeout {
			f.Fatalf("returned after %v, before the %v wait", el, stdinFirstDataTimeout)
		}
		if !strings.Contains(r.stderr, "秒内未从 stdin 读到数据，已忽略管道输入") {
			f.Fatalf("stderr lacks the no-data notice:\n%s", r.stderr)
		}
		if r.code != 0 || strings.TrimSpace(r.stdout) != "没有管道输入，只按提示回答。" {
			f.Fatalf("code %d stdout %q", r.code, r.stdout)
		}
		if got := strings.TrimSpace(userText(f.Requests(1)[0])); got != "只有提示 marker-f6-nodata" {
			f.Fatalf("user message = %q, want the prompt alone", got)
		}
		if !savedWith("marker-f6-nodata") {
			f.Fatalf("the run's session was not saved")
		}
	})

	t.Run("-p从文件读stdin不设超时", func(t *testing.T) {
		path := filepath.Join(home, "f6-stdin.txt")
		writeTestFile(t, path, "文件内容 f6-file-line\n")
		in, err := os.Open(path)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = in.Close() }()
		f := reset(t, textReply("文件里写的是一行说明。"))
		began := time.Now()
		r := runCove(t, in, nil, "-p", "这个文件说了什么")
		if r.code != 0 || strings.Contains(r.stderr, "未从 stdin 读到数据") || strings.Contains(r.stderr, "正在等待管道输入") {
			f.Fatalf("code %d stderr:\n%s", r.code, r.stderr)
		}
		if el := time.Since(began); el >= stdinFirstDataTimeout+2*time.Second {
			f.Fatalf("reading a file took %v", el)
		}
		if got := userText(f.Requests(1)[0]); !strings.Contains(got, "f6-file-line") {
			f.Fatalf("user message = %q, want the file's content", got)
		}
		if !savedWith("f6-file-line") {
			f.Fatalf("the file's content is not in the saved session")
		}
	})

	// ---------- headless line script ----------

	var oldSession string
	t.Run("headless的/history选号在下一行有效", func(t *testing.T) {
		f := reset(t, textReply("旧会话讲的是解析器，回答完毕。"))
		if failed, _, stderr := headlessRunHere(t, "旧会话 marker-f6-old\n"); failed {
			f.Fatalf("seeding the old session failed: %s", stderr)
		}
		f = reset(t, textReply("接着旧会话的话题回答，回答完毕。"))
		failed, stdout, stderr := headlessRunHere(t, "/history\n1\n接着问\n")
		if failed {
			f.Fatalf("run failed: %s", stderr)
		}
		if !strings.Contains(stdout, "已成功拉回历史会话 #1") {
			f.Fatalf("the number did not resume a session; stdout:\n%s", stdout)
		}
		req := f.Requests(1)[0]
		if countMessages(req, "user", "marker-f6-old") != 1 || userText(req) != "接着问" {
			f.Fatalf("the request after the pick lacks the old conversation: %q", requestTexts(req))
		}
		if !savedWith("接着问") {
			f.Fatalf("the resumed session was not saved")
		}
		oldSession = "marker-f6-old"
	})

	t.Run("headless的/history后隔一条命令选号失效", func(t *testing.T) {
		if oldSession == "" {
			t.Skip("needs the previous subtest's session")
		}
		f := reset(t, textReply("收到数字 1，这是一条普通消息。"))
		failed, stdout, stderr := headlessRunHere(t, "/history\n/tasks\n1\n")
		if failed {
			f.Fatalf("run failed: %s", stderr)
		}
		if strings.Contains(stdout, "已成功拉回历史会话") {
			f.Fatalf("a number after another command still picked a session:\n%s", stdout)
		}
		req := f.Requests(1)[0]
		if userText(req) != "1" || countMessages(req, "", "marker-f6-old") != 0 {
			f.Fatalf("\"1\" was not sent as a message of a fresh session: %q", requestTexts(req))
		}
		if !strings.Contains(stdout, "收到数字 1") {
			f.Fatalf("stdout lacks the answer:\n%s", stdout)
		}
	})

	t.Run("headless的/history后隔一条消息选号失效", func(t *testing.T) {
		if oldSession == "" {
			t.Skip("needs the previous subtest's session")
		}
		f := reset(t, textReply("这是对新问题的回答，完毕。"), textReply("收到数字 1，这是一条普通消息。"))
		failed, stdout, stderr := headlessRunHere(t, "/history\n新问题 marker-f6-between\n1\n")
		if failed {
			f.Fatalf("run failed: %s", stderr)
		}
		if strings.Contains(stdout, "已成功拉回历史会话") {
			f.Fatalf("a number two lines after /history still picked a session:\n%s", stdout)
		}
		reqs := f.Requests(2)
		if userText(reqs[1]) != "1" || countMessages(reqs[1], "user", "marker-f6-between") != 1 {
			f.Fatalf("\"1\" was not sent as the next message of the same session: %q", requestTexts(reqs[1]))
		}
		if !savedWith("marker-f6-between") {
			f.Fatalf("the session was not saved")
		}
	})

	t.Run("headless遇到exit结束", func(t *testing.T) {
		f := reset(t, textReply("第一问的回答，完毕。"), textReply("不该出现的回答"))
		failed, stdout, stderr := headlessRunHere(t, "第一问 marker-f6-exit\nexit\n第二问不应执行 marker-f6-after-exit\n")
		if failed {
			f.Fatalf("run failed: %s", stderr)
		}
		if !strings.Contains(stdout, "第一问的回答") || strings.Contains(stdout, "不该出现的回答") {
			f.Fatalf("stdout = %q", stdout)
		}
		f.Requests(1)
		if !savedWith("marker-f6-exit") || savedWith("marker-f6-after-exit") {
			f.Fatalf("saved sessions do not match the lines before exit")
		}
	})

	t.Run("headless一行失败不影响后续行且退出码非零", func(t *testing.T) {
		f := reset(t,
			fakeStep{Status: 401, Body: `{"error":{"message":"invalid api key","type":"authentication_error"}}`},
			textReply("第二行的回答，完毕。"),
			textReply("第三行的回答，完毕。"))
		r := runCove(t, strings.NewReader("第一行 marker-f6-fail\n第二行\n第三行\n"), nil, "--no-tui")
		if r.code == 0 {
			f.Fatalf("exit code 0 after a failed line; stderr:\n%s", r.stderr)
		}
		if !strings.Contains(r.stdout, "第二行的回答") || !strings.Contains(r.stdout, "第三行的回答") {
			f.Fatalf("the later lines did not run; stdout:\n%s\nstderr:\n%s", r.stdout, r.stderr)
		}
		if !strings.Contains(r.stderr, "Error:") {
			f.Fatalf("stderr does not report the failed line:\n%s", r.stderr)
		}
		reqs := f.Requests(3)
		if userText(reqs[2]) != "第三行" {
			f.Fatalf("third request = %q", requestTexts(reqs[2]))
		}
		if !savedWith("第三行") {
			f.Fatalf("the session was not saved")
		}
	})

	t.Run("headless提交命令失败停止后续输入", func(t *testing.T) {
		f := reset(t, textReply("不应运行后续推送"))
		r := runCove(t, strings.NewReader("/commit --unknown\n推送代码\n"), nil, "--no-tui")
		if r.code != 1 || !strings.Contains(r.stderr, "不再执行后续输入") || strings.Contains(r.stdout, "不应运行后续推送") {
			f.Fatalf("code %d stdout %q stderr %q", r.code, r.stdout, r.stderr)
		}
		f.Requests(0)
	})

	// ---------- --max-turns ----------

	t.Run("--max-turns到达时已有答复则保留答复", func(t *testing.T) {
		const answer = "最终答复：echo 已经执行，输出正确，任务在第二次调用时完成，没有遗留事项。"
		f := reset(t, toolReply(bashCall("echo f6-turns")), textReply(answer))
		r := runCove(t, nil, nil, "-p", "跑一下 echo marker-f6-turns", "--max-turns", "2")
		if r.code != 0 || strings.TrimSpace(r.stdout) != answer {
			f.Fatalf("code %d stdout %q; stderr:\n%s", r.code, r.stdout, r.stderr)
		}
		if strings.Contains(r.stderr, "已达到单轮最大迭代次数") {
			f.Fatalf("the answer at the cap was reported as a limit error:\n%s", r.stderr)
		}
		f.Requests(2)
		if !savedWith(answer) {
			f.Fatalf("the answer is not in the saved session")
		}
	})

	t.Run("--max-turns到达时简短答复也保留", func(t *testing.T) {
		// A short final text after tool calls normally earns one "too
		// brief" reminder, a third model call the cap does not allow.
		f := reset(t, toolReply(bashCall("echo f6-brief")), textReply("好了"), textReply("不该有第三次调用"))
		r := runCove(t, nil, nil, "-p", "跑一下 echo marker-f6-brief", "--max-turns", "2")
		if r.code != 0 || strings.TrimSpace(r.stdout) != "好了" {
			f.Fatalf("code %d stdout %q; stderr:\n%s", r.code, r.stdout, r.stderr)
		}
		if strings.Contains(r.stderr, "已达到单轮最大迭代次数") {
			f.Fatalf("the answer at the cap was replaced by a limit error:\n%s", r.stderr)
		}
		f.Requests(2)
		if !savedWith("marker-f6-brief") {
			f.Fatalf("the session was not saved")
		}
	})

	t.Run("--max-turns到达时没有答复则报错退出1", func(t *testing.T) {
		f := reset(t, toolReply(bashCall("echo f6-cap")), textReply("收尾总结：echo 已执行，还没来得及给出结论。"))
		r := runCove(t, nil, nil, "-p", "跑一下 echo marker-f6-cap", "--max-turns", "1")
		if r.code != 1 || !strings.Contains(r.stderr, "已达到单轮最大迭代次数 1（可用 --max-turns 调整）") {
			f.Fatalf("code %d stderr:\n%s", r.code, r.stderr)
		}
		if !strings.Contains(r.stdout, "收尾总结") {
			f.Fatalf("stdout lacks the wrap-up summary: %q", r.stdout)
		}
		reqs := f.Requests(2)
		if len(reqs[1].Messages) == 0 {
			f.Fatalf("empty wrap-up request")
		}
		if !savedWith("marker-f6-cap") {
			f.Fatalf("the session was not saved")
		}
	})
}
