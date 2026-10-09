package browser

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func testWorkflow() Workflow {
	return Workflow{URL: "https://93.184.216.34", Steps: []WorkflowStep{{Action: "assert_visible", Selector: "body"}}}
}

func TestWorkflowRequiresAssertionsAndGuardsURLs(t *testing.T) {
	browser := New(DefaultConfig())
	workflow := testWorkflow()
	workflow.Steps = []WorkflowStep{{Action: "click", Selector: "button"}}
	if err := browser.validateWorkflow(context.Background(), workflow); err == nil {
		t.Fatal("action-only workflow accepted")
	}
	workflow = testWorkflow()
	workflow.Steps = append(workflow.Steps, WorkflowStep{Action: "navigate", URL: "http://127.0.0.1"})
	if err := browser.validateWorkflow(context.Background(), workflow); err == nil {
		t.Fatal("private navigation accepted")
	}
	workflow = testWorkflow()
	workflow.URL = "file:///tmp/page.html"
	if err := browser.validateWorkflow(context.Background(), workflow); err == nil {
		t.Fatal("file URL accepted")
	}
}

func TestWorkflowDecodeLimits(t *testing.T) {
	for _, input := range []string{`{"url":"https://example.com","allow_localhost":true}`, `{} {}`, strings.Repeat(" ", MaxWorkflowBytes+1)} {
		if _, err := DecodeWorkflow(strings.NewReader(input)); err == nil {
			t.Fatal("unsafe JSON accepted")
		}
	}
}

func TestWorkflowBoundsAndTrustedLoopback(t *testing.T) {
	browser := New(Config{AllowLocalhost: true})
	workflow := testWorkflow()
	workflow.URL = "http://127.0.0.1:12345"
	if err := browser.validateWorkflow(context.Background(), workflow); err != nil {
		t.Fatal(err)
	}
	for _, rawURL := range []string{"http://169.254.169.254", "http://10.0.0.1", "http://192.168.1.1", "http://user:secret@127.0.0.1"} {
		if err := browser.workflowURL(context.Background(), rawURL); err == nil {
			t.Fatalf("trusted loopback expanded to %s", rawURL)
		}
	}
	for _, mutate := range []func(*Workflow){
		func(workflow *Workflow) { workflow.TimeoutMS = 120001 },
		func(workflow *Workflow) { workflow.StepTimeoutMS = 10001 },
		func(workflow *Workflow) { workflow.Steps = make([]WorkflowStep, MaxWorkflowSteps+1) },
		func(workflow *Workflow) {
			workflow.Steps = append(workflow.Steps, WorkflowStep{Action: "evaluate", Text: "alert(1)"})
		},
		func(workflow *Workflow) {
			workflow.Steps = append(workflow.Steps, WorkflowStep{Action: "fill", Selector: "input", Text: "inline input"})
		},
		func(workflow *Workflow) { workflow.Fixtures = map[string]string{"input": strings.Repeat("a", 4097)} },
	} {
		invalid := testWorkflow()
		mutate(&invalid)
		if err := browser.validateWorkflow(context.Background(), invalid); err == nil {
			t.Fatal("out-of-bounds workflow accepted")
		}
	}
	report, err := browser.Run(context.Background(), Workflow{URL: "http://127.0.0.1", Steps: []WorkflowStep{{Action: "click", Selector: "button"}}}, RunOptions{EvidenceDir: t.TempDir()})
	if err == nil || report.Status != StatusUnverified || report.Reason != "invalid_workflow" {
		t.Fatalf("invalid workflow outcome: %+v %v", report, err)
	}
}

func TestWorkflowUnavailableAndCancelledPersist(t *testing.T) {
	if chromeAvailable() {
		t.Skip("disabled backend check")
	}
	browser := New(DefaultConfig())
	report, err := browser.Run(context.Background(), testWorkflow(), RunOptions{EvidenceDir: t.TempDir()})
	if !errors.Is(err, ErrChromeUnavailable) || report.Status != StatusUnverified || report.Reason != "chrome_unavailable" {
		t.Fatalf("unexpected outcome: %+v %v", report, err)
	}
	data, err := os.ReadFile(filepath.Join(report.EvidenceDir, "report.json"))
	if err != nil {
		t.Fatal(err)
	}
	var persisted Report
	if err := json.Unmarshal(data, &persisted); err != nil || persisted.Status != StatusUnverified {
		t.Fatalf("persisted report: %s %v", data, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	report, err = browser.Run(ctx, testWorkflow(), RunOptions{EvidenceDir: t.TempDir()})
	if !errors.Is(err, context.Canceled) || report.Status != StatusUnverified || report.Reason != "cancelled_or_timeout" {
		t.Fatalf("cancelled outcome: %+v %v", report, err)
	}
}
