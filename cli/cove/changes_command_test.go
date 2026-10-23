package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/liuzhixin405/cove-agent/internal/api"
	"github.com/liuzhixin405/cove-agent/internal/automation"
	"github.com/liuzhixin405/cove-agent/internal/changeset"
	"github.com/liuzhixin405/cove-agent/internal/command"
	"github.com/liuzhixin405/cove-agent/internal/config"
	"github.com/liuzhixin405/cove-agent/internal/engine"
	"github.com/liuzhixin405/cove-agent/internal/permission"
	"github.com/liuzhixin405/cove-agent/internal/race"
	"github.com/liuzhixin405/cove-agent/internal/repl"
	"github.com/liuzhixin405/cove-agent/internal/termui"
	"github.com/liuzhixin405/cove-agent/internal/tool"
	"github.com/liuzhixin405/cove-agent/internal/workspace"
)

// changesRunner is a maintenance runner that writes new.txt in its worktree.
type changesRunner struct{}

func (changesRunner) Run(ctx context.Context, worktree string, spec automation.Spec, budgetUSD float64) (automation.Check, error) {
	if err := os.WriteFile(filepath.Join(worktree, "new.txt"), []byte("maintenance patch\n"), 0600); err != nil {
		return automation.Check{ExitCode: -1}, err
	}
	return automation.Check{ExitCode: 0, Output: "fixture complete"}, nil
}

type changesEnv struct {
	fe                 *frontend
	out                *lockedBuffer
	project, configDir string
}

// newChangesFixture is a clean committed Git project (the working directory)
// with an engine over it and a front end with every command installed.
func newChangesFixture(t *testing.T, interactive bool) *changesEnv {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	configDir := t.TempDir()
	t.Setenv("COVE_CONFIG_DIR", configDir)
	t.Setenv("GIT_AUTHOR_NAME", "fixture")
	t.Setenv("GIT_AUTHOR_EMAIL", "fixture@example.invalid")
	t.Setenv("GIT_COMMITTER_NAME", "fixture")
	t.Setenv("GIT_COMMITTER_EMAIL", "fixture@example.invalid")
	t.Setenv("COVE_FILE_PREVIEW", "0")
	t.Setenv("COVE_REPLAY_EXPLAIN", "0")
	project := t.TempDir()
	git := func(args ...string) {
		t.Helper()
		if _, err := workspace.Git(context.Background(), project, nil, args...); err != nil {
			t.Fatal(err)
		}
	}
	git("init", "-b", "main")
	git("config", "core.autocrlf", "false")
	if err := os.WriteFile(filepath.Join(project, "value.txt"), []byte("base\n"), 0600); err != nil {
		t.Fatal(err)
	}
	git("add", ".")
	git("-c", "user.name=fixture", "-c", "user.email=fixture@example.invalid", "commit", "-m", "fixture")
	t.Chdir(project)
	eng, err := engine.New(engine.Config{
		Model:          "test-model",
		PermissionMode: "auto",
		MaxBudget:      100,
		Provider:       api.ProviderConfig{Name: "mock", APIKey: "sk-test"},
		Tools:          []tool.Tool{tool.NewWriteTool()},
	})
	if err != nil {
		t.Fatal(err)
	}
	out := &lockedBuffer{}
	termui.SetWriter(out)
	oldInteractive := replInteractive
	replInteractive = interactive
	t.Cleanup(func() { replInteractive = oldInteractive; repl.ClearPermInputCh(); termui.SetWriter(nil) })
	fe := &frontend{eng: eng, cfg: config.DefaultConfig(), print: func(s string) { termui.PrintAbove(s + "\n") }, enqueue: func(api.Message) {}}
	if interactive {
		fe.tasks = &replTaskRunner{}
	}
	fe.install(registerAllCommands())
	t.Cleanup(fe.closeWorkflows)
	return &changesEnv{fe: fe, out: out, project: project, configDir: configDir}
}

// run dispatches line and returns only the output it produced.
func (env *changesEnv) run(line string) string {
	before := len(env.out.String())
	env.fe.dispatch(line)
	return env.out.String()[before:]
}

// inboxResult makes one succeeded maintenance result that adds new.txt.
func (env *changesEnv) inboxResult(t *testing.T) string {
	t.Helper()
	store, err := automationStore(env.project)
	if err != nil {
		t.Fatal(err)
	}
	spec := automation.Spec{ID: "maint", Prompt: "add new.txt", Enabled: true, TimeoutSeconds: 60, BudgetUSD: 1, MaxTurns: 1}
	if err := store.Add(spec, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	result, err := (&automation.Executor{Store: store, Runner: changesRunner{}}).Run(context.Background(), spec.ID, "")
	if err != nil || result.State != "succeeded" || result.PatchHash == "" {
		t.Fatalf("inbox result: %+v %v", result, err)
	}
	return result.ID
}

// raceRun runs a race whose candidate a passes and b fails, and returns its ID.
func (env *changesEnv) raceRun(t *testing.T) string {
	t.Helper()
	entry, ok := env.fe.reg.Find("race")
	if !ok {
		t.Fatal("race not registered")
	}
	cmd := entry.(*RaceCommand)
	cmd.options.Runner = race.RunnerFunc(func(ctx context.Context, request race.Request) race.Execution {
		if err := os.WriteFile(filepath.Join(request.Directory, "value.txt"), []byte(request.Prompt+"\n"), 0600); err != nil {
			return race.Execution{Status: "start_error", Error: err.Error()}
		}
		return race.Execution{Status: "passed", ExitCode: 0}
	})
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	spec := race.Spec{Version: 1, Prompts: []string{"good", "bad"}, Verify: [][]string{{executable, "-test.run=^TestRaceCommandVerifierHelper$", "--", "race-command-verify"}}, TotalBudgetUSD: 2, CandidateBudgetUSD: 1, TotalTimeoutSeconds: 60, CandidateTimeoutSeconds: 50, VerifyTimeoutSeconds: 30}
	data, err := json.Marshal(spec)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "spec.json")
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	output, err := cmd.Execute(context.Background(), command.Input{Cwd: env.project, Args: []string{"run", path}})
	if err != nil || output.Data == "" {
		t.Fatalf("race run: %+v %v", output, err)
	}
	id := output.Data
	deadline := time.Now().Add(60 * time.Second)
	for {
		report, err := cmd.service.Load(id)
		if err != nil {
			t.Fatal(err)
		}
		if report.Status != "running" {
			if !report.Candidates[0].Correct || report.Candidates[1].Correct {
				t.Fatalf("race fixture: %+v", report.Candidates)
			}
			return id
		}
		if time.Now().After(deadline) {
			t.Fatal("race did not complete")
		}
		time.Sleep(25 * time.Millisecond)
	}
}

// turnWrite runs one model turn that writes name in the project.
func (env *changesEnv) turnWrite(t *testing.T, name string) {
	t.Helper()
	target := filepath.Join(env.project, name)
	args, err := json.Marshal(map[string]any{"filePath": target, "content": "turn edit\n"})
	if err != nil {
		t.Fatal(err)
	}
	model := newFakeModel(t,
		fakeStep{ToolCalls: []fakeToolCall{{Name: "write", Args: string(args)}}},
		fakeStep{Content: "完成"},
	)
	eng := env.fe.eng
	if err := eng.ReloadProvider("openai-compatible", "test-model", model.BaseURL(), "placeholder"); err != nil {
		t.Fatal(err)
	}
	eng.SetPermissionMode(permission.Bypass)
	eng.PermissionPrompt = func(string, map[string]any, string) bool { return true }
	if _, err := runChatInteraction(context.Background(), eng, "写一个文件"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(target); err != nil {
		t.Fatalf("turn did not write %s: %v", name, err)
	}
	if _, ok := eng.TurnChangeBase(); !ok {
		t.Fatal("turn recorded no change base")
	}
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func TestChangesListShowDiffAcrossSources(t *testing.T) {
	env := newChangesFixture(t, true)
	inbox := env.inboxResult(t)
	run := env.raceRun(t)

	list := env.run("/changes list")
	if !strings.Contains(list, "inbox:"+inbox) || !strings.Contains(list, "race:"+run+"/a") || !strings.Contains(list, "race:"+run+"/b") {
		t.Fatalf("list misses a source: %s", list)
	}
	if strings.Contains(list, "turn ") {
		t.Fatalf("turn listed before any edit: %s", list)
	}
	if only := env.run("/changes list inbox"); !strings.Contains(only, "inbox:"+inbox) || strings.Contains(only, "race:") {
		t.Fatalf("list inbox not filtered: %s", only)
	}

	show := env.run("/changes show inbox:" + inbox)
	for _, want := range []string{"可应用: 是", "无独立验证命令", "new.txt", "main@", "pending"} {
		if !strings.Contains(show, want) {
			t.Fatalf("show inbox lacks %q: %s", want, show)
		}
	}
	if strings.Contains(show, "+maintenance patch") {
		t.Fatalf("show printed the patch text: %s", show)
	}
	if b := env.run("/changes show race:" + run + "/b"); !strings.Contains(b, "可应用: 否") {
		t.Fatalf("failing candidate shown as applicable: %s", b)
	}
	if a := env.run("/changes show race:" + run + "/a"); !strings.Contains(a, "可应用: 是") || !strings.Contains(a, "value.txt") || strings.Contains(a, "无独立验证命令") {
		t.Fatalf("passing candidate: %s", a)
	}
	if diff := env.run("/changes diff inbox:" + inbox); !strings.Contains(diff, "+maintenance patch") {
		t.Fatalf("diff lacks patch text: %s", diff)
	}

	env.turnWrite(t, "turn.txt")
	if list := env.run("/changes list"); !strings.Contains(list, "turn") || !strings.Contains(list, "本轮改动") {
		t.Fatalf("turn not listed after an edit: %s", list)
	}
	if diff := env.run("/changes diff turn"); !strings.Contains(diff, "turn.txt") {
		t.Fatalf("turn diff lacks the written file: %s", diff)
	}
	if show := env.run("/changes show turn"); !strings.Contains(show, "可应用: 否") || !strings.Contains(show, "/undo") {
		t.Fatalf("turn show: %s", show)
	}
}

func TestChangesApplyAsksThenApplies(t *testing.T) {
	env := newChangesFixture(t, true)
	inbox := env.inboxResult(t)
	newFile := filepath.Join(env.project, "new.txt")

	if out := env.run("/changes apply inbox:" + inbox); !strings.Contains(out, "需要确认") {
		t.Fatalf("no confirmation box: %s", out)
	}
	if !env.fe.takePendingConfirm("n") || fileExists(newFile) {
		t.Fatal("n must not apply")
	}
	env.run("/changes apply inbox:" + inbox)
	before := len(env.out.String())
	if !env.fe.takePendingConfirm("y") {
		t.Fatal("y must resolve the confirmation")
	}
	if out := env.out.String()[before:]; !fileExists(newFile) || !strings.Contains(out, "已应用 inbox:"+inbox) {
		t.Fatalf("y must apply: %s", out)
	}
	if show := env.run("/inbox show " + inbox); !strings.Contains(show, "applied") {
		t.Fatalf("inbox did not record applied: %s", show)
	}
}

func TestChangesApplyRefusesAfterWorkspaceEdit(t *testing.T) {
	env := newChangesFixture(t, true)
	inbox := env.inboxResult(t)
	if out := env.run("/changes apply inbox:" + inbox); !strings.Contains(out, "需要确认") {
		t.Fatalf("no confirmation box: %s", out)
	}
	if err := os.WriteFile(filepath.Join(env.project, "stray.txt"), []byte("user edit\n"), 0600); err != nil {
		t.Fatal(err)
	}
	before := len(env.out.String())
	env.fe.takePendingConfirm("y")
	if out := env.out.String()[before:]; !strings.Contains(out, "clean") || fileExists(filepath.Join(env.project, "new.txt")) {
		t.Fatalf("apply over a dirty tree: %s", out)
	}
}

func TestChangesApplyHeadlessNeedsConfirm(t *testing.T) {
	env := newChangesFixture(t, false)
	inbox := env.inboxResult(t)
	newFile := filepath.Join(env.project, "new.txt")
	if out := env.run("/changes apply inbox:" + inbox); !strings.Contains(out, "confirm") || fileExists(newFile) {
		t.Fatalf("headless apply without confirm: %s", out)
	}
	if out := env.run("/changes apply inbox:" + inbox + " confirm"); !fileExists(newFile) || !strings.Contains(out, "已应用") {
		t.Fatalf("headless apply with confirm: %s", out)
	}
}

// TestChangesApplyHeadlessExitStatus pins what `cove -p "/changes apply …"`
// reports: a refusal fails the command, the no-confirm preview does not.
func TestChangesApplyHeadlessExitStatus(t *testing.T) {
	env := newChangesFixture(t, false)
	inbox := env.inboxResult(t)
	if out := env.run("/changes apply inbox:" + inbox); env.fe.commandFailed {
		t.Fatalf("preview without confirm marked failed: %s", out)
	}
	if out := env.run("/changes apply turn confirm"); !env.fe.commandFailed {
		t.Fatalf("turn apply refusal not marked failed: %s", out)
	}
	if out := env.run("/changes review inbox:" + inbox + " rejected"); env.fe.commandFailed {
		t.Fatalf("review: %s", out)
	}
	if out := env.run("/changes apply inbox:" + inbox + " confirm"); !env.fe.commandFailed || !strings.Contains(out, "不能应用") || fileExists(filepath.Join(env.project, "new.txt")) {
		t.Fatalf("not-applicable apply not marked failed: %s", out)
	}
}

func TestChangesApplyTurnRefused(t *testing.T) {
	env := newChangesFixture(t, false)
	if out := env.run("/changes apply turn confirm"); !strings.Contains(out, "/undo") || strings.Contains(out, "已应用") {
		t.Fatalf("turn apply: %s", out)
	}
	if out := env.run("/changes review turn accepted"); strings.Contains(out, "已记录") {
		t.Fatalf("turn review accepted: %s", out)
	}
}

func TestChangesExportBoundary(t *testing.T) {
	env := newChangesFixture(t, false)
	inbox := env.inboxResult(t)
	if out := env.run("/changes export inbox:" + inbox + " " + filepath.Join(env.project, "x.patch")); !strings.Contains(out, "之外") || fileExists(filepath.Join(env.project, "x.patch")) {
		t.Fatalf("export into project: %s", out)
	}
	if out := env.run("/changes export inbox:" + inbox + " " + filepath.Join(env.configDir, "x.patch")); !strings.Contains(out, "之外") || fileExists(filepath.Join(env.configDir, "x.patch")) {
		t.Fatalf("export into config dir: %s", out)
	}
	target := filepath.Join(t.TempDir(), "x.patch")
	if out := env.run("/changes export inbox:" + inbox + " " + target); !strings.Contains(out, target) {
		t.Fatalf("export outside: %s", out)
	}
	store, err := automationStore(env.project)
	if err != nil {
		t.Fatal(err)
	}
	want, err := store.ReadPatch(inbox)
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(target)
	if err != nil || string(got) != string(want) {
		t.Fatalf("exported bytes differ: %v", err)
	}
}

func TestRaceSelectAndChangesApplyShareLock(t *testing.T) {
	env := newChangesFixture(t, false)
	inbox := env.inboxResult(t)
	entry, _ := env.fe.reg.Find("race")
	cmd := entry.(*RaceCommand)
	err := changeset.WithProjectLock(env.configDir, env.project, func() error {
		_, err := cmd.Execute(context.Background(), command.Input{Cwd: env.project, Args: []string{"select", "run-id", "a"}})
		if !errors.Is(err, changeset.ErrLocked) || !strings.Contains(err.Error(), "另一个 apply") {
			t.Errorf("race select under the lock: %v", err)
		}
		if out := env.run("/race select run-id a"); !strings.Contains(out, "另一个 apply") {
			t.Errorf("/race select under the lock: %s", out)
		}
		if out := env.run("/changes apply inbox:" + inbox + " confirm"); !strings.Contains(out, "另一个 apply") {
			t.Errorf("/changes apply under the lock: %s", out)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if fileExists(filepath.Join(env.project, "new.txt")) {
		t.Fatal("applied under another holder's lock")
	}
}

func TestLegacyCommandsDelegate(t *testing.T) {
	env := newChangesFixture(t, false)
	inbox := env.inboxResult(t)
	if show := env.run("/inbox show " + inbox); !strings.Contains(show, "可应用: 是") {
		t.Fatalf("inbox show lacks the applicable line: %s", show)
	}
	env.run("/inbox review " + inbox + " rejected")
	if show := env.run("/changes show inbox:" + inbox); !strings.Contains(show, "rejected") || !strings.Contains(show, "可应用: 否") {
		t.Fatalf("review not visible through /changes: %s", show)
	}
	if show := env.run("/inbox show " + inbox); !strings.Contains(show, "可应用: 否（") {
		t.Fatalf("inbox show after reject: %s", show)
	}
	dir := t.TempDir()
	legacy, unified := filepath.Join(dir, "legacy.patch"), filepath.Join(dir, "unified.patch")
	env.run("/inbox patch " + inbox + " " + legacy)
	env.run("/changes export inbox:" + inbox + " " + unified)
	a, errA := os.ReadFile(legacy)
	b, errB := os.ReadFile(unified)
	if errA != nil || errB != nil || len(a) == 0 || string(a) != string(b) {
		t.Fatalf("/inbox patch and /changes export differ: %v %v", errA, errB)
	}

	run := env.raceRun(t)
	out := env.run("/race select " + run + " a")
	var report race.Report
	body := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(out), "[race]"))
	if err := json.Unmarshal([]byte(body), &report); err != nil || report.Selected != "a" {
		t.Fatalf("/race select output changed: %s (%v)", out, err)
	}
	if data, _ := os.ReadFile(filepath.Join(env.project, "value.txt")); string(data) != "good\n" {
		t.Fatalf("race select did not apply: %q", data)
	}
	if show := env.run("/changes show race:" + run + "/a"); !strings.Contains(show, "applied") {
		t.Fatalf("selection not visible through /changes: %s", show)
	}
}
