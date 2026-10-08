package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/liuzhixin405/cove-agent/internal/browser"
	"github.com/liuzhixin405/cove-agent/internal/command"
	"github.com/liuzhixin405/cove-agent/internal/engine"
)

func TestBrowserVerificationCommandsNilConstruction(t *testing.T) {
	var frontend *frontend
	commands := frontend.browserVerificationCommands()
	if len(commands) != 1 || commands[0].Name() != "browser-verify" {
		t.Fatal("nil construction failed")
	}
	output, err := commands[0].Execute(context.Background(), command.Input{})
	if err != nil || output.Message == "" {
		t.Fatalf("help: %+v %v", output, err)
	}
}

func TestBrowserVerificationCommandReportsAndHook(t *testing.T) {
	directory := t.TempDir()
	workflowPath := filepath.Join(directory, "workflow.json")
	if err := os.WriteFile(workflowPath, []byte(`{"url":"https://93.184.216.34","steps":[{"action":"assert_visible","selector":"body"}]}`), 0600); err != nil {
		t.Fatal(err)
	}
	called := 0
	var recorded browser.Report
	cmd := NewBrowserVerificationCommand(BrowserVerificationCommandOptions{EvidenceDir: filepath.Join(directory, "evidence"), ChromePath: filepath.Join(directory, "missing-chrome"), OnReport: func(ctx context.Context, report browser.Report) error { called++; recorded = report; return nil }})
	output, err := cmd.Execute(context.Background(), command.Input{Cwd: directory, Args: []string{"workflow.json"}})
	if err != nil {
		t.Fatal(err)
	}
	var report browser.Report
	if err := json.Unmarshal([]byte(output.Data), &report); err != nil {
		t.Fatal(err)
	}
	if report.Status != browser.StatusUnverified || report.Reason != "chrome_unavailable" || called != 1 || recorded.EvidenceDir != report.EvidenceDir {
		t.Fatalf("report/hook: %+v called=%d", report, called)
	}
	output, err = cmd.Execute(context.Background(), command.Input{Args: []string{"artifacts", filepath.Base(report.EvidenceDir)}})
	if err != nil || output.Data == "" {
		t.Fatalf("artifacts: %+v %v", output, err)
	}
	output, err = cmd.Execute(context.Background(), command.Input{Args: []string{"results"}})
	if err != nil || output.Data == "" {
		t.Fatalf("results: %+v %v", output, err)
	}
	for _, args := range [][]string{{"artifacts", "../escape"}, {"workflow.json", "--unsafe"}, {"results", "extra"}} {
		if _, err := cmd.Execute(context.Background(), command.Input{Args: args}); err == nil {
			t.Fatalf("invalid args accepted: %v", args)
		}
	}
	if _, err := cmd.Execute(context.Background(), command.Input{Args: []string{directory}}); err == nil {
		t.Fatal("directory accepted as workflow file")
	}
}

func TestBrowserVerificationCommandRealEngineRecordReload(t *testing.T) {
	directory := t.TempDir()
	t.Setenv("COVE_CONFIG_DIR", filepath.Join(directory, "config"))
	t.Chdir(directory)
	fixture := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		_, _ = writer.Write([]byte("<html><body>Fixture</body></html>"))
	}))
	defer fixture.Close()
	workflow, err := json.Marshal(browser.Workflow{URL: fixture.URL, Steps: []browser.WorkflowStep{{Action: "assert_visible", Selector: "body"}}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "workflow.json"), workflow, 0600); err != nil {
		t.Fatal(err)
	}
	eng, err := engine.New(engine.Config{Model: "test-model"})
	if err != nil {
		t.Fatal(err)
	}
	cmd := &BrowserVerificationCommand{frontend: &frontend{eng: eng}, options: BrowserVerificationCommandOptions{ChromePath: filepath.Join(directory, "missing-chrome")}}
	output, err := cmd.Execute(context.Background(), command.Input{Cwd: directory, Args: []string{"workflow.json", "--allow-local"}})
	if err != nil {
		t.Fatal(err)
	}
	var evidence browser.Report
	if err := json.Unmarshal([]byte(output.Data), &evidence); err != nil {
		t.Fatal(err)
	}
	report, err := eng.LastAcceptance()
	if err != nil || report == nil || len(report.Browser) != 1 || report.Browser[0].Status != browser.StatusUnverified || report.Browser[0].Reason != "chrome_unavailable" || report.Browser[0].EvidenceDir != evidence.EvidenceDir || report.Outcome != "finished" || report.FinishedAt.IsZero() {
		t.Fatalf("real CLI engine record: %+v %v", report, err)
	}
	entries, err := os.ReadDir(filepath.Join(directory, "config", "acceptance"))
	if err != nil || len(entries) != 1 {
		t.Fatalf("acceptance files: %v %v", entries, err)
	}
	data, err := os.ReadFile(filepath.Join(directory, "config", "acceptance", entries[0].Name()))
	if err != nil {
		t.Fatal(err)
	}
	var persisted engine.AcceptanceReport
	if err := json.Unmarshal(data, &persisted); err != nil {
		t.Fatal(err)
	}
	if persisted.ID != report.ID || persisted.SessionID != eng.SessionID() || persisted.Cwd != report.Cwd || persisted.Browser[0].Status != browser.StatusUnverified || persisted.Browser[0].Reason != "chrome_unavailable" {
		t.Fatalf("CLI disk reload: %+v", persisted)
	}
	report.Browser[0].Status = browser.StatusPass
	again, err := eng.LastAcceptance()
	if err != nil || again.Browser[0].Status != browser.StatusUnverified {
		t.Fatalf("CLI snapshot alias: %+v %v", again, err)
	}
	if err := os.Remove(filepath.Join(directory, "config", "acceptance", entries[0].Name())); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(directory, "config", "acceptance", entries[0].Name()), 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := cmd.Execute(context.Background(), command.Input{Cwd: directory, Args: []string{"workflow.json", "--allow-local"}}); err == nil {
		t.Fatal("CLI swallowed real engine persistence failure")
	}
}

func TestBrowserVerificationCommandChromeEngineEvidence(t *testing.T) {
	directory := t.TempDir()
	t.Setenv("COVE_CONFIG_DIR", filepath.Join(directory, "config"))
	t.Chdir(directory)
	fixture := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = writer.Write([]byte(`<html><head><meta name="viewport" content="width=device-width,initial-scale=1"></head><body style="margin:0;background:#fff"><header style="background:#18745c;color:white;padding:24px">Browser CLI fixture</header><main style="padding:24px"><input id="reference"><button id="submit" onclick="document.querySelector('#result').textContent='Confirmed '+document.querySelector('#reference').value">Confirm</button><p id="result">Pending</p></main></body></html>`))
	}))
	defer fixture.Close()
	workflow := browser.Workflow{URL: fixture.URL, StepTimeoutMS: 500, Fixtures: map[string]string{"reference": "cli-fixture"}, Steps: []browser.WorkflowStep{
		{Action: "fill", Selector: "#reference", Fixture: "reference"},
		{Action: "click", Selector: "#submit"},
		{Action: "assert_text", Selector: "#result", Text: "Confirmed cli-fixture"},
	}}
	writeWorkflow := func() {
		t.Helper()
		data, err := json.Marshal(workflow)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(directory, "workflow.json"), data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	writeWorkflow()
	eng, err := engine.New(engine.Config{Model: "test-model"})
	if err != nil {
		t.Fatal(err)
	}
	cmd := &BrowserVerificationCommand{frontend: &frontend{eng: eng}}
	output, err := cmd.Execute(context.Background(), command.Input{Cwd: directory, Args: []string{"workflow.json", "--allow-local"}})
	if err != nil {
		t.Fatal(err)
	}
	var evidence browser.Report
	if err := json.Unmarshal([]byte(output.Data), &evidence); err != nil {
		t.Fatal(err)
	}
	if evidence.Status == browser.StatusUnverified && evidence.Reason == "chrome_unavailable" {
		t.Skip("Chrome workflow unavailable (requires -tags chromedp and installed Chrome); real CLI DOM verification unverified")
	}
	report, err := eng.LastAcceptance()
	if err != nil || report == nil || len(report.Browser) != 1 || report.Browser[0].Status != browser.StatusPass || report.Browser[0].Assertions != 2 || len(report.Browser[0].Artifacts) != 2 {
		t.Fatalf("real Chrome CLI acceptance: %+v %v", report, err)
	}
	entries, err := os.ReadDir(filepath.Join(directory, "config", "acceptance"))
	if err != nil || len(entries) != 1 {
		t.Fatalf("acceptance files: %v %v", entries, err)
	}
	readSaved := func() engine.AcceptanceReport {
		t.Helper()
		data, err := os.ReadFile(filepath.Join(directory, "config", "acceptance", entries[0].Name()))
		if err != nil {
			t.Fatal(err)
		}
		var saved engine.AcceptanceReport
		if err := json.Unmarshal(data, &saved); err != nil {
			t.Fatal(err)
		}
		return saved
	}
	saved := readSaved()
	if saved.Browser[0].Status != browser.StatusPass || saved.Browser[0].Artifacts[0].SHA256 != report.Browser[0].Artifacts[0].SHA256 {
		t.Fatalf("real Chrome CLI reload: %+v", saved)
	}
	report.Browser[0].Steps[0].Status = "tampered"
	report.Browser[0].Artifacts[0].SHA256 = "tampered"
	again, err := eng.LastAcceptance()
	if err != nil || again.Browser[0].Steps[0].Status != browser.StatusPass || again.Browser[0].Artifacts[0].SHA256 == "tampered" {
		t.Fatalf("real Chrome CLI clone alias: %+v %v", again, err)
	}
	workflow.Steps[2].Text = "wrong expected result"
	writeWorkflow()
	output, err = cmd.Execute(context.Background(), command.Input{Cwd: directory, Args: []string{"workflow.json", "--allow-local"}})
	if err != nil {
		t.Fatal(err)
	}
	saved = readSaved()
	if len(saved.Browser) != 2 || saved.Browser[1].Status != browser.StatusFail || saved.Browser[1].Reason != "assertion_failed" || !strings.Contains(output.Data, "assertion_failed") {
		t.Fatalf("real Chrome CLI failure lost: %+v", saved)
	}
	t.Logf("negative CLI proof: wrong DOM text persisted status=%s reason=%s assertions=%d screenshots=%d", saved.Browser[1].Status, saved.Browser[1].Reason, saved.Browser[1].Assertions, len(saved.Browser[1].Artifacts))
}
