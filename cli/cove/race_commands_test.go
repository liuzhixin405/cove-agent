package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/liuzhixin405/cove-agent/internal/command"
	"github.com/liuzhixin405/cove-agent/internal/engine"
	"github.com/liuzhixin405/cove-agent/internal/race"
	"github.com/liuzhixin405/cove-agent/internal/session"
	"github.com/liuzhixin405/cove-agent/internal/workspace"
)

func TestRaceCommandsLazyAndArguments(t *testing.T) {
	var frontend *frontend
	commands := frontend.raceCommands()
	if len(commands) != 1 || commands[0].Name() != "race" {
		t.Fatal("missing lazy race command")
	}
	cmd := commands[0].(*RaceCommand)
	defer cmd.Close()
	output, err := cmd.Execute(context.Background(), command.Input{})
	if err != nil || !strings.Contains(output.Message, "/race select <runID> <a|b>") {
		t.Fatalf("missing operator help: %v", err)
	}
	if cmd.service != nil {
		t.Fatal("eager initialization for nil frontend")
	}
	for _, args := range [][]string{{"unknown"}, {"list", "extra"}, {"run"}, {"select", "id"}, {"cancel", "id", "extra"}} {
		if _, err := cmd.Execute(context.Background(), command.Input{Args: args}); err == nil {
			t.Errorf("accepted unknown arguments: %v", args)
		}
	}
	if !cmd.MutatesEngine([]string{"select"}) || cmd.MutatesEngine([]string{"show"}) {
		t.Fatal("incorrect mutation declaration")
	}
}

func TestRaceCommandListAndMaliciousTokens(t *testing.T) {
	t.Setenv("COVE_CONFIG_DIR", t.TempDir())
	cmd := NewRaceCommand(RaceCommandOptions{})
	defer cmd.Close()
	output, err := cmd.Execute(context.Background(), command.Input{Args: []string{"list"}, Cwd: t.TempDir()})
	if err != nil || output.Message != "[]" {
		t.Fatalf("empty report list: %q %v", output.Message, err)
	}
	for _, args := range [][]string{{"show", "../escape"}, {"select", "id", "../a"}, {"cancel", "unknown"}} {
		if _, err := cmd.Execute(context.Background(), command.Input{Args: args}); err == nil {
			t.Errorf("accepted token: %v", args)
		}
	}
}

func TestRaceCommandVerifierHelper(t *testing.T) {
	if os.Args[len(os.Args)-1] != "race-command-verify" {
		return
	}
	data, err := os.ReadFile("value.txt")
	if err != nil || string(data) != "good\n" {
		os.Exit(7)
	}
	os.Exit(0)
}

func TestRaceCommandOperatorPath(t *testing.T) {
	configDirectory := t.TempDir()
	t.Setenv("COVE_CONFIG_DIR", configDirectory)
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
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	spec := race.Spec{Version: 1, Prompts: []string{"good", "bad"}, Verify: [][]string{{executable, "-test.run=^TestRaceCommandVerifierHelper$", "--", "race-command-verify"}}, TotalBudgetUSD: 2, CandidateBudgetUSD: 1, TotalTimeoutSeconds: 30, CandidateTimeoutSeconds: 20, VerifyTimeoutSeconds: 5}
	data, err := json.Marshal(spec)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "spec.json")
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	t.Chdir(project)
	eng := &engine.Engine{}
	previous := engine.AcceptanceReport{Version: 1, ID: "previous-acceptance", SessionID: eng.SessionID(), Cwd: session.NormalizeProjectDir(project), Request: "previous task", Outcome: "finished", Checks: []engine.AcceptanceCheck{{Criterion: "old test", Command: "old test", Status: "passed", Result: &engine.VerifyResult{Command: "old test", Passed: true}}}}
	sum := sha256.Sum256([]byte(previous.Cwd + "\x00" + previous.SessionID))
	acceptancePath := filepath.Join(configDirectory, "acceptance", fmt.Sprintf("%x.json", sum))
	if err := os.MkdirAll(filepath.Dir(acceptancePath), 0700); err != nil {
		t.Fatal(err)
	}
	previousData, err := json.Marshal(previous)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(acceptancePath, previousData, 0600); err != nil {
		t.Fatal(err)
	}
	fe := &frontend{eng: eng}
	cmd := fe.raceCommands()[0].(*RaceCommand)
	cmd.options.Runner = race.RunnerFunc(func(ctx context.Context, request race.Request) race.Execution {
		if err := os.WriteFile(filepath.Join(request.Directory, "value.txt"), []byte(request.Prompt+"\n"), 0600); err != nil {
			return race.Execution{Status: "start_error", Error: err.Error()}
		}
		return race.Execution{Status: "passed", ExitCode: 0}
	})
	defer cmd.Close()
	assertAcceptance := func(status string) {
		t.Helper()
		report, err := eng.LastAcceptance()
		if err != nil || report == nil || report.ID != previous.ID || report.Request != previous.Request || len(report.Checks) != 1 || report.Checks[0].Status != status {
			t.Fatalf("selection hook acceptance: %+v, %v; want previous report status %s", report, err, status)
		}
		if status == "unverified" && report.Checks[0].Result != nil {
			t.Fatal("selection retained old executable evidence")
		}
	}
	assertAcceptance("passed")
	invoke := func(args ...string) (command.Output, error) {
		return cmd.Execute(context.Background(), command.Input{Cwd: project, Args: args})
	}
	output, err := invoke("run", path)
	if err != nil || output.Data == "" {
		t.Fatalf("run did not return ID: %+v %v", output, err)
	}
	id := output.Data
	ticker := time.NewTicker(25 * time.Millisecond)
	defer ticker.Stop()
	deadline := time.NewTimer(25 * time.Second)
	defer deadline.Stop()
	for {
		report, err := cmd.service.Load(id)
		if err != nil {
			t.Fatal(err)
		}
		if report.Status != "running" {
			break
		}
		select {
		case <-ticker.C:
		case <-deadline.C:
			t.Fatal("race command run did not complete")
		}
	}
	data, _ = os.ReadFile(filepath.Join(project, "value.txt"))
	if string(data) != "base\n" {
		t.Fatal("CLI silently applied candidate")
	}
	output, err = invoke("show", id)
	var report race.Report
	if err != nil || json.Unmarshal([]byte(output.Message), &report) != nil || report.ID != id || !report.Candidates[0].Correct || report.Candidates[1].Correct {
		t.Fatalf("show lost comparison evidence: %v %s", err, output.Message)
	}
	output, err = invoke("list")
	var reports []race.Report
	if err != nil || json.Unmarshal([]byte(output.Message), &reports) != nil || len(reports) != 1 || reports[0].ID != id {
		t.Fatalf("list lost run: %v %s", err, output.Message)
	}
	if _, err := invoke("select", id, "b"); err == nil {
		t.Fatal("CLI selected failing candidate")
	}
	assertAcceptance("passed")
	if _, err := invoke("select", id, "a"); err != nil {
		t.Fatal(err)
	}
	data, _ = os.ReadFile(filepath.Join(project, "value.txt"))
	if string(data) != "good\n" {
		t.Fatal("CLI did not apply selected patch")
	}
	assertAcceptance("unverified")
	persisted, err := os.ReadFile(acceptancePath)
	var saved engine.AcceptanceReport
	if err != nil || json.Unmarshal(persisted, &saved) != nil || saved.ID != previous.ID || saved.Checks[0].Status != "unverified" || saved.Checks[0].Result != nil {
		t.Fatalf("selection did not invalidate persisted acceptance: %s %v", persisted, err)
	}
}

func TestRaceCommandRefusesNoGit(t *testing.T) {
	t.Setenv("COVE_CONFIG_DIR", t.TempDir())
	path := filepath.Join(t.TempDir(), "spec.json")
	data := []byte(`{"version":1,"prompts":["a","b"],"verify":[["go","version"]],"total_budget_usd":2,"candidate_budget_usd":1,"total_timeout_seconds":30,"candidate_timeout_seconds":20,"verify_timeout_seconds":5}`)
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	cmd := NewRaceCommand(RaceCommandOptions{})
	defer cmd.Close()
	_, err := cmd.Execute(context.Background(), command.Input{Args: []string{"run", path}, Cwd: t.TempDir()})
	if err == nil || !strings.Contains(err.Error(), ".git") {
		t.Fatalf("faked isolation without .git: %v", err)
	}
}
