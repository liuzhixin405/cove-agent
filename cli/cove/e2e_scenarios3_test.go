package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The write tool asks once; "a" covers every later write in the session,
// and the files land in the project directory.
func TestE2E_WriteToolRememberedForTheSession(t *testing.T) {
	if testing.Short() {
		t.Skip("slow (>3s): skipped under -short")
	}
	model := newFakeModel(t,
		fakeStep{ToolCalls: []fakeToolCall{{Name: "write", Args: `{"filePath":"notes/a.txt","content":"first\n"}`}}},
		fakeStep{ToolCalls: []fakeToolCall{{Name: "write", Args: `{"filePath":"notes/b.txt","content":"second\n"}`}}},
		fakeStep{Content: "两个文件都写好了，内容分别是 first 和 second，放在 notes 目录下。"},
	)
	_, project := e2eHome(t, model)
	s := startREPL(t)

	s.Type("写两个笔记文件")
	s.WaitFor("需要授权", e2eTimeout)
	s.WaitFor("write", e2eTimeout)
	s.Type("a")
	s.WaitFor("两个文件都写好了", e2eTimeout)
	if n := strings.Count(s.Output(), "需要授权"); n != 1 {
		t.Fatalf("write asked %d times, want once:\n%s", n, s.Output())
	}
	for _, f := range []string{"notes/a.txt", "notes/b.txt"} {
		if _, err := os.Stat(filepath.Join(project, f)); err != nil {
			t.Errorf("%s not written: %v", f, err)
		}
	}
}

// Two tool calls in one reply both run and both results reach the model.
func TestE2E_ParallelToolCallsBothRun(t *testing.T) {
	model := newFakeModel(t,
		fakeStep{ToolCalls: []fakeToolCall{
			{Name: "bash", Args: `{"command":"echo alpha"}`},
			{Name: "bash", Args: `{"command":"echo beta"}`},
		}},
		fakeStep{Content: "两条命令都跑完了，输出分别是 alpha 和 beta，结果和预期一致。"},
	)
	e2eHome(t, model)
	s := startREPL(t)

	s.Type("跑两条命令")
	s.WaitFor("两条命令都跑完了", e2eTimeout)
	reqs := model.Requests()
	var alpha, beta bool
	for _, m := range reqs[len(reqs)-1].Messages {
		if m.Role == "tool" {
			alpha = alpha || strings.Contains(m.Text(), "alpha")
			beta = beta || strings.Contains(m.Text(), "beta")
		}
	}
	if !alpha || !beta {
		t.Fatalf("tool results missing (alpha %v, beta %v):\n%+v", alpha, beta, reqs[len(reqs)-1].Messages)
	}
}

// auto mode runs build commands unasked and still asks for git pushes.
func TestE2E_AutoModeRunsBuildsButAsksForPush(t *testing.T) {
	model := newFakeModel(t,
		fakeStep{ToolCalls: []fakeToolCall{{Name: "bash", Args: `{"command":"go version"}`}}},
		fakeStep{ToolCalls: []fakeToolCall{{Name: "bash", Args: `{"command":"git push origin main"}`}}},
		fakeStep{Content: "版本确认完毕，推送需要你的授权，我已经按你的决定处理了后续步骤。"},
	)
	e2eHome(t, model)
	s := startREPL(t)

	s.Type("/mode auto")
	s.WaitFor("auto", e2eTimeout)
	s.Type("检查版本然后推送")
	s.WaitFor("需要授权", e2eTimeout)
	if !strings.Contains(s.Output(), "git push") {
		t.Fatalf("the prompt is not for the push:\n%s", s.Output())
	}
	if strings.Count(s.Output(), "需要授权") != 1 {
		t.Fatalf("the build command was asked about in auto mode:\n%s", s.Output())
	}
	s.Type("n")
	s.WaitFor("版本确认完毕", e2eTimeout)
}

// /model changes the model the very next request names.
func TestE2E_ModelSwitchIsUsedByTheNextRequest(t *testing.T) {
	model := newFakeModel(t, fakeStep{Content: "用新模型回答：一切正常，切换已经生效。"})
	e2eHome(t, model)
	s := startREPL(t)

	s.Type("/model qwen-large")
	s.WaitFor("qwen-large", e2eTimeout)
	s.Type("你现在是什么模型")
	s.WaitFor("切换已经生效", e2eTimeout)
	reqs := model.Requests()
	if got := reqs[len(reqs)-1].Model; got != "qwen-large" {
		t.Fatalf("request model = %q, want qwen-large after /model", got)
	}
}

// A stream that dies mid-answer is retried by the REPL once (the model
// resumes from where the turn was) and the person gets the whole answer,
// with a note about the retry; no half answer, no silent stop.
func TestE2E_BrokenStreamIsRetriedTransparently(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 {
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"回答的前半\"}}]}\n\n")
			return // connection closes mid-stream
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"这次是完整的回答，前面断掉的部分已经补齐。\"}}]}\n\n")
		fmt.Fprint(w, "data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":5}}\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	t.Cleanup(srv.Close)
	model := &fakeModel{t: t, srv: srv}
	e2eHome(t, model)
	s := startREPL(t)

	s.Type("回答一个问题")
	s.WaitFor("网络波动", 30*time.Second)
	s.WaitFor("这次是完整的回答", e2eTimeout)
	if calls != 2 {
		t.Fatalf("server called %d times, want the broken call and one retry", calls)
	}
}

// /history clear empties the current project's history: the confirmation
// box names the count, "n" keeps everything, "y" deletes, and /history
// afterwards lists nothing. (/history clean only repairs files, which is
// what the person mistook for clearing.)
func TestE2E_HistoryClearEmptiesTheList(t *testing.T) {
	model := newFakeModel(t, fakeStep{Content: "这是关于解析器结构的说明，先从词法分析开始讲。"})
	e2eHome(t, model)

	first := startREPL(t)
	first.Type("讲讲解析器的结构")
	first.WaitFor("解析器结构的说明", e2eTimeout)
	first.Exit()

	second := startREPL(t)
	second.Type("/history")
	second.WaitFor("讲讲解析器的结构", e2eTimeout)
	second.Type("/history clear")
	second.WaitFor("清除历史会话", e2eTimeout)
	second.WaitFor("将删除 1 个历史会话", e2eTimeout)
	if strings.Contains(second.Output(), "已删除") {
		t.Fatalf("history deleted without confirmation:\n%s", second.Output())
	}
	second.Type("n")
	second.WaitFor("已取消：清除历史会话", e2eTimeout)
	second.Type("/history clear")
	second.WaitFor("[y] 确认", e2eTimeout)
	second.Type("y")
	second.WaitFor("已删除 1 个会话", e2eTimeout)
	second.Type("/history")
	second.WaitFor("当前项目暂无历史", e2eTimeout)
}

// /history delete <N> removes just that session; the other one stays listed.
func TestE2E_HistoryDeleteRemovesOneSession(t *testing.T) {
	model := newFakeModel(t, fakeStep{Content: "第一个话题的回答，讲的是词法分析器的分层设计。"})
	e2eHome(t, model)

	first := startREPL(t)
	first.Type("讲讲词法分析器")
	first.WaitFor("第一个话题的回答", e2eTimeout)
	first.Exit()

	model.Append(fakeStep{Content: "第二个话题的回答，讲的是语法树的构造过程。"})
	second := startREPL(t)
	second.Type("讲讲语法树")
	second.WaitFor("第二个话题的回答", e2eTimeout)
	second.Exit()

	third := startREPL(t)
	third.Type("/history")
	third.WaitFor("讲讲词法分析器", e2eTimeout)
	third.WaitFor("讲讲语法树", e2eTimeout)
	// The newest session is number 1.
	third.Type("/history delete 1")
	third.WaitFor("已删除会话", e2eTimeout)
	third.Type("/history")
	third.WaitFor("历史记录 (当前项目, 1 个会话)", e2eTimeout)
	out := third.Output()
	tail := out[strings.LastIndex(out, "历史记录 (当前项目, 1 个会话)"):]
	if strings.Contains(tail, "讲讲语法树") || !strings.Contains(tail, "讲讲词法分析器") {
		t.Fatalf("wrong session deleted:\n%s", tail)
	}
}

// A request naming a directory outside the project is told, before the
// model runs, that the file tools cannot reach it and how to switch.
func TestE2E_OutsideDirectoryRequestGetsACdHintUpFront(t *testing.T) {
	model := newFakeModel(t, fakeStep{Content: "明白，这个目录在工作目录之外，请先切换目录后再让我开始。"})
	home, _ := e2eHome(t, model)
	other := filepath.Join(home, "agent")
	if err := os.MkdirAll(other, 0o755); err != nil {
		t.Fatal(err)
	}
	s := startREPL(t)
	// The path must not open the line: off Windows the temporary home is an
	// absolute path, and a line starting with "/" is read as a slash command
	// ("未知命令: /var/...") instead of a request naming a directory.
	s.Type("请在 " + other + " 这个目录写一个 netcore 的 agent 框架")
	s.WaitFor("/cd "+other, e2eTimeout)
	s.WaitFor("请先切换目录", e2eTimeout)
}
