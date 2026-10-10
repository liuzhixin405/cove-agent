package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/liuzhixin405/cove-agent/internal/api"
	"github.com/liuzhixin405/cove-agent/internal/engine"
	"github.com/liuzhixin405/cove-agent/internal/permission"
	"github.com/liuzhixin405/cove-agent/internal/render"
	"github.com/liuzhixin405/cove-agent/internal/repl"
	"github.com/liuzhixin405/cove-agent/internal/tool"
)

func TestFilePreviewTaskReviewEndToEnd(t *testing.T) {
	fe, _ := headlessFrontend(t)
	eng, err := engine.New(engine.Config{
		Model:    "test-model",
		Provider: api.ProviderConfig{Name: "openai-compatible", APIKey: "placeholder", BaseURL: "http://127.0.0.1:1"},
		Tools:    []tool.Tool{tool.NewWriteTool(), tool.NewExitPlanModeTool()},
	})
	if err != nil {
		t.Fatal(err)
	}
	fe.eng = eng
	t.Setenv("COVE_FILE_PREVIEW", "0")
	t.Setenv("COVE_REPLAY_EXPLAIN", "0")
	output := captureTurnOutput(t)
	target, err := filepath.Abs("main.go")
	if err != nil {
		t.Fatal(err)
	}
	writeArgs := func(content string) string {
		data, err := json.Marshal(map[string]any{"filePath": target, "content": content})
		if err != nil {
			t.Fatal(err)
		}
		return string(data)
	}
	model := newFakeModel(t,
		fakeStep{ToolCalls: []fakeToolCall{{Name: "exit_plan_mode", Args: `{"summary":"第一项方案","files":["main.go"],"decisions":["保留接口"],"checks":["检查内容"]}`}}},
		fakeStep{ToolCalls: []fakeToolCall{{Name: "write", Args: writeArgs("first")}}},
		fakeStep{Content: "第一项完成"},
	)
	if err := fe.eng.ReloadProvider("openai-compatible", "test-model", model.BaseURL(), "placeholder"); err != nil {
		t.Fatal(err)
	}
	fe.eng.SetPermissionMode(permission.Bypass)
	if err := fe.eng.SetWorkflowMode("once"); err != nil {
		t.Fatal(err)
	}
	asked := 0
	fe.eng.PermissionPrompt = func(string, map[string]any, string) bool { asked++; return true }
	dir, err := filePreviewDir(fe.eng.SessionID())
	if err != nil {
		t.Fatal(err)
	}
	var firstTaskID string
	for index, request := range []string{"执行第一项", "执行第二项"} {
		if index == 1 {
			model.Append(fakeStep{ToolCalls: []fakeToolCall{{Name: "write", Args: writeArgs("second")}}}, fakeStep{Content: "第二项完成"})
		}
		if _, err := runChatInteraction(context.Background(), fe.eng, request); err != nil {
			t.Fatal(err)
		}
		state := fe.eng.WorkflowStatus()
		review, err := loadFilePreviewTaskReview(dir, state.TaskID)
		if err != nil || review.Acceptance == nil || review.Acceptance.Request != request || review.Task.Title != request || review.Acceptance.SessionID != fe.eng.SessionID() {
			t.Fatalf("task receipt missing or misassociated: index=%d err=%v", index, err)
		}
		if index == 0 {
			firstTaskID = state.TaskID
			if review.Plan == nil || review.Plan.Summary != "第一项方案" || review.Plan.Decision != "approved" {
				t.Fatalf("approved plan was not captured: pending=%v asked=%d output=%s", state.Pending, asked, output.String())
			}
		} else if review.Plan != nil || state.TaskID == firstTaskID {
			t.Fatal("second task borrowed the first plan or identity")
		}
	}
	ids, err := listFilePreviews(dir)
	if err != nil || len(ids) != 2 {
		t.Fatalf("expected two actual diffs: count=%d err=%v", len(ids), err)
	}
	groups := filePreviewGroups(dir, ids)
	if len(groups) != 2 || asked != 1 {
		t.Fatalf("wrong grouping or approval count: groups=%d asked=%d", len(groups), asked)
	}
	firstRecord, err := loadFilePreview(dir, ids[0])
	if err != nil || firstRecord.Task.ID != firstTaskID {
		t.Fatal("diff did not share the task receipt identity")
	}
	firstReview, err := loadFilePreviewTaskReview(dir, firstTaskID)
	if err != nil || firstReview.Acceptance.Request != "执行第一项" || firstReview.Plan.Summary != "第一项方案" {
		t.Fatal("later task replaced the earlier receipt")
	}
	data, err := os.ReadFile(target)
	if err != nil || string(data) != "second" {
		t.Fatal("scripted actual changes did not execute")
	}
}

func TestFilePreviewOverviewTaskEvidence(t *testing.T) {
	dir := t.TempDir()
	first := filePreviewTask{ID: "20261010T120000.000000001", Title: "实现第一项"}
	second := filePreviewTask{ID: "20261010T120000.000000002", Title: "实现第二项"}
	var ids []string
	for _, task := range []filePreviewTask{first, second} {
		id, err := saveFilePreview(dir, render.Block{Tool: "edit", Header: "main.go", Diff: true, Full: "--- main.go\n+++ main.go\n-old\n+new"}, task)
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	if err := saveFilePreviewTaskReview(dir, filePreviewTaskReview{Task: first, Plan: &engine.ImplementationPlan{Summary: "保留接口", Decision: "approved", Decisions: []string{"不添加依赖"}, Checks: []string{"未执行的计划标准"}}, Acceptance: &engine.AcceptanceReport{Outcome: "failed", Checks: []engine.AcceptanceCheck{{Criterion: "FIRST_TASK_ONLY", Status: "failed", Reason: "失败证据"}}}}); err != nil {
		t.Fatal(err)
	}
	groups := filePreviewGroups(dir, ids)
	firstOverview := filePreviewOverview(dir, groups, []string{ids[0]})
	for _, want := range []string{"实现第一项", "main.go", "累计 +1 −1", "保留接口", "FIRST_TASK_ONLY", "[失败]", "计划验收标准（不等于已通过）"} {
		if !strings.Contains(firstOverview, want) {
			t.Fatalf("missing %q in overview: %s", want, firstOverview)
		}
	}
	secondOverview := filePreviewOverview(dir, groups, []string{ids[1]})
	if strings.Contains(secondOverview, "FIRST_TASK_ONLY") || strings.Contains(secondOverview, "保留接口") || !strings.Contains(secondOverview, "未记录任务级证据") {
		t.Fatalf("evidence leaked across tasks: %s", secondOverview)
	}
	if _, err := loadFilePreviewTaskReview(dir, "../outside"); err == nil {
		t.Fatal("task receipt traversal accepted")
	}
	fe, notices := headlessFrontend(t)
	previewDir, err := filePreviewDir(fe.eng.SessionID())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := saveFilePreview(previewDir, render.Block{Tool: "edit", Header: "main.go", Diff: true, Full: "-old\n+new"}, second); err != nil {
		t.Fatal(err)
	}
	fe.dispatch("/replay overview 1")
	output := strings.Join(*notices, "\n")
	if !strings.Contains(output, "实现第二项") || !strings.Contains(output, "未记录任务级证据") {
		t.Fatalf("overview command did not show the selected task: %s", output)
	}
}

func TestCompleteAtPath(t *testing.T) {
	dir := t.TempDir()
	for _, p := range []string{"internal/engine/turn.go", "internal/repl/x.go", "README.md", ".hidden"} {
		full := filepath.Join(dir, filepath.FromSlash(p))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	wd, _ := os.Getwd()
	t.Cleanup(func() { _ = os.Chdir(wd) })
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	got, ok := completeAtPath("看看 @int")
	if !ok || strings.Join(got, "|") != "看看 @internal/" {
		t.Fatalf("got %q, %v", got, ok)
	}
	got, _ = completeAtPath("看看 @internal/")
	if strings.Join(got, "|") != "看看 @internal/engine/|看看 @internal/repl/" {
		t.Fatalf("dir listing = %q", got)
	}
	if got, _ := completeAtPath("@"); strings.Contains(strings.Join(got, "|"), ".hidden") {
		t.Fatalf("dotfile offered: %q", got)
	}
	if _, ok := completeAtPath("/help"); ok {
		t.Fatal("a command line was taken as a path")
	}
}

func TestExpandCommand(t *testing.T) {
	sessionBlocks = &blockStore{}
	if got := expandCommand(nil); !strings.Contains(got, "还没有") {
		t.Fatalf("empty store: %q", got)
	}
	sessionBlocks.add(render.ToolBlock("41", "bash", "ls", "", "a\nb\nc", false, 0))
	sessionBlocks.add(render.ToolBlock("42", "grep", "x", "", "hit1\nhit2", false, 0))
	if got := expandCommand(nil); !strings.Contains(got, "hit2") {
		t.Fatalf("/x should expand the latest block: %q", got)
	}
	if got := expandCommand([]string{"#41"}); !strings.Contains(got, "c") || strings.Contains(got, "hit") {
		t.Fatalf("/x 41 = %q", got)
	}
	if got := expandCommand([]string{"99"}); !strings.Contains(got, "找不到 #99") {
		t.Fatalf("/x 99 = %q", got)
	}
}

func TestFilePreviewPlayback(t *testing.T) {
	diff := "@@ -1 +1 @@\n-old\n+中文🙂\n"
	var chunks []string
	wait := func(context.Context, time.Duration) bool { return true }
	if !playFilePreview(context.Background(), diff, time.Second, func(chunk string) {
		chunks = append(chunks, chunk)
	}, wait) {
		t.Fatal("playback cancelled")
	}
	if strings.Join(chunks, "") != diff {
		t.Fatalf("playback changed diff: %q", chunks)
	}
	for _, chunk := range chunks {
		if len([]rune(chunk)) != 1 {
			t.Fatalf("short preview did not type one character: %q", chunk)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	printed := ""
	if playFilePreview(ctx, diff, time.Second, func(chunk string) {
		printed += chunk
		cancel()
	}, wait) || printed != "@" {
		t.Fatalf("cancelled playback continued: %q", printed)
	}
	long := strings.Repeat("+code\n", 1000)
	frames := 0
	printed = ""
	playFilePreview(context.Background(), long, 80*time.Millisecond, func(chunk string) {
		frames++
		printed += chunk
	}, wait)
	if frames > 10 || printed != long {
		t.Fatalf("long playback: frames=%d complete=%v", frames, printed == long)
	}
	printed = ""
	playFilePreview(context.Background(), "\x1b[2J+safe\n", time.Second, func(chunk string) {
		printed += chunk
	}, wait)
	if printed != "+safe\n" {
		t.Fatalf("terminal control leaked: %q", printed)
	}
}

func TestFilePreviewPersistence(t *testing.T) {
	dir := t.TempDir()
	block := render.Block{ID: "41", Kind: render.KindTool, Tool: "edit", Header: "a.go", Full: "-old\n+中文\n", Diff: true}
	id, err := saveFilePreview(dir, block)
	if err != nil || id == "" {
		t.Fatalf("save: id=%q err=%v", id, err)
	}
	for repeat := 0; repeat < 310; repeat++ {
		record, err := loadFilePreview(dir, id)
		if err != nil || record.Block.Full != block.Full || record.Block.Header != "a.go" {
			t.Fatalf("replay %d: err=%v", repeat, err)
		}
	}
	ids, err := listFilePreviews(dir)
	if err != nil || len(ids) != 1 || ids[0] != id {
		t.Fatalf("list: %v err=%v", ids, err)
	}
	blocks := &blockStore{}
	blocks.add(block)
	for index := 0; index < blockStoreMax+1; index++ {
		next := block
		next.ID = "later-" + time.Duration(index).String()
		blocks.add(next)
		if _, err := saveFilePreview(dir, next); err != nil {
			t.Fatal(err)
		}
	}
	if _, ok := blocks.get(block.ID); ok {
		t.Fatal("test did not evict the old tool block")
	}
	if record, err := loadFilePreview(dir, id); err != nil || record.Block.Full != block.Full {
		t.Fatal("old preview was lost when tool blocks were evicted")
	}
	if _, err := loadFilePreview(dir, "../escape"); err == nil {
		t.Fatal("accepted path traversal")
	}
	block.IsError = true
	if id, err := saveFilePreview(dir, block); err != nil || id != "" {
		t.Fatalf("saved failed edit: %q %v", id, err)
	}
	block.IsError, block.Diff = false, false
	if id, err := saveFilePreview(dir, block); err != nil || id != "" {
		t.Fatalf("saved non-diff: %q %v", id, err)
	}
	long := strings.Repeat("+"+strings.Repeat("中", 200)+"\n", 20)
	excerpt := filePreviewExcerpt(long)
	if strings.Count(excerpt, "\n") != toolPreviewLines || strings.Contains(excerpt, strings.Repeat("中", 200)) {
		t.Fatal("automatic preview is not bounded")
	}
}

func TestFilePreviewAutomaticAndReplayCommand(t *testing.T) {
	fe, notices := headlessFrontend(t)
	t.Setenv("COVE_CONFIG_DIR", t.TempDir())
	t.Setenv("COVE_FILE_PREVIEW", "1")
	output := captureTurnOutput(t)
	printer := newTurnPrinter()
	block := render.Block{ID: "77", Kind: render.KindTool, Tool: "write", Header: "new.go", Full: "+中文\n", Diff: true}
	printer.fileChange(context.Background(), fe.eng.SessionID(), block, true)
	printer.stop()
	if !strings.Contains(output.String(), "+中文") || !strings.Contains(output.String(), "回放: /replay ") {
		t.Fatalf("automatic preview missing: %q", output.String())
	}
	dir, err := filePreviewDir(fe.eng.SessionID())
	if err != nil {
		t.Fatal(err)
	}
	ids, err := listFilePreviews(dir)
	if err != nil || len(ids) != 1 {
		t.Fatalf("automatic preview not saved: %v %v", ids, err)
	}
	if commandMutatesEngine("/replay") || !fe.dispatch("/replay list") || !strings.Contains(strings.Join(*notices, "\n"), "new.go") {
		t.Fatal("replay list is not a registered read-only command")
	}
	if !strings.Contains(strings.Join(*notices, "\n"), "1.1  new.go") {
		t.Fatal("replay list does not show selectable ordinals")
	}
	for _, command := range []string{"/replay", "/replay 1", "/replay " + ids[0], "/replay 1"} {
		*notices = nil
		if !fe.dispatch(command) || !strings.Contains(strings.Join(*notices, "\n"), block.Full) {
			t.Fatalf("headless replay lost saved diff: %s", command)
		}
	}
	otherDir, err := filePreviewDir(fe.eng.SessionID() + "-other")
	if err != nil || otherDir == dir {
		t.Fatal("previews are not isolated by session")
	}
	t.Setenv("COVE_FILE_PREVIEW", "0")
	before := output.String()
	printer.fileChange(context.Background(), fe.eng.SessionID(), block, true)
	if output.String() != before {
		t.Fatal("disabled automatic preview produced output")
	}
	ids, err = listFilePreviews(dir)
	if err != nil || len(ids) != 2 {
		t.Fatal("disabled animation stopped saving previews")
	}
	second := block
	second.Header, second.Full = "second.go", "+second\n"
	if _, err := saveFilePreview(dir, second); err != nil {
		t.Fatal(err)
	}
	*notices = nil
	fe.dispatch("/replay 3")
	if got := strings.Join(*notices, "\n"); got != "second.go\n+second\n" {
		t.Fatalf("ordinal selected wrong record: %q", got)
	}
	for _, input := range []string{"/replay 0", "/replay -1", "/replay 4", "/replay 999999999999999999999999"} {
		*notices = nil
		fe.dispatch(input)
		if !strings.Contains(strings.Join(*notices, "\n"), "请选择 1-3") {
			t.Fatalf("missing ordinal range hint: %s", input)
		}
	}
}

func TestQuestionOptions(t *testing.T) {
	prompt := "[Q1] 选择\n怎么处理？\n  1. 保留: 不改\n  2. 删除: 去掉\n"
	opts, keys := questionOptions(prompt)
	if len(opts) != 2 || opts[1] != "2. 删除: 去掉" || keys != "12" {
		t.Fatalf("options %q keys %q", opts, keys)
	}
}

func TestFilePreviewTaskGroups(t *testing.T) {
	dir := t.TempDir()
	block := render.Block{Kind: render.KindTool, Tool: "edit", Header: "main.go", Full: "+one\n", Diff: true}
	first := filePreviewTask{ID: "task-a", Title: "实现棋盘"}
	second := filePreviewTask{ID: "task-b", Title: "实现棋盘"}
	var saved []string
	for _, task := range []filePreviewTask{first, first, second, {}} {
		id, err := saveFilePreview(dir, block, task)
		if err != nil {
			t.Fatal(err)
		}
		saved = append(saved, id)
	}
	ids, err := listFilePreviews(dir)
	if err != nil {
		t.Fatal(err)
	}
	groups := filePreviewGroups(dir, ids)
	if len(groups) != 3 || groups[0].Title != "实现棋盘" || strings.Join(groups[0].IDs, ",") != strings.Join(saved[:2], ",") ||
		groups[1].Title != "实现棋盘" || groups[1].IDs[0] != saved[2] || groups[2].Title != "历史改动 · main.go" {
		t.Fatalf("incorrect task groups: %+v", groups)
	}
	record, err := loadFilePreview(dir, saved[0])
	if err != nil || record.Version != 2 || record.Task != first {
		t.Fatalf("task metadata not persisted: %+v %v", record.Task, err)
	}
	legacy, err := loadFilePreview(dir, saved[3])
	if err != nil || legacy.Version != 1 {
		t.Fatalf("legacy compatibility lost: version=%d err=%v", legacy.Version, err)
	}
}

func TestFilePreviewGroupedReplay(t *testing.T) {
	fe, notices := headlessFrontend(t)
	t.Setenv("COVE_CONFIG_DIR", t.TempDir())
	dir, err := filePreviewDir(fe.eng.SessionID())
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	printer := newTurnPrinter()
	for index, taskID := range []string{"task-a", "task-a", "task-b"} {
		block := render.Block{Kind: render.KindTool, Tool: "edit", Header: "main.go", Full: "+change-" + time.Duration(index).String() + "\n", Diff: true}
		printer.previewTask = filePreviewTask{ID: taskID, Title: "实现棋盘"}
		printer.fileChange(context.Background(), fe.eng.SessionID(), block, false)
		saved, err := listFilePreviews(dir)
		if err != nil {
			t.Fatal(err)
		}
		if len(saved) != index+1 {
			t.Fatal("automatic file change did not save task metadata")
		}
		ids = saved
	}
	fe.dispatch("/replay list")
	listing := strings.Join(*notices, "\n")
	for _, want := range []string{"1. 实现棋盘 · 2 次改动", "1.1  main.go", "1.2  main.go", "2. 实现棋盘 · 1 次改动", "2.1  main.go"} {
		if !strings.Contains(listing, want) {
			t.Fatalf("missing %q in %q", want, listing)
		}
	}
	for _, test := range []struct {
		command string
		changes []string
	}{
		{"/replay 1", []string{"+change-0s", "+change-1ns"}},
		{"/replay 1.2", []string{"+change-1ns"}},
		{"/replay", []string{"+change-2ns"}},
		{"/replay " + ids[0], []string{"+change-0s"}},
	} {
		*notices = nil
		fe.dispatch(test.command)
		if len(*notices) != len(test.changes) {
			t.Fatalf("%s replayed %d changes", test.command, len(*notices))
		}
		for index, change := range test.changes {
			if !strings.Contains((*notices)[index], "实现棋盘 · main.go\n"+change+"\n") {
				t.Fatalf("%s incorrect change %d: %q", test.command, index, (*notices)[index])
			}
		}
	}
	for _, input := range []string{"/replay 1.0", "/replay 1.3", "/replay 1.x", "/replay 1.2.3"} {
		*notices = nil
		fe.dispatch(input)
		if len(*notices) != 1 || !strings.Contains((*notices)[0], "请选择") {
			t.Fatalf("invalid selection not rejected: %s %v", input, *notices)
		}
	}
	groups := filePreviewGroups(dir, ids)
	selected, err := selectFilePreviews(groups, "", ids[1])
	if err != nil || strings.Join(selected, ",") != strings.Join(ids[:2], ",") {
		t.Fatal("latest selection did not choose the task containing the latest change")
	}
	ctx, cancel := context.WithCancel(context.Background())
	*notices = nil
	fe.print = func(text string) {
		*notices = append(*notices, text)
		cancel()
	}
	fe.replayFilePreview(ctx, []string{"1"})
	if len(*notices) != 1 || !strings.Contains((*notices)[0], "+change-0s") {
		t.Fatal("cancellation did not stop subsequent task changes")
	}
}

func TestTurnUsageFormatting(t *testing.T) {
	if humanTokens(950) != "950" || humanTokens(3200) != "3.2k" || humanTokens(1_500_000) != "1.5M" {
		t.Fatal("humanTokens")
	}
	if humanElapsed(65e9) != "1m05s" || humanElapsed(9e9) != "9s" {
		t.Fatal("humanElapsed")
	}
}

func TestFilePreviewSearch(t *testing.T) {
	dir := t.TempDir()
	var ids []string
	for _, test := range []struct {
		title, path string
	}{
		{"实现棋盘", "demo/chess/main.go"},
		{"实现棋盘", "demo/chess/rules.go"},
		{"修复布局", "demo/chess/main.go"},
		{"", "legacy/main.go"},
	} {
		block := render.Block{Kind: render.KindTool, Tool: "edit", Header: test.path, Summary: "+2 −1", Full: "+code\n", Diff: true}
		task := filePreviewTask{ID: test.title, Title: test.title}
		id, err := saveFilePreview(dir, block, task)
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	for _, test := range []struct {
		query string
		want  []string
	}{
		{"main.go", []string{ids[0], ids[2], ids[3]}},
		{"title:棋盘", ids[:2]},
		{"title:棋盘 file:main.go", ids[:1]},
		{"DEMO\\CHESS\\MAIN.GO", []string{ids[0], ids[2]}},
		{"棋盘 rules.go", []string{ids[1]}},
		{"edit main.go", []string{ids[0], ids[2], ids[3]}},
		{"missing.go", nil},
		{"file:", nil},
		{"title:", nil},
	} {
		got := searchFilePreviews(dir, ids, test.query)
		if strings.Join(got, ",") != strings.Join(test.want, ",") {
			t.Fatalf("query %q: got %v want %v", test.query, got, test.want)
		}
	}
}

func TestFilePreviewSearchCommands(t *testing.T) {
	fe, notices := headlessFrontend(t)
	t.Setenv("COVE_CONFIG_DIR", t.TempDir())
	dir, err := filePreviewDir(fe.eng.SessionID())
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		title, path, change string
	}{
		{"实现棋盘", "demo/chess/rules.go", "+rules"},
		{"实现棋盘", "demo/chess/main.go", "+board"},
		{"修复布局", "demo/chess/main.go", "+layout"},
		{"", "legacy/main.go", "+legacy"},
	} {
		block := render.Block{Kind: render.KindTool, Tool: "edit", Header: test.path, Full: test.change + "\n", Diff: true}
		if _, err := saveFilePreview(dir, block, filePreviewTask{ID: test.title, Title: test.title}); err != nil {
			t.Fatal(err)
		}
	}
	fe.dispatch("/replay list file:main.go")
	listing := strings.Join(*notices, "\n")
	for _, want := range []string{"1.2  demo/chess/main.go", "2.1  demo/chess/main.go", "3.1  legacy/main.go", "匹配 1 次"} {
		if !strings.Contains(listing, want) {
			t.Fatalf("filtered list missing %q: %q", want, listing)
		}
	}
	if strings.Contains(listing, "1.1  demo/chess/rules.go") {
		t.Fatal("filtered list included an unrelated file")
	}
	*notices = nil
	fe.dispatch("/replay 1.2")
	if len(*notices) != 1 || !strings.Contains((*notices)[0], "+board\n") {
		t.Fatal("filtered list changed selection numbering")
	}
	for _, test := range []struct {
		command string
		changes []string
	}{
		{"/replay search file:main.go", []string{"+board", "+layout", "+legacy"}},
		{"/replay search title:棋盘", []string{"+rules", "+board"}},
		{"/replay search title:棋盘 file:main.go", []string{"+board"}},
		{"/replay search DEMO\\CHESS\\MAIN.GO", []string{"+board", "+layout"}},
	} {
		*notices = nil
		fe.dispatch(test.command)
		if len(*notices) != len(test.changes) {
			t.Fatalf("%s replayed %d changes, want %d", test.command, len(*notices), len(test.changes))
		}
		for index, change := range test.changes {
			if !strings.HasSuffix((*notices)[index], change+"\n") {
				t.Fatalf("%s incorrect replay order: %v", test.command, *notices)
			}
		}
	}
	for _, input := range []string{"/replay list missing.go", "/replay search missing.go", "/replay search file:", "/replay search title:"} {
		*notices = nil
		fe.dispatch(input)
		if len(*notices) != 1 || !strings.Contains((*notices)[0], "没有匹配的文件改动") {
			t.Fatalf("no-match search unexpectedly replayed: %s %v", input, *notices)
		}
	}
	*notices = nil
	fe.dispatch("/replay search")
	if len(*notices) != 1 || !strings.Contains((*notices)[0], "用法") {
		t.Fatal("empty search did not show usage")
	}
}

func TestFilePreviewExplanationSegments(t *testing.T) {
	diff := "@@ -1,2 +1,2 @@\n-old\n+new\n context\n"
	explanation, err := parseFilePreviewExplanation(diff, `{"summary":"替换返回值","notes":[{"line":3,"text":"这里是新代码"}]}`)
	if err != nil || explanation.Status != "ready" {
		t.Fatalf("valid explanation rejected: %v", err)
	}
	record := filePreviewRecord{Block: render.Block{Full: diff}, Explanation: explanation}
	var code, notes strings.Builder
	for _, segment := range filePreviewSegments(record) {
		if segment.Note {
			notes.WriteString(segment.Text)
		} else {
			code.WriteString(segment.Text)
		}
	}
	if code.String() != diff || record.Block.Full != diff || !strings.Contains(notes.String(), "不属于源码") || !strings.Contains(notes.String(), "这里是新代码") {
		t.Fatal("explanation modified source diff or lost attribution")
	}
	for _, response := range []string{
		`{"summary":"说明","notes":[{"line":99,"text":"错误"}]}`,
		`{"summary":"说明","notes":[{"line":1,"text":"不是代码行"}]}`,
		`{"summary":"说明","notes":[{"line":3,"text":"一次"},{"line":3,"text":"重复"}]}`,
		`{"summary":""}`,
		`not json`,
	} {
		if _, err := parseFilePreviewExplanation(diff, response); err == nil {
			t.Fatalf("invalid explanation accepted: %s", response)
		}
	}
	explanation, err = parseFilePreviewExplanation(diff, `{"summary":"说明\u001b[2J","notes":[{"line":3,"text":"解释\u001b]0;bad\u0007"}]}`)
	if err != nil || strings.ContainsAny(explanation.Summary+explanation.Notes[0].Text, "\x1b\a") {
		t.Fatal("untrusted terminal controls retained")
	}
}

func TestFilePreviewExplanationCache(t *testing.T) {
	dir := t.TempDir()
	block := render.Block{Kind: render.KindTool, Tool: "edit", Header: "main.go", Full: "@@ -1 +1 @@\n-old\n+new\n", Diff: true}
	id, err := saveFilePreview(dir, block)
	if err != nil {
		t.Fatal(err)
	}
	record, err := loadFilePreview(dir, id)
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	generate := func(ctx context.Context, system, prompt string) (string, error) {
		calls++
		if _, ok := ctx.Deadline(); !ok || !strings.Contains(system, "不可信数据") || !strings.Contains(prompt, "main.go") {
			t.Fatal("generation has no timeout or grounding data")
		}
		return `{"summary":"替换旧值","notes":[{"line":3,"text":"新值"}]}`, nil
	}
	for repeat := 0; repeat < 3; repeat++ {
		updated, err := ensureFilePreviewExplanation(context.Background(), dir, record, generate)
		if err != nil || updated.Explanation == nil || updated.Explanation.Status != "ready" || updated.Block.Full != block.Full {
			t.Fatalf("explanation cache failed: %v", err)
		}
	}
	stored, err := loadFilePreview(dir, id)
	if err != nil || stored.Version != 3 || stored.Explanation == nil || stored.Explanation.Summary != "替换旧值" || calls != 1 {
		t.Fatalf("cache not persisted or regenerated: calls=%d err=%v", calls, err)
	}
	id, err = saveFilePreview(dir, block)
	if err != nil {
		t.Fatal(err)
	}
	record, err = loadFilePreview(dir, id)
	if err != nil {
		t.Fatal(err)
	}
	failures := 0
	for repeat := 0; repeat < 2; repeat++ {
		updated, err := ensureFilePreviewExplanation(context.Background(), dir, record, func(context.Context, string, string) (string, error) {
			failures++
			return "", fmt.Errorf("provider failed with sensitive details")
		})
		if err != nil || updated.Explanation.Status != "failed" || updated.Block.Full != block.Full || strings.Contains(updated.Explanation.Error, "sensitive") {
			t.Fatal("failed explanation did not preserve code safely")
		}
	}
	if failures != 1 {
		t.Fatal("failed explanation retried on every replay")
	}
}

func TestFilePreviewExplanationReplayIntegration(t *testing.T) {
	fe, notices := headlessFrontend(t)
	t.Setenv("COVE_REPLAY_EXPLAIN", "1")
	t.Setenv("COVE_FILE_PREVIEW", "1")
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		calls.Add(1)
		var payload map[string]any
		if err := json.NewDecoder(request.Body).Decode(&payload); err != nil {
			t.Error(err)
		}
		if tools, ok := payload["tools"].([]any); ok && len(tools) > 0 {
			t.Error("explanation request enabled tools")
		}
		writer.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(writer).Encode(map[string]any{
			"choices": []any{map[string]any{"message": map[string]any{"role": "assistant", "content": `{"summary":"替换旧值","notes":[{"line":3,"text":"这里使用新值"}]}`}, "finish_reason": "stop"}},
			"usage":   map[string]int{"prompt_tokens": 20, "completion_tokens": 10},
		})
	}))
	t.Cleanup(server.Close)
	if err := fe.eng.ReloadProvider("openai-compatible", "test-model", server.URL, "placeholder"); err != nil {
		t.Fatal(err)
	}
	source := "new\n"
	if err := os.WriteFile("main.go", []byte(source), 0600); err != nil {
		t.Fatal(err)
	}
	dir, err := filePreviewDir(fe.eng.SessionID())
	if err != nil {
		t.Fatal(err)
	}
	block := render.Block{Kind: render.KindTool, Tool: "edit", Header: "main.go", Full: "@@ -1 +1 @@\n-old\n+new\n", Diff: true}
	id, err := saveFilePreview(dir, block)
	if err != nil {
		t.Fatal(err)
	}
	output := captureTurnOutput(t)
	fe.tasks = &replTaskRunner{}
	for repeat := 0; repeat < 2; repeat++ {
		fe.replayFilePreview(context.Background(), []string{id})
	}
	if calls.Load() != 1 || !strings.Contains(output.String(), "AI 解读（可能有误，不属于源码）") || !strings.Contains(output.String(), "这里使用新值") || !strings.Contains(output.String(), "+new") {
		t.Fatalf("replay did not generate and reuse explanation: calls=%d output=%q", calls.Load(), output.String())
	}
	data, err := os.ReadFile("main.go")
	if err != nil || string(data) != source {
		t.Fatal("replay modified the source file")
	}
	record, err := loadFilePreview(dir, id)
	if err != nil || record.Block.Full != block.Full || record.Explanation == nil {
		t.Fatal("replay modified or failed to save the original diff")
	}
	fe.tasks = nil
	*notices = nil
	fe.replayFilePreview(context.Background(), []string{id})
	if calls.Load() != 1 || !strings.Contains(strings.Join(*notices, "\n"), "这里使用新值") {
		t.Fatal("headless replay did not reuse cached explanation")
	}
	t.Setenv("COVE_REPLAY_EXPLAIN", "0")
	*notices = nil
	fe.replayFilePreview(context.Background(), []string{id})
	if calls.Load() != 1 || len(*notices) != 1 || (*notices)[0] != "main.go\n"+block.Full {
		t.Fatal("disabled explanation changed plain replay")
	}
	t.Setenv("COVE_REPLAY_EXPLAIN", "1")
	id, err = saveFilePreview(dir, block)
	if err != nil {
		t.Fatal(err)
	}
	*notices = nil
	fe.replayFilePreview(context.Background(), []string{id})
	if calls.Load() != 1 || len(*notices) != 1 || (*notices)[0] != "main.go\n"+block.Full {
		t.Fatal("headless replay generated an uncached explanation")
	}
}

func TestFilePreviewExplanationFailureGuards(t *testing.T) {
	dir := t.TempDir()
	create := func(diff string) filePreviewRecord {
		t.Helper()
		block := render.Block{Kind: render.KindTool, Tool: "edit", Header: "main.go", Full: diff, Diff: true}
		id, err := saveFilePreview(dir, block)
		if err != nil {
			t.Fatal(err)
		}
		record, err := loadFilePreview(dir, id)
		if err != nil {
			t.Fatal(err)
		}
		return record
	}
	record := create("+new\n")
	ctx, cancel := context.WithCancel(context.Background())
	_, err := ensureFilePreviewExplanation(ctx, dir, record, func(context.Context, string, string) (string, error) {
		cancel()
		return `{"summary":"说明"}`, nil
	})
	stored, loadErr := loadFilePreview(dir, record.ID)
	if err == nil || loadErr != nil || stored.Explanation != nil || stored.Block.Full != record.Block.Full {
		t.Fatal("cancelled generation changed the record")
	}
	record = create(strings.Repeat("+code\n", 6000))
	updated, err := ensureFilePreviewExplanation(context.Background(), dir, record, func(context.Context, string, string) (string, error) {
		t.Fatal("oversized diff was sent to the model")
		return "", nil
	})
	if err != nil || updated.Explanation.Status != "skipped" || updated.Block.Full != record.Block.Full {
		t.Fatal("large diff fallback failed")
	}
	record = create("+new\n")
	_, err = ensureFilePreviewExplanation(context.Background(), dir, record, func(context.Context, string, string) (string, error) {
		changed := record
		changed.Block.Full = "+other\n"
		data, err := json.Marshal(changed)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, record.ID+".json"), data, 0600); err != nil {
			t.Fatal(err)
		}
		return `{"summary":"旧记录说明"}`, nil
	})
	stored, loadErr = loadFilePreview(dir, record.ID)
	if err == nil || loadErr != nil || stored.Explanation != nil || stored.Block.Full != "+other\n" {
		t.Fatal("stale explanation overwrote a changed record")
	}
	record = create("+new\n")
	updated, err = ensureFilePreviewExplanation(context.Background(), dir, record, func(context.Context, string, string) (string, error) {
		_, lockedErr := ensureFilePreviewExplanation(context.Background(), dir, record, func(context.Context, string, string) (string, error) {
			t.Fatal("another caller generated under the same record lock")
			return "", nil
		})
		if lockedErr == nil {
			t.Fatal("concurrent generation was not locked")
		}
		return `{"summary":"说明"}`, nil
	})
	if err != nil || updated.Explanation.Status != "ready" {
		t.Fatal("record lock prevented its owner from finishing")
	}
}

func TestFilePreviewPicker(t *testing.T) {
	fe, notices := headlessFrontend(t)
	dir, err := filePreviewDir(fe.eng.SessionID())
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, path := range []string{"main.go", "rules.go"} {
		id, err := saveFilePreview(dir, render.Block{Kind: render.KindTool, Tool: "edit", Header: path, Full: "+code\n", Diff: true}, filePreviewTask{ID: "one", Title: "实现棋盘"})
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	fe.tasks = &replTaskRunner{}
	var offered []repl.Choice
	fe.choose = func(title string, choices []repl.Choice) bool {
		if title != "文件改动回放" {
			t.Errorf("incorrect picker title: %q", title)
		}
		offered = choices
		return true
	}
	fe.dispatch("/replay list")
	if len(offered) != 3 || offered[0].Value != "/replay 1" || offered[1].Value != "/replay "+ids[0] || !strings.Contains(offered[1].Label, "实现棋盘") {
		t.Fatalf("incorrect replay choices: %+v", offered)
	}
	offered = nil
	fe.dispatch("/replay list file:rules.go")
	if len(offered) != 1 || offered[0].Value != "/replay "+ids[1] || !strings.HasPrefix(offered[0].Label, "1.2") {
		t.Fatalf("filtered picker includes unrelated changes: %+v", offered)
	}
	offered = nil
	fe.tasks = nil
	fe.dispatch("/replay list")
	if offered != nil || !strings.Contains(strings.Join(*notices, "\n"), "1.2  rules.go") {
		t.Fatal("headless replay lost its text list")
	}
}
