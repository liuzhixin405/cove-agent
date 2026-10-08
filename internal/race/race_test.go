package race

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/liuzhixin405/cove-agent/internal/workspace"
)

func fixture(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	git := func(args ...string) {
		t.Helper()
		if _, err := workspace.Git(context.Background(), root, nil, args...); err != nil {
			t.Fatal(err)
		}
	}
	git("init", "-b", "main")
	git("config", "core.autocrlf", "false")
	if err := os.WriteFile(filepath.Join(root, "value.txt"), []byte("base\n"), 0600); err != nil {
		t.Fatal(err)
	}
	git("add", ".")
	git("-c", "user.name=fixture", "-c", "user.email=fixture@example.invalid", "commit", "-m", "fixture")
	return root
}

func verifier(t *testing.T, mode string) []string {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return []string{executable, "-test.run=^TestRaceVerifierHelper$", "--", mode}
}

func TestRaceVerifierHelper(t *testing.T) {
	if len(os.Args) < 2 {
		return
	}
	mode := os.Args[len(os.Args)-1]
	if mode != "race-verify" && mode != "race-timeout" {
		return
	}
	if mode == "race-timeout" {
		<-time.NewTimer(24 * time.Hour).C
	}
	data, err := os.ReadFile("value.txt")
	if err != nil || string(data) != "good\n" {
		os.Exit(7)
	}
	os.Exit(0)
}

func testSpec(t *testing.T) Spec {
	return Spec{Version: Version, Prompts: []string{"good", "bad"}, Verify: [][]string{verifier(t, "race-verify")}, TotalBudgetUSD: 3, CandidateBudgetUSD: 2, TotalTimeoutSeconds: 45, CandidateTimeoutSeconds: 30, VerifyTimeoutSeconds: 5}
}

func TestRunVerifySelect(t *testing.T) {
	project := fixture(t)
	entered := make(chan string, 2)
	release := make(chan struct{})
	selected := 0
	svc := &Service{Directory: t.TempDir(), Runner: RunnerFunc(func(ctx context.Context, request Request) Execution {
		entered <- request.Directory
		select {
		case <-release:
		case <-ctx.Done():
			return Execution{Status: contextStatus(ctx)}
		}
		if request.BudgetUSD != 1.5 {
			return Execution{Status: "start_error", Error: "wrong allocation"}
		}
		if err := os.WriteFile(filepath.Join(request.Directory, "value.txt"), []byte(request.Prompt+"\n"), 0600); err != nil {
			return Execution{Status: "start_error", Error: err.Error()}
		}
		return Execution{Status: "passed", ExitCode: 0}
	}), OnSelect: func() {
		data, err := os.ReadFile(filepath.Join(project, "value.txt"))
		if err != nil || string(data) != "good\n" {
			t.Errorf("selection hook preceded apply: %q %v", data, err)
		}
		selected++
	}}
	defer svc.Close()
	type response struct {
		report *Report
		err    error
	}
	done := make(chan response, 1)
	go func() {
		report, err := svc.Run(context.Background(), project, testSpec(t))
		done <- response{report, err}
	}()
	first, second := <-entered, <-entered
	if first == second || first == project || second == project {
		t.Fatal("not separate worktrees")
	}
	close(release)
	result := <-done
	if result.err != nil {
		t.Fatal(result.err)
	}
	report := result.report
	if !report.Candidates[0].Correct || report.Candidates[1].Correct {
		t.Fatalf("wrong verifier outcome: %+v", report.Candidates)
	}
	if report.Candidates[1].Verification[0].Execution.ExitCode != 7 || report.Candidates[1].Verification[0].Execution.Status != "nonzero" {
		t.Fatal("nonzero not distinguished")
	}
	if report.Candidates[0].Generation.CostUSD != nil || report.Candidates[0].Generation.CostSource != "unverified" {
		t.Fatal("fabricated model cost")
	}
	for _, candidate := range report.Candidates {
		if len(candidate.Verification) != 1 || !slices.Equal(candidate.Verification[0].Argv, report.Spec.Verify[0]) {
			t.Fatal("candidates did not execute the same verifier argv")
		}
	}
	encoded, err := json.Marshal(report)
	if err != nil || !strings.Contains(string(encoded), `"cost_usd":null`) {
		t.Fatalf("unverified cost was not JSON null: %s %v", encoded, err)
	}
	for _, path := range []string{first, second} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("worktree leaked: %s", path)
		}
	}
	data, _ := os.ReadFile(filepath.Join(project, "value.txt"))
	if string(data) != "base\n" {
		t.Fatal("silently applied candidate")
	}
	if _, err := svc.Select(context.Background(), project, report.ID, "b"); err == nil {
		t.Fatal("selected failed candidate")
	}
	if _, err := svc.Select(context.Background(), project, report.ID, "../a"); err == nil {
		t.Fatal("accepted malicious candidate")
	}
	if _, err := svc.Load("../escape"); err == nil {
		t.Fatal("accepted malicious run ID")
	}
	if err := os.WriteFile(filepath.Join(project, "value.txt"), []byte("user\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Select(context.Background(), project, report.ID, "a"); err == nil {
		t.Fatal("accepted drift")
	}
	data, _ = os.ReadFile(filepath.Join(project, "value.txt"))
	if string(data) != "user\n" {
		t.Fatal("overwrote user")
	}
	if selected != 0 {
		t.Fatal("failed selection triggered hook")
	}
	if err := os.WriteFile(filepath.Join(project, "value.txt"), []byte("base\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Select(context.Background(), project, report.ID, "a"); err != nil {
		t.Fatalf("select: %v; candidates: %+v", err, report.Candidates)
	}
	data, _ = os.ReadFile(filepath.Join(project, "value.txt"))
	if string(data) != "good\n" {
		t.Fatal("selection did not apply exact candidate")
	}
	if selected != 1 {
		t.Fatalf("successful selection hook count: %d", selected)
	}
	if _, err := svc.Select(context.Background(), project, report.ID, "a"); err == nil || selected != 1 {
		t.Fatal("repeated selection applied or triggered hook")
	}
}

func TestVerifierTimeout(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 500000000)
	defer cancel()
	result := Execute(ctx, t.TempDir(), nil, verifier(t, "race-timeout"))
	if result.Status != "timeout" {
		t.Fatalf("timeout became %s: %s", result.Status, result.Error)
	}
}

func TestRaceCancelAndClose(t *testing.T) {
	for _, cancelRun := range []bool{true, false} {
		t.Run(map[bool]string{true: "cancel", false: "close"}[cancelRun], func(t *testing.T) {
			project := fixture(t)
			entered := make(chan struct{}, 2)
			svc := &Service{Directory: t.TempDir(), Runner: RunnerFunc(func(ctx context.Context, request Request) Execution {
				entered <- struct{}{}
				<-ctx.Done()
				return Execution{Status: contextStatus(ctx), ExitCode: -1}
			})}
			id, err := svc.Start(context.Background(), project, testSpec(t))
			if err != nil {
				t.Fatal(err)
			}
			<-entered
			<-entered
			if cancelRun {
				if err := svc.Cancel(id); err != nil {
					t.Fatal(err)
				}
			}
			if err := svc.Close(); err != nil {
				t.Fatal(err)
			}
			report, err := svc.Load(id)
			if err != nil {
				t.Fatal(err)
			}
			if report.Status != "cancelled" || report.Candidates[0].Generation.Status != "cancelled" || report.Candidates[1].Generation.Status != "cancelled" {
				t.Fatalf("wrong cancellation report: %+v", report)
			}
			if _, err := svc.Start(context.Background(), project, testSpec(t)); err == nil {
				t.Fatal("started after Close")
			}
			if _, err := svc.Select(context.Background(), project, id, "a"); err == nil || !strings.Contains(err.Error(), "closed") {
				t.Fatalf("selection not rejected by Close: %v", err)
			}
			if err := svc.Cancel(id); err == nil {
				t.Fatal("completed cancellation retained active run")
			}
			if err := svc.Close(); err != nil {
				t.Fatal("Close was not idempotent:", err)
			}
			for _, candidate := range report.Candidates {
				if candidate.CleanupError != "" {
					t.Fatalf("cancelled worktree cleanup failed: %+v", candidate)
				}
			}
			worktrees, err := workspace.Git(context.Background(), project, nil, "worktree", "list", "--porcelain")
			if err != nil || strings.Count(string(worktrees), "worktree ") != 1 {
				t.Fatalf("leaked worktree registration: %s %v", worktrees, err)
			}
		})
	}
}

func TestRaceTotalAndCandidateTimeout(t *testing.T) {
	project := fixture(t)
	for _, total := range []bool{false, true} {
		spec := testSpec(t)
		if total {
			spec.TotalTimeoutSeconds = 1
		} else {
			spec.CandidateTimeoutSeconds = 1
		}
		svc := &Service{Directory: t.TempDir(), Runner: RunnerFunc(func(ctx context.Context, request Request) Execution {
			<-ctx.Done()
			return Execution{Status: contextStatus(ctx), ExitCode: -1}
		})}
		report, err := svc.Run(context.Background(), project, spec)
		if err != nil && total {
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("total limit failed for another reason: %v", err)
			}
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		if total && report.Status != "timeout" {
			t.Fatalf("total timeout: %s", report.Status)
		}
		for _, candidate := range report.Candidates {
			if candidate.Correct {
				t.Fatal("timeout accepted as correct")
			}
			if candidate.Generation.Status != "timeout" {
				t.Fatalf("candidate timeout not distinguished: %+v", candidate)
			}
		}
		if report.DurationMS > 10000 {
			t.Fatal("timeout was not bounded")
		}
	}
}

func TestRaceRefusesReportsInsideProject(t *testing.T) {
	project := fixture(t)
	svc := &Service{Directory: filepath.Join(project, "not-created", "race"), Runner: RunnerFunc(func(context.Context, Request) Execution {
		t.Error("runner started with unsafe report directory")
		return Execution{Status: "passed"}
	})}
	if _, err := svc.Run(context.Background(), project, testSpec(t)); err == nil {
		t.Fatal("reports accepted in source")
	}
	if _, err := os.Stat(filepath.Join(project, "not-created")); !os.IsNotExist(err) {
		t.Fatal("unsafe report directory was created")
	}
}

func TestRacePatchTamperAndBranchDrift(t *testing.T) {
	project := fixture(t)
	svc := &Service{Directory: t.TempDir(), Runner: RunnerFunc(func(ctx context.Context, request Request) Execution {
		if err := os.WriteFile(filepath.Join(request.Directory, "value.txt"), []byte("good\n"), 0600); err != nil {
			return Execution{Status: "start_error", Error: err.Error()}
		}
		return Execution{Status: "passed", ExitCode: 0}
	})}
	report, err := svc.Run(context.Background(), project, testSpec(t))
	if err != nil {
		t.Fatal(err)
	}
	patchPath := filepath.Join(svc.Directory, report.ID, "a.patch")
	patch, err := os.ReadFile(patchPath)
	if err != nil {
		t.Fatal(err)
	}
	tampered := append([]byte(nil), patch...)
	tampered[len(tampered)-2] ^= 1
	if err := os.WriteFile(patchPath, tampered, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Select(context.Background(), project, report.ID, "a"); err == nil || !strings.Contains(err.Error(), "hash") {
		t.Fatalf("accepted tampered patch: %v", err)
	}
	if err := os.WriteFile(patchPath, patch, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := workspace.Git(context.Background(), project, nil, "symbolic-ref", "HEAD", "refs/heads/other"); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Select(context.Background(), project, report.ID, "a"); err == nil {
		t.Fatal("accepted branch drift")
	}
	data, _ := os.ReadFile(filepath.Join(project, "value.txt"))
	if string(data) != "base\n" {
		t.Fatal("tamper/drift changed original")
	}
	if _, err := workspace.Git(context.Background(), project, nil, "symbolic-ref", "HEAD", "refs/heads/main"); err != nil {
		t.Fatal(err)
	}
	otherProject := filepath.Join(t.TempDir(), "clone")
	if _, err := workspace.Git(context.Background(), project, nil, "clone", "--", project, otherProject); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Select(context.Background(), otherProject, report.ID, "a"); err == nil || !strings.Contains(err.Error(), "scope") {
		t.Fatalf("accepted another project with same commit/branch: %v", err)
	}
	originalArgv := report.Candidates[0].Verification[0].Argv
	report.Candidates[0].Verification[0].Argv = []string{"different-verifier"}
	if err := svc.save(report); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Select(context.Background(), project, report.ID, "a"); err == nil || !strings.Contains(err.Error(), "mismatched") {
		t.Fatalf("accepted mismatched verifier argv: %v", err)
	}
	report.Candidates[0].Verification[0].Argv = originalArgv
	if err := svc.save(report); err != nil {
		t.Fatal(err)
	}
	if _, err := workspace.Git(context.Background(), project, nil, "-c", "user.name=fixture", "-c", "user.email=fixture@example.invalid", "commit", "--allow-empty", "-m", "drift"); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Select(context.Background(), project, report.ID, "a"); err == nil || !strings.Contains(err.Error(), "commit drift") {
		t.Fatalf("accepted base commit drift: %v", err)
	}
}

func TestRaceSelectionHookBeforeAuditFailure(t *testing.T) {
	project := fixture(t)
	selected := 0
	svc := &Service{Directory: t.TempDir(), Runner: RunnerFunc(func(ctx context.Context, request Request) Execution {
		if err := os.WriteFile(filepath.Join(request.Directory, "value.txt"), []byte("good\n"), 0600); err != nil {
			return Execution{Status: "start_error", Error: err.Error()}
		}
		return Execution{Status: "passed", ExitCode: 0}
	})}
	defer svc.Close()
	report, err := svc.Run(context.Background(), project, testSpec(t))
	if err != nil {
		t.Fatal(err)
	}
	svc.OnSelect = func() {
		selected++
		path := filepath.Join(svc.Directory, report.ID, "report.json")
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(path, 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(path, "block"), []byte("audit write must fail"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	result, err := svc.Select(context.Background(), project, report.ID, "a")
	if err == nil || !strings.Contains(err.Error(), "patch applied") || selected != 1 || result == nil || result.Selected != "a" {
		t.Fatalf("applied patch lost hook/audit failure: %+v %v; hook=%d", result, err, selected)
	}
	data, err := os.ReadFile(filepath.Join(project, "value.txt"))
	if err != nil || string(data) != "good\n" {
		t.Fatalf("audit failure test did not apply: %q %v", data, err)
	}
	if _, err := os.Stat(filepath.Join(svc.Directory, "selection.lock")); !os.IsNotExist(err) {
		t.Fatal("selection lock leaked")
	}
}

func TestRaceCLIExecutableHelper(t *testing.T) {
	index := -1
	for pos, arg := range os.Args {
		if arg == "race-cli-helper" {
			index = pos
			break
		}
	}
	if index < 0 {
		return
	}
	args := os.Args[index+1:]
	want := []string{"-p", "genuine subprocess prompt", "--no-tui", "--profile", "race-budget", "--max-turns", "12"}
	if strings.Join(args, "|") != strings.Join(want, "|") {
		os.Exit(9)
	}
	dir := os.Getenv("COVE_CONFIG_DIR")
	data, err := os.ReadFile(filepath.Join(dir, "config.json"))
	if err != nil {
		os.Exit(10)
	}
	var config map[string]any
	if json.Unmarshal(data, &config) != nil || config["max_budget_usd"] != 1.25 {
		os.Exit(11)
	}
	profile := config["profiles"].(map[string]any)["race-budget"].(map[string]any)
	if profile["max_budget_usd"] != 1.25 || profile["permission_mode"] != "auto" {
		os.Exit(12)
	}
	if _, err := os.Stat(".git"); err != nil {
		os.Exit(13)
	}
	if err := os.WriteFile("config-location.txt", []byte(dir), 0600); err != nil {
		os.Exit(14)
	}
	os.Exit(0)
}

func TestCLIRunnerActualSubprocessConfigAndCleanup(t *testing.T) {
	project := fixture(t)
	configDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(configDir, "config.json"), []byte(`{"max_budget_usd":999,"profiles":{"existing":{"max_budget_usd":999}},"provider":{"name":"fixture","api_key":"private-key"}}`), 0600); err != nil {
		t.Fatal(err)
	}
	runner := CLIRunner{Executable: os.Executable, PrefixArgs: []string{"-test.run=^TestRaceCLIExecutableHelper$", "--", "race-cli-helper"}, ConfigDirectory: configDir}
	result := runner.Run(context.Background(), Request{Directory: project, Prompt: "genuine subprocess prompt", BudgetUSD: 1.25})
	if result.Status != "passed" {
		t.Fatalf("runner did not execute expected CLI args/config: %+v", result)
	}
	location, err := os.ReadFile(filepath.Join(project, "config-location.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(string(location)); !os.IsNotExist(err) {
		t.Fatal("private child config leaked")
	}
	if result.CostUSD != nil || result.CostSource != "unverified" {
		t.Fatal("fabricated CLI cost")
	}
}

func TestRaceStrictSpec(t *testing.T) {
	spec := testSpec(t)
	spec.Prompts = append(spec.Prompts, "third")
	if err := spec.Validate(); err == nil {
		t.Fatal("accepted three prompts")
	}
	path := filepath.Join(t.TempDir(), "spec.json")
	if err := os.WriteFile(path, []byte(`{"version":1,"unknown":true}`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadSpec(path); err == nil {
		t.Fatal("accepted unknown spec key")
	}
}
