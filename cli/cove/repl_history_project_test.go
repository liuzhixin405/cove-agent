package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/liuzhixin405/cove-agent/internal/api"
	"github.com/liuzhixin405/cove-agent/internal/engine"
	"github.com/liuzhixin405/cove-agent/internal/session"
	"github.com/liuzhixin405/cove-agent/internal/termui"
)

const (
	titleA   = "修复 A 项目的登录流程"
	titleB   = "重构 B 项目的配置加载"
	titleOld = "升级旧项目的依赖版本"

	// mismatchWarning is the phrase of session.ProjectMismatchWarning; the
	// hidden-sessions hint also says "其他项目", so match the warning itself.
	mismatchWarning = "该会话属于其他项目目录"
)

type projectHistory struct {
	eng        *engine.Engine
	dirA, dirB string
	out        *bytes.Buffer
}

// setupProjectHistory saves one session in project A, a richer one in project
// B (it would win "most relevant" on score alone) and a legacy session with no
// directory, then starts an engine in project A with output captured.
func setupProjectHistory(t *testing.T) *projectHistory {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("COVE_CONFIG_DIR", filepath.Join(home, ".cove"))
	seedCheckpointStore(t, home)
	root := t.TempDir()
	dirA := filepath.Join(root, "project-a")
	dirB := filepath.Join(root, "project-b")
	for _, d := range []string{dirA, dirB} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}

	store, err := session.NewStore()
	if err != nil {
		t.Fatal(err)
	}
	save := func(id, title, cwd string, msgs []api.Message) {
		t.Helper()
		if err := store.Save(&session.Record{ID: id, CreatedAt: time.Now(), Title: title, Model: "claude-opus-5", Cwd: cwd, Messages: msgs}); err != nil {
			t.Fatal(err)
		}
	}
	save("sess-a", titleA, dirA, []api.Message{
		{Role: "user", Content: "请" + titleA},
		{Role: "assistant", Content: "好的"},
	})
	save("sess-b", titleB, dirB, []api.Message{
		{Role: "user", Content: "请" + titleB + ",参考 https://example.com/config 的写法"},
		{Role: "assistant", Content: "先读取配置", ToolCalls: []api.ToolCall{{ID: "c1", Name: "read_file"}}},
		{Role: "tool", ToolCallID: "c1", Name: "read_file", Content: "package config"},
		{Role: "assistant", Content: "已完成重构"},
		{Role: "user", Content: "再补充测试"},
		{Role: "assistant", Content: "测试已补充"},
	})
	save("sess-old", titleOld, "", []api.Message{
		{Role: "user", Content: "请" + titleOld},
		{Role: "assistant", Content: "好的"},
	})

	t.Chdir(dirA)
	eng, err := engine.New(engine.Config{Model: "test-model"})
	if err != nil {
		t.Fatalf("engine.New: %v", err)
	}

	var out bytes.Buffer
	termui.SetWriter(&out)
	oldPickAll := historyPickAll
	t.Cleanup(func() {
		termui.SetWriter(nil)
		historyPickAll = oldPickAll
	})
	return &projectHistory{eng: eng, dirA: dirA, dirB: dirB, out: &out}
}

func firstUserContent(msgs []api.Message) string {
	for _, m := range msgs {
		if m.Role == "user" {
			return m.Content
		}
	}
	return ""
}

func assertListed(t *testing.T, out string, want, notWant []string) {
	t.Helper()
	for _, s := range want {
		if !strings.Contains(out, s) {
			t.Errorf("output is missing %q:\n%s", s, out)
		}
	}
	for _, s := range notWant {
		if strings.Contains(out, s) {
			t.Errorf("output lists %q, which is not from this project:\n%s", s, out)
		}
	}
}

func TestHistoryListsOnlyCurrentProject(t *testing.T) {
	h := setupProjectHistory(t)

	handleHistory(h.eng, false)

	assertListed(t, h.out.String(), []string{titleA, "/history all"}, []string{titleB, titleOld})
}

func TestHistoryPickerUsesProjectScopeAndStableSessionID(t *testing.T) {
	h := setupProjectHistory(t)
	choices := historyPickerChoices(h.eng, false)
	if len(choices) != 1 || choices[0].Value != "/resume sess-a" || choices[0].Label != titleA {
		t.Fatalf("project choices = %+v", choices)
	}
	if !strings.Contains(choices[0].Preview, h.dirA) || !strings.Contains(choices[0].Description, "2 条") {
		t.Fatalf("session preview is incomplete: %+v", choices[0])
	}
	if all := historyPickerChoices(h.eng, true); len(all) != 3 {
		t.Fatalf("all-project choices = %+v", all)
	}
}

func TestHistoryAllListsEveryProject(t *testing.T) {
	h := setupProjectHistory(t)

	handleHistory(h.eng, true)

	assertListed(t, h.out.String(), []string{titleA, titleB, titleOld}, nil)
}

// After /history all, a bare number must pick from the list the user just
// saw, not from the shorter per-project list — or it resumes the wrong one.
func TestHistoryNumberAfterAllViewPicksFromAllListAndWarns(t *testing.T) {
	h := setupProjectHistory(t)
	store := h.eng.Store()
	all, _ := listHistoryRecords(store, h.dirA, true)
	idx := -1
	for i, r := range all {
		if r.ID == "sess-b" {
			idx = i + 1
		}
	}
	if idx < 0 {
		t.Fatalf("sess-b missing from the all view: %+v", all)
	}

	handleHistory(h.eng, true)
	h.out.Reset()
	handleHistoryResume(strconv.Itoa(idx), h.eng)

	if got := firstUserContent(h.eng.Messages()); !strings.Contains(got, titleB) {
		t.Fatalf("picked #%d from the all view but resumed %q", idx, got)
	}
	if out := h.out.String(); !strings.Contains(out, mismatchWarning) || !strings.Contains(out, h.dirB) {
		t.Errorf("resuming project B's session from project A should warn:\n%s", out)
	}
}

func TestHistoryNumberAfterProjectViewPicksFromProjectList(t *testing.T) {
	h := setupProjectHistory(t)

	handleHistory(h.eng, false)
	handleHistoryResume("1", h.eng)

	if got := firstUserContent(h.eng.Messages()); !strings.Contains(got, titleA) {
		t.Fatalf("/history then 1 resumed %q, want project A's session", got)
	}
	if strings.Contains(h.out.String(), mismatchWarning) {
		t.Errorf("same-project resume warned:\n%s", h.out.String())
	}
}

func TestResumeMostRelevantIgnoresOtherProjects(t *testing.T) {
	h := setupProjectHistory(t)

	if !handleHistoryResumeMostRelevant(h.eng) {
		t.Fatal("no session resumed")
	}

	if got := firstUserContent(h.eng.Messages()); !strings.Contains(got, titleA) {
		t.Fatalf("resumed %q, want project A's session even though B scores higher", got)
	}
}

func TestResumeListsOnlyCurrentProject(t *testing.T) {
	h := setupProjectHistory(t)

	handleResume(context.Background(), "", h.eng)

	assertListed(t, h.out.String(), []string{"sess-a", "/resume all"}, []string{"sess-b", "sess-old"})
}

func TestResumeAllListsEveryProject(t *testing.T) {
	h := setupProjectHistory(t)

	handleResume(context.Background(), "all", h.eng)

	assertListed(t, h.out.String(), []string{"sess-a", "sess-b", "sess-old"}, nil)
}

func TestResumeByIDFromOtherProjectLoadsAndWarns(t *testing.T) {
	h := setupProjectHistory(t)

	handleResume(context.Background(), "sess-b", h.eng)

	if got := firstUserContent(h.eng.Messages()); !strings.Contains(got, titleB) {
		t.Fatalf("explicit ID from another project was not resumed, got %q", got)
	}
	if out := h.out.String(); !strings.Contains(out, mismatchWarning) || !strings.Contains(out, h.dirB) {
		t.Errorf("resuming by ID from another project should warn:\n%s", out)
	}
}

func TestListSessionsFlagDefaultsToCurrentProject(t *testing.T) {
	h := setupProjectHistory(t)

	listSessions(false)
	assertListed(t, h.out.String(), []string{"sess-a", "--list-sessions all"}, []string{"sess-b", "sess-old"})

	h.out.Reset()
	listSessions(true)
	assertListed(t, h.out.String(), []string{"sess-a", "sess-b", "sess-old"}, nil)
}
