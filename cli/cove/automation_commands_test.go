package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/liuzhixin405/cove-agent/internal/automation"
	"github.com/liuzhixin405/cove-agent/internal/command"
	"github.com/liuzhixin405/cove-agent/internal/config"
)

func TestAutomationCommandsLazyMetadataAndOperatorFlow(t *testing.T) {
	t.Setenv("COVE_CONFIG_DIR", t.TempDir())
	project := t.TempDir()
	var output string
	fe := &frontend{print: func(message string) { output = message }}
	var nilFrontend *frontend
	commands := nilFrontend.automationCommands()
	if len(commands) != 2 || commands[0].Name() != "automations" || !strings.Contains(commands[1].Help(), "accepted does not apply or merge") {
		t.Fatalf("metadata=%v", commands)
	}
	commands = fe.automationCommands()
	spec := automationSpecFile{Version: 1, Spec: automation.Spec{ID: "daily", Enabled: true, Prompt: "maintenance", TimeoutSeconds: 60, BudgetUSD: 1, MaxTurns: 3, EverySeconds: 60}}
	data, err := json.Marshal(spec)
	if err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(project, "spec.json")
	if err := os.WriteFile(file, data, 0600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"add", file}, {"list"}, {"remove", "daily"}} {
		if _, err := commands[0].Execute(context.Background(), command.Input{Cwd: project, Args: args}); err != nil {
			t.Fatal(err)
		}
		if strings.HasPrefix(output, "automation:") {
			t.Fatal(output)
		}
		if args[0] == "list" && !strings.Contains(output, `"id": "daily"`) {
			t.Fatal(output)
		}
	}
	store, err := automationStore(project)
	if err != nil {
		t.Fatal(err)
	}
	state, err := store.Read()
	if err != nil || len(state.Specs) != 0 {
		t.Fatalf("state=%+v err=%v", state, err)
	}
}

func TestAutomationCLIExplicitDispatchAndNestedRefusal(t *testing.T) {
	t.Setenv("COVE_CONFIG_DIR", t.TempDir())
	var stdout, stderr bytes.Buffer
	handled, code := RunAutomationCLI(context.Background(), []string{"--version"}, &stdout, &stderr)
	if handled || code != 0 {
		t.Fatal("intercepted unrelated CLI")
	}
	handled, code = RunAutomationCLI(context.Background(), []string{"--automation", "list", t.TempDir()}, &stdout, &stderr)
	if !handled || code != 0 {
		t.Fatalf("handled=%v code=%d stderr=%s", handled, code, stderr.String())
	}
	t.Setenv("COVE_AUTOMATION_CHILD", "1")
	_, err := executeAutomationCommand(context.Background(), "automations", command.Input{Cwd: t.TempDir(), Args: []string{"tick"}}, nil)
	if err == nil || !strings.Contains(err.Error(), "nested") {
		t.Fatalf("nested error=%v", err)
	}
}

func TestAutomationRunnerPrivateBudgetConfiguration(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.MaxBudgetUsd = 20
	cfg.ActiveProfile = "expensive"
	cfg.Profiles = map[string]*config.Profile{"expensive": {MaxBudgetUsd: 50}}
	cfg.Provider.APIKey = "sk-real-1234567890"
	runner, err := automationRunner(cfg)
	if err != nil {
		t.Fatal(err)
	}
	commandRunner := runner.(automation.CommandRunner)
	env, cleanup, err := commandRunner.Prepare(t.TempDir(), .25, automation.Spec{MaxTurns: 4})
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	dir := strings.TrimPrefix(env[0], "COVE_CONFIG_DIR=")
	data, err := os.ReadFile(filepath.Join(dir, "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	var isolated config.Config
	if err := json.Unmarshal(data, &isolated); err != nil {
		t.Fatal(err)
	}
	if isolated.MaxBudgetUsd != .25 || isolated.MaxIterations != 4 || isolated.ActiveProfile != "" || len(isolated.Profiles) != 0 || isolated.PermissionMode != "auto" || isolated.DoneVerifyAuto == nil || *isolated.DoneVerifyAuto {
		t.Fatalf("isolated=%+v", isolated)
	}
	// The worker reads this file with config.Load, which clears a masked
	// key: the private config must carry the real one or every run fails
	// authentication.
	if !strings.Contains(string(data), `"sk-real-1234567890"`) || strings.Contains(string(data), "****") {
		t.Fatalf("private config does not carry the real API key: %s", data)
	}
	if cfg.MaxBudgetUsd != 20 || cfg.ActiveProfile != "expensive" {
		t.Fatal("mutated parent config")
	}
	if env[1] != "COVE_AUTOMATION_CHILD=1" {
		t.Fatalf("env=%v", env)
	}
}

type automationCLIForbiddenRunner struct{ calls int }

func (runner *automationCLIForbiddenRunner) Run(context.Context, string, automation.Spec, float64) (automation.Check, error) {
	runner.calls++
	return automation.Check{ExitCode: 0}, nil
}

func TestAutomationInboxFailedResultReviewAndExport(t *testing.T) {
	t.Setenv("COVE_CONFIG_DIR", t.TempDir())
	project := t.TempDir()
	store, err := automationStore(project)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Add(automation.Spec{ID: "manual", Enabled: true, Prompt: "maintain", TimeoutSeconds: 60, BudgetUSD: 1, MaxTurns: 3}, time.Now()); err != nil {
		t.Fatal(err)
	}
	runner := &automationCLIForbiddenRunner{}
	_, err = executeAutomationCommand(context.Background(), "automations", command.Input{Cwd: project, Args: []string{"run", "manual"}}, runner)
	if err == nil || !strings.Contains(err.Error(), "requires a Git repository") || runner.calls != 0 {
		t.Fatalf("run err=%v calls=%d", err, runner.calls)
	}
	state, err := store.Read()
	if err != nil || len(state.Results) != 1 {
		t.Fatalf("inbox=%+v err=%v", state, err)
	}
	id := state.Results[0].ID
	message, err := executeAutomationCommand(context.Background(), "inbox", command.Input{Cwd: project, Args: []string{"show", id}}, nil)
	if err != nil || !strings.Contains(message, `"state": "failed"`) {
		t.Fatalf("show=%s err=%v", message, err)
	}
	message, err = executeAutomationCommand(context.Background(), "inbox", command.Input{Cwd: project, Args: []string{"review", id, "rejected"}}, nil)
	if err != nil || !strings.Contains(message, "nothing applied or merged") {
		t.Fatalf("review=%s err=%v", message, err)
	}
	_, err = executeAutomationCommand(context.Background(), "inbox", command.Input{Cwd: project, Args: []string{"patch", id, filepath.Join(project, "result.patch")}}, nil)
	if err == nil || !strings.Contains(err.Error(), "outside the original project") {
		t.Fatalf("root export accepted: %v", err)
	}
	state, err = store.Read()
	if err != nil || state.Results[0].Review != "rejected" {
		t.Fatalf("review state=%+v err=%v", state, err)
	}
	entries, err := os.ReadDir(project)
	if err != nil || len(entries) != 0 {
		t.Fatalf("original changed: %v %v", entries, err)
	}
}

func TestAutomationFrontendTaskBoundary(t *testing.T) {
	t.Setenv("COVE_CONFIG_DIR", t.TempDir())
	fe, notices := headlessFrontend(t)
	fe.tasks = &replTaskRunner{running: true}
	for _, line := range []string{"/automations add spec.json", "/automations remove daily", "/automations run daily", "/automations tick", "/automations event tests key", "/inbox review result accepted", "/inbox patch result export.patch"} {
		*notices = nil
		if !fe.mutates(line) || !fe.dispatch(line) || !strings.Contains(strings.Join(*notices, "\n"), "任务运行中不能执行") {
			t.Fatalf("write command not blocked: %s %v", line, *notices)
		}
	}
	for _, line := range []string{"/automations", "/automations list", "/automations help", "/inbox list", "/inbox show result", "/inbox patch result"} {
		*notices = nil
		if fe.mutates(line) || !fe.dispatch(line) || strings.Contains(strings.Join(*notices, "\n"), "任务运行中不能执行") {
			t.Fatalf("read command blocked: %s %v", line, *notices)
		}
	}
}

func TestAutomationRealCLIStartup(t *testing.T) {
	caller, project, configDir := t.TempDir(), t.TempDir(), t.TempDir()
	executable := filepath.Join(t.TempDir(), "cove")
	if runtime.GOOS == "windows" {
		executable += ".exe"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	build := exec.CommandContext(ctx, "go", "build", "-o", executable, ".")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build real CLI: %v %s", err, output)
	}
	spec := automationSpecFile{Version: 1, Spec: automation.Spec{ID: "manual", Enabled: true, Prompt: "maintain", Event: "test-event", TimeoutSeconds: 1, BudgetUSD: 1, MaxTurns: 1}}
	data, err := json.Marshal(spec)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(caller, "spec.json"), data, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(caller, ".cove.json"), []byte("invalid caller configuration"), 0600); err != nil {
		t.Fatal(err)
	}
	run := func(code int, contains string, child bool, args ...string) string {
		t.Helper()
		commandCtx, stop := context.WithTimeout(context.Background(), 20*time.Second)
		defer stop()
		process := exec.CommandContext(commandCtx, executable, args...)
		process.Dir = caller
		for _, entry := range os.Environ() {
			key, _, _ := strings.Cut(entry, "=")
			if !strings.EqualFold(key, "COVE_CONFIG_DIR") && !strings.EqualFold(key, "COVE_AUTOMATION_CHILD") && !strings.HasPrefix(strings.ToUpper(key), "GIT_") {
				process.Env = append(process.Env, entry)
			}
		}
		process.Env = append(process.Env, "COVE_CONFIG_DIR="+configDir)
		if child {
			process.Env = append(process.Env, "COVE_AUTOMATION_CHILD=1")
		}
		output, err := process.CombinedOutput()
		actualCode := 0
		if err != nil {
			if exit, ok := err.(*exec.ExitError); ok {
				actualCode = exit.ExitCode()
			} else {
				t.Fatal(err)
			}
		}
		if actualCode != code || !strings.Contains(string(output), contains) {
			t.Fatalf("CLI %v: code=%d want=%d output=%s", args, actualCode, code, output)
		}
		return string(output)
	}
	run(0, "null", false, "--automation", "list", project)
	run(0, "null", false, "--automation", "tick", project)
	run(0, "Added manual", false, "--automation", "add", project, "spec.json")
	run(0, `"id": "manual"`, false, "--automation", "list", project)
	run(1, "requires a Git repository", false, "--automation", "run", project, "manual")
	store, err := automation.Open(filepath.Join(configDir, "automations"), project)
	if err != nil {
		t.Fatal(err)
	}
	state, err := store.Read()
	if err != nil || len(state.Results) != 1 || state.Results[0].State != "failed" {
		t.Fatalf("noGit result not persisted: %+v %v", state, err)
	}
	resultID := state.Results[0].ID
	run(0, resultID, false, "--automation-inbox", "list", project)
	run(0, `"state": "failed"`, false, "--automation-inbox", "show", project, resultID)
	run(0, "nothing applied or merged", false, "--automation-inbox", "review", project, resultID, "rejected")
	run(0, `"review": "rejected"`, false, "--automation-inbox", "show", project, resultID)
	run(1, "outside the original project", false, "--automation-inbox", "patch", project, resultID, filepath.Join(project, "unsafe.patch"))
	run(0, "nothing applied", false, "--automation-inbox", "patch", project, resultID, "export.patch")
	if _, err := os.Stat(filepath.Join(caller, "export.patch")); err != nil {
		t.Fatalf("CLI export did not use caller cwd: %v", err)
	}
	run(1, "requires a Git repository", false, "--automation", "event", project, "test-event", "unique-key")
	run(1, "event already claimed", false, "--automation", "event", project, "test-event", "unique-key")
	state, err = store.Read()
	if err != nil || len(state.Results) != 2 || state.Results[1].EventKey != "unique-key" {
		t.Fatalf("event duplicated across processes: %+v %v", state, err)
	}
	due := spec.Spec
	due.ID, due.EverySeconds, due.Event = "scheduled", 60, ""
	if err := store.Add(due, time.Now().Add(-2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	run(1, "requires a Git repository", false, "--automation", "tick", project)
	state, err = store.Read()
	if err != nil || len(state.Results) != 3 || state.Results[2].Trigger != "schedule" || state.Results[2].State != "failed" {
		t.Fatalf("noGit scheduled failure not persisted: %+v %v", state, err)
	}
	run(1, "nested", true, "--automation", "list", project)
	run(1, "nested", true, "--automation", "tick", project)
	run(1, "nested", true, "--automation-inbox", "list", project)
	run(2, "Usage:", false, "--automation", "list")
	run(2, "--not-a-real-option", false, "--not-a-real-option", "--automation", "list", project)
	run(1, "/automations list", false, "--automation", "list", project, "--not-a-real-option")
	run(1, "active session", false, "--automation", "add", project, "spec.json", "session")
	if err := os.WriteFile(filepath.Join(project, ".cove.json"), []byte("invalid target configuration"), 0600); err != nil {
		t.Fatal(err)
	}
	run(1, "parse project config", false, "--automation", "tick", project)
	run(1, "nested", true, "--automation", "tick", project)
}

func TestAutomationInboxExportAndConfigBoundaries(t *testing.T) {
	configDir, project := t.TempDir(), t.TempDir()
	t.Setenv("COVE_CONFIG_DIR", configDir)
	store, err := automationStore(project)
	if err != nil {
		t.Fatal(err)
	}
	state := automation.Snapshot{Project: project, Results: []automation.Result{{ID: "fixture", Patch: "fixture patch"}}}
	for _, destination := range []string{"relative.patch", filepath.Join(project, "absolute.patch"), filepath.Join(configDir, "config.patch"), store.Path()} {
		if _, err := executeInbox(store, state, []string{"patch", "fixture", destination}); err == nil {
			t.Fatalf("unsafe export accepted: %s", destination)
		}
	}
	destination := filepath.Join(t.TempDir(), "export.patch")
	if _, err := executeInbox(store, state, []string{"patch", "fixture", destination}); err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(destination); err != nil || string(data) != "fixture patch" {
		t.Fatalf("export=%q err=%v", data, err)
	}
	if _, err := executeInbox(store, state, []string{"patch", "fixture", destination}); err == nil {
		t.Fatal("existing export overwritten")
	}
	t.Setenv("COVE_CONFIG_DIR", project)
	if _, err := automationStore(project); err == nil {
		t.Fatal("config directory inside project accepted")
	}
}

func TestAutomationSessionScopedCommands(t *testing.T) {
	t.Setenv("COVE_CONFIG_DIR", t.TempDir())
	project := t.TempDir()
	fe, _ := headlessFrontend(t)
	sessionID := fe.eng.SessionID()
	if sessionID == "" {
		t.Fatal("fixture requires an active session")
	}
	spec := automationSpecFile{Version: 1, Spec: automation.Spec{ID: "scoped", Enabled: true, Prompt: "maintain", EverySeconds: 60, Event: "test-event", TimeoutSeconds: 1, BudgetUSD: 1, MaxTurns: 1}}
	data, err := json.Marshal(spec)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(project, "spec.json"), data, 0600); err != nil {
		t.Fatal(err)
	}
	in := command.Input{Cwd: project, Args: []string{"add", "spec.json", "session"}, Engine: replEngineAdapter{eng: fe.eng}}
	if _, err := executeAutomationCommand(context.Background(), "automations", in, nil); err != nil {
		t.Fatal(err)
	}
	store, err := automationStore(project)
	if err != nil {
		t.Fatal(err)
	}
	state, err := store.Read()
	if err != nil || len(state.Specs) != 1 || state.Specs[0].SessionID != sessionID {
		t.Fatalf("scope not persisted: %+v %v", state, err)
	}
	runner := &automationCLIForbiddenRunner{}
	if _, err := executeAutomationCommand(context.Background(), "automations", command.Input{Cwd: project, Args: []string{"run", "scoped"}}, runner); err == nil || !strings.Contains(err.Error(), "different session") {
		t.Fatalf("headless session run accepted: %v", err)
	}
	if _, err := executeAutomationCommand(context.Background(), "automations", command.Input{Cwd: project, Args: []string{"event", "test-event", "key"}}, runner); err != nil {
		t.Fatal(err)
	}
	state, err = store.Read()
	if err != nil || len(state.Results) != 0 || runner.calls != 0 {
		t.Fatalf("wrong session executed: %+v calls=%d err=%v", state, runner.calls, err)
	}
}

func TestAutomationCLIConfigRestoresCallerState(t *testing.T) {
	caller, project := t.TempDir(), t.TempDir()
	t.Chdir(caller)
	t.Setenv("COVE_CONFIG_DIR", "relative-config")
	if err := os.Mkdir("relative-config", 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(caller, "relative-config", "config.json"), []byte(`{"model":"fixture-global-model"}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(project, ".cove.json"), []byte(`{"model":"fixture-project-model"}`), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := loadAutomationCLIConfig(project)
	if err != nil || cfg.Model != "fixture-project-model" {
		t.Fatalf("target configuration not loaded: err=%v", err)
	}
	cwd, err := os.Getwd()
	if err != nil || !strings.EqualFold(cwd, caller) || os.Getenv("COVE_CONFIG_DIR") != "relative-config" {
		t.Fatalf("caller cwd/environment not restored: %s %v", cwd, err)
	}
}
