package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/liuzhixin405/cove-agent/internal/api"
	"github.com/liuzhixin405/cove-agent/internal/checkpoint"
	"github.com/liuzhixin405/cove-agent/internal/config"
	"github.com/liuzhixin405/cove-agent/internal/memory"
	"github.com/liuzhixin405/cove-agent/internal/tool"
)

func TestParallelWorkflowCommandsUseRealFrontendRegistry(t *testing.T) {
	registry := (&frontend{}).install(registerAllCommands())
	for _, name := range []string{"automations", "inbox", "browser-verify", "race", "remote"} {
		entry, exists := registry.Find(name)
		if !exists || entry.Help() == "" {
			t.Fatalf("missing real frontend command: %s", name)
		}
	}
}

func TestFrontendWorkflowShutdownClosesRegisteredRace(t *testing.T) {
	fe := &frontend{}
	fe.install(registerAllCommands())
	entry, exists := fe.reg.Find("race")
	if !exists {
		t.Fatal("race missing")
	}
	fe.closeWorkflows()
	fe.closeWorkflows()
	if _, err := entry.(*RaceCommand).initialize(); err == nil {
		t.Fatal("race remains open after frontend shutdown")
	}
}

// headlessFrontend is the headless front end's command dispatch with its
// notices captured.
func headlessFrontend(t *testing.T) (*frontend, *[]string) {
	t.Helper()
	var notices []string
	fe := &frontend{eng: newTestEngine(t), cfg: config.DefaultConfig(), toolReg: tool.NewRegistry(),
		print:   func(s string) { notices = append(notices, s) },
		enqueue: func(api.Message) {},
	}
	fe.install(registerAllCommands())
	return fe, &notices
}

// Headless answered "未找到命令" for /continue and /clear, and bare /model
// and /mode were unknown in both front ends (only "/model x" was matched).
func TestHeadlessKnowsTheFrontEndCommands(t *testing.T) {
	out := captureOut(t)
	fe, notices := headlessFrontend(t)
	for _, in := range []string{"/continue", "/clear", "/model", "/mode", "/tasks", "/tasks saved", "/acceptance", "/stop"} {
		*notices = nil
		out.Reset()
		if !fe.dispatch(in) {
			t.Fatalf("%s not dispatched", in)
		}
		all := strings.Join(*notices, "\n") + out.String()
		if strings.Contains(all, "未找到命令") || strings.Contains(all, "Unknown") {
			t.Errorf("%s is unknown in headless: %s", in, all)
		}
		if strings.TrimSpace(all) == "" {
			t.Errorf("%s said nothing", in)
		}
	}
}

// One list: every registered command is offered by completion and listed
// by /help.
func TestCompletionAndHelpComeFromTheRegistry(t *testing.T) {
	reg := (&frontend{}).install(registerAllCommands())
	entries := map[string]cmdEntry{}
	for _, e := range buildCommandList(reg, tool.NewRegistry()) {
		entries[e.Name] = e
	}
	out := captureOut(t)
	printHelp(reg, tool.NewRegistry(), nil)
	help := out.String()
	for _, c := range reg.All() {
		if _, ok := entries["/"+c.Name()]; !ok {
			t.Errorf("/%s is registered but not offered by completion", c.Name())
		}
		if !strings.Contains(help, "/"+c.Name()+" ") && !strings.Contains(help, "/"+c.Name()+"\n") {
			t.Errorf("/%s is registered but not in /help", c.Name())
		}
	}
	if hints := entries["/mode"].ArgHints[""]; len(hints) != 4 {
		t.Errorf("/mode completion hints = %v", hints)
	}
	if hints := entries["/tasks"].ArgHints[""]; strings.Join(hints, ",") != "saved,restore,remove,move,run,retry,skip" {
		t.Errorf("/tasks completion hints = %v", hints)
	}
}

// A front-end command registered over a generic one replaces it: one entry
// per name, and the generic command still handles what the front-end one
// does not (/config <key> <value>).
func TestFrontEndCommandsReplaceGenericOnes(t *testing.T) {
	reg := (&frontend{}).install(registerAllCommands())
	seen := map[string]int{}
	for _, c := range reg.All() {
		seen[c.Name()]++
	}
	for name, n := range seen {
		if n != 1 {
			t.Errorf("/%s listed %d times", name, n)
		}
	}
	c, ok := reg.Find("config")
	if !ok {
		t.Fatal("/config not registered")
	}
	if fc, ok := c.(*feCmd); !ok || fc.base == nil {
		t.Fatalf("/config is not the front-end command over the generic one: %T", c)
	}
}

func TestContinueCannotDiscardUncertainRecoveredTask(t *testing.T) {
	fe, notices := headlessFrontend(t)
	fe.tasks = newREPLTaskRunner(fe.eng)
	fe.tasks.paused = true
	fe.tasks.pendingFailedMsg = &api.Message{Role: "user", Content: "uncertain"}
	fe.tasks.queue = []api.Message{{Role: "user", Content: "later task"}}
	fe.dispatch("/continue")
	fe.continueTyped("继续")
	snapshot := fe.tasks.Snapshot()
	if snapshot.Running || snapshot.PendingRetry != "uncertain" || len(snapshot.Queued) != 1 || snapshot.Queued[0] != "later task" || !strings.Contains(strings.Join(*notices, "\n"), "/tasks retry") {
		t.Fatalf("continue changed paused recovery: %+v, %v", snapshot, *notices)
	}
}

func TestFrontendSelectiveUndoUsesRealAdapterAndQuotedPaths(t *testing.T) {
	eng := steerTestEngine(t)
	cwd, _ := os.Getwd()
	selected := filepath.Join(cwd, "selected file.txt")
	keep := filepath.Join(cwd, "keep.txt")
	if err := os.WriteFile(selected, []byte("before"), 0600); err != nil {
		t.Fatal(err)
	}
	manager, err := checkpoint.New(cwd)
	if err != nil {
		t.Fatal(err)
	}
	target, err := manager.Create("before")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(selected, []byte("agent changed"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keep, []byte("user changes"), 0600); err != nil {
		t.Fatal(err)
	}
	output := captureOut(t)
	fe := &frontend{eng: eng, cfg: config.DefaultConfig(), print: func(string) {}}
	fe.install(registerAllCommands())
	fe.dispatch("/undo files " + target + " \"selected file.txt\"")
	var token string
	fields := strings.Fields(output.String())
	for index, word := range fields {
		if strings.Contains(word, "选择性回滚预览") && index+1 < len(fields) {
			token = fields[index+1]
			break
		}
	}
	if token == "" {
		t.Fatalf("preview not exposed through adapter: %s", output.String())
	}
	data, _ := os.ReadFile(selected)
	if string(data) != "agent changed" {
		t.Fatal("command preview changed the file")
	}
	fe.dispatch("/undo apply " + token)
	data, _ = os.ReadFile(selected)
	unselected, _ := os.ReadFile(keep)
	if string(data) != "before" || string(unselected) != "user changes" {
		t.Fatalf("command restore failed: %q, %q; output: %s", data, unselected, output.String())
	}
}

func TestFrontendMemorySourceRecordsManualSession(t *testing.T) {
	eng := steerTestEngine(t)
	store := memory.NewStoreForDirs(t.TempDir())
	output := captureOut(t)
	fe := &frontend{eng: eng, cfg: config.DefaultConfig(), memStore: store, print: func(string) {}}
	fe.install(registerAllCommands())
	fe.dispatch("/memory add facts.md durable fact")
	fe.dispatch("/memory source facts.md")
	record, err := store.Provenance("facts.md")
	if err != nil || record == nil || len(record.Sources) != 1 || record.Sources[0].Kind != "manual" || record.Sources[0].SessionIDs[0] != eng.SessionID() || !strings.Contains(output.String(), eng.SessionID()) {
		t.Fatalf("manual source missing: %+v, %v; output: %s", record, err, output.String())
	}
}

func TestFrontendSelectiveUndoApprovalDoesNotSurviveSessionSwitch(t *testing.T) {
	eng := steerTestEngine(t)
	cwd, _ := os.Getwd()
	path := filepath.Join(cwd, "selected.txt")
	if err := os.WriteFile(path, []byte("before"), 0600); err != nil {
		t.Fatal(err)
	}
	manager, err := checkpoint.New(cwd)
	if err != nil {
		t.Fatal(err)
	}
	target, err := manager.Create("before")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("after"), 0600); err != nil {
		t.Fatal(err)
	}
	output := captureOut(t)
	fe := &frontend{eng: eng, cfg: config.DefaultConfig(), print: func(string) {}}
	fe.install(registerAllCommands())
	fe.dispatch("/undo files " + target + " selected.txt")
	var token string
	fields := strings.Fields(output.String())
	for index, word := range fields {
		if strings.Contains(word, "选择性回滚预览") && index+1 < len(fields) {
			token = fields[index+1]
			break
		}
	}
	if token == "" {
		t.Fatalf("no preview: %s", output.String())
	}
	fe.dispatch("/new")
	fe.dispatch("/undo apply " + token)
	data, _ := os.ReadFile(path)
	if string(data) != "after" || !strings.Contains(output.String(), "没有有效预览") {
		t.Fatalf("approval survived session switch: %q, %s", data, output.String())
	}
}
