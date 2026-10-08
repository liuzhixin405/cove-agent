package engine

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/liuzhixin405/cove-agent/internal/api"
	"github.com/liuzhixin405/cove-agent/internal/browser"
	"github.com/liuzhixin405/cove-agent/internal/delegate"
	"github.com/liuzhixin405/cove-agent/internal/session"
)

func browserEvidenceEngine(project, sessionID string) *Engine {
	return &Engine{session: &session.Record{ID: sessionID}, verifyGate: NewVerifyGate(nil, project)}
}

func passingBrowserEvidence() browser.Report {
	return browser.Report{Version: 1, Status: browser.StatusPass, StartedAt: time.Now(), Assertions: 2, EvidenceDir: "browser-fixture", Steps: []browser.StepEvidence{
		{Viewport: "desktop", Action: "assert_visible", Status: browser.StatusPass},
		{Viewport: "mobile", Action: "assert_visible", Status: browser.StatusPass},
	}, Artifacts: []browser.Artifact{
		{Name: "desktop.png", Viewport: "desktop", SHA256: strings.Repeat("a", 64), Bytes: 100, Width: 1280, Height: 800},
		{Name: "mobile.png", Viewport: "mobile", SHA256: strings.Repeat("b", 64), Bytes: 100, Width: 390, Height: 844},
	}}
}

func TestBrowserEvidenceRecordReloadCloneAndStandalone(t *testing.T) {
	t.Setenv("COVE_CONFIG_DIR", t.TempDir())
	project := t.TempDir()
	eng := browserEvidenceEngine(project, "browser-session")
	eng.beginAcceptance(api.Message{Content: "existing task"})
	eng.recordAcceptanceResults([]string{"build", "test"}, []VerifyResult{{Passed: true}, {Passed: false}})
	eng.recordRegressionEvidence(delegate.RegressionEvidence{Status: "passed", Tests: []string{"regression"}, Files: []string{"source.go"}})
	eng.finishAcceptance(nil)
	old, err := eng.LastAcceptance()
	if err != nil {
		t.Fatal(err)
	}
	evidence := passingBrowserEvidence()
	if err := eng.RecordBrowserVerification(evidence); err != nil {
		t.Fatal(err)
	}
	evidence.Steps[0].Status, evidence.Artifacts[0].SHA256 = "tampered", "tampered"
	report, err := eng.LastAcceptance()
	if err != nil || report == nil {
		t.Fatalf("record: %+v %v", report, err)
	}
	if report.ID == old.ID || report.Outcome != "finished" || report.FinishedAt.IsZero() || report.Request != "browser verification" || len(report.Checks) != 2 || report.Checks[1].Status != "failed" || len(report.Regression) != 1 || report.Regression[0].Tests[0] != "regression" {
		t.Fatalf("standalone report lost existing evidence: %+v", report)
	}
	if report.Browser[0].Steps[0].Status != browser.StatusPass || report.Browser[0].Artifacts[0].SHA256 != strings.Repeat("a", 64) {
		t.Fatal("input slices alias stored evidence")
	}
	for _, expected := range []string{"Browser [pass]", "assertions=2", "screenshots=2", "desktop.png", "mobile.png", "sha256=" + strings.Repeat("a", 64)} {
		if !strings.Contains(report.Summary(), expected) {
			t.Fatalf("summary missing %q: %s", expected, report.Summary())
		}
	}
	report.Browser[0].Steps[0].Status = "tampered"
	report.Browser[0].Artifacts[0].Name = "tampered"
	report.Regression[0].Tests[0] = "tampered"
	again, err := eng.LastAcceptance()
	if err != nil || again.Browser[0].Steps[0].Status != browser.StatusPass || again.Browser[0].Artifacts[0].Name != "desktop.png" || again.Regression[0].Tests[0] != "regression" {
		t.Fatalf("snapshot alias: %+v %v", again, err)
	}
	reloaded, err := browserEvidenceEngine(project, "browser-session").LastAcceptance()
	if err != nil || reloaded == nil || reloaded.ID != again.ID || reloaded.Browser[0].Status != browser.StatusPass || reloaded.Checks[1].Status != "failed" {
		t.Fatalf("disk reload: %+v %v", reloaded, err)
	}
}

func TestBrowserEvidenceCurrentScopeBoundAndNoEmptyPass(t *testing.T) {
	t.Setenv("COVE_CONFIG_DIR", t.TempDir())
	project := t.TempDir()
	eng := browserEvidenceEngine(project, "scope-a")
	eng.beginAcceptance(api.Message{Content: "current task"})
	currentID := eng.acceptance.ID
	for index := 0; index < 18; index++ {
		evidence := passingBrowserEvidence()
		evidence.Reason = fmt.Sprintf("run-%d", index)
		if err := eng.RecordBrowserVerification(evidence); err != nil {
			t.Fatal(err)
		}
	}
	report, err := eng.LastAcceptance()
	if err != nil || report.ID != currentID || report.Outcome != "running" || len(report.Browser) != 16 || report.Browser[0].Reason != "run-2" || report.Browser[15].Reason != "run-17" {
		t.Fatalf("running/bounded report: %+v %v", report, err)
	}
	eng.finishAcceptance(nil)
	for _, other := range []*Engine{browserEvidenceEngine(project, "scope-b"), browserEvidenceEngine(t.TempDir(), "scope-a")} {
		report, err := other.LastAcceptance()
		if err != nil || report != nil {
			t.Fatalf("cross-scope report: %+v %v", report, err)
		}
		if err := other.RecordBrowserVerification(browser.Report{Version: 1, Status: browser.StatusPass}); err != nil {
			t.Fatal(err)
		}
		report, err = other.LastAcceptance()
		if err != nil || report.Browser[0].Status != browser.StatusUnverified || len(report.Checks) != 0 || len(report.Regression) != 0 || report.Outcome != "finished" {
			t.Fatalf("empty pass became verified or borrowed scope: %+v %v", report, err)
		}
	}
}

func TestBrowserEvidenceMutationAndRollbackReload(t *testing.T) {
	t.Setenv("COVE_CONFIG_DIR", t.TempDir())
	project := t.TempDir()
	eng := browserEvidenceEngine(project, "mutation")
	if err := eng.RecordBrowserVerification(passingBrowserEvidence()); err != nil {
		t.Fatal(err)
	}
	mutation := browserEvidenceEngine(project, "mutation")
	mutation.invalidateAcceptance("")
	reloaded, err := browserEvidenceEngine(project, "mutation").LastAcceptance()
	if err != nil || reloaded.Browser[0].Status != browser.StatusUnverified || !strings.Contains(reloaded.Browser[0].Reason, "workspace may have changed") || len(reloaded.Browser[0].Artifacts) != 2 {
		t.Fatalf("mutation retained stale disk pass: %+v %v", reloaded, err)
	}
	if err := mutation.RecordBrowserVerification(passingBrowserEvidence()); err != nil {
		t.Fatal(err)
	}
	rollback := browserEvidenceEngine(project, "mutation")
	rollback.invalidateSavedAcceptance()
	reloaded, err = browserEvidenceEngine(project, "mutation").LastAcceptance()
	if err != nil || reloaded.Browser[1].Status != browser.StatusUnverified || !strings.Contains(reloaded.Browser[1].Reason, "rolled back") || reloaded.Browser[0].Status != browser.StatusUnverified {
		t.Fatalf("rollback retained stale disk pass: %+v %v", reloaded, err)
	}
}

func TestBrowserEvidencePersistenceAndReadErrors(t *testing.T) {
	root := t.TempDir()
	t.Setenv("COVE_CONFIG_DIR", root)
	eng := browserEvidenceEngine(t.TempDir(), "errors")
	eng.beginAcceptance(api.Message{Content: "current task"})
	if err := os.WriteFile(filepath.Join(root, "acceptance"), []byte("not a directory"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := eng.RecordBrowserVerification(passingBrowserEvidence()); err == nil {
		t.Fatal("persistence failure swallowed")
	}
	report, err := eng.LastAcceptance()
	if err != nil || len(report.Browser) != 0 {
		t.Fatalf("failed save published evidence: %+v %v", report, err)
	}
	if err := os.Remove(filepath.Join(root, "acceptance")); err != nil {
		t.Fatal(err)
	}
	eng.acceptance = nil
	path, err := acceptancePath(eng.verifyGate.workDir, eng.SessionID())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("invalid JSON"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := eng.RecordBrowserVerification(passingBrowserEvidence()); err == nil {
		t.Fatal("corrupt saved acceptance overwritten")
	}
}

func TestBrowserEvidenceConcurrentSnapshots(t *testing.T) {
	t.Setenv("COVE_CONFIG_DIR", t.TempDir())
	eng := browserEvidenceEngine(t.TempDir(), "concurrent")
	var workers sync.WaitGroup
	for index := 0; index < 4; index++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for iteration := 0; iteration < 5; iteration++ {
				if err := eng.RecordBrowserVerification(passingBrowserEvidence()); err != nil {
					t.Error(err)
				}
				eng.finishAcceptance(nil)
				report, err := eng.LastAcceptance()
				if err != nil || report == nil {
					t.Errorf("snapshot: %+v %v", report, err)
					return
				}
				report.Browser[0].Steps[0].Status = "detached"
			}
		}()
	}
	workers.Wait()
	report, err := eng.LastAcceptance()
	if err != nil || len(report.Browser) != 16 || report.Browser[0].Steps[0].Status != browser.StatusPass {
		t.Fatalf("concurrent append/clone: %+v %v", report, err)
	}
	reloaded, err := browserEvidenceEngine(eng.verifyGate.workDir, eng.SessionID()).LastAcceptance()
	if err != nil || reloaded == nil || reloaded.ID != report.ID || len(reloaded.Browser) != len(report.Browser) {
		t.Fatalf("concurrent finish overwrote disk evidence: %+v %v", reloaded, err)
	}
}

func TestBrowserEvidenceIncompletePassIsUnverified(t *testing.T) {
	t.Setenv("COVE_CONFIG_DIR", t.TempDir())
	for _, scenario := range []struct {
		name   string
		change func(*browser.Report)
	}{
		{"missing_mobile", func(report *browser.Report) { report.Artifacts[1] = report.Artifacts[0] }},
		{"bad_hash", func(report *browser.Report) { report.Artifacts[0].SHA256 = "not-a-hash" }},
		{"empty_pixels", func(report *browser.Report) { report.Artifacts[0].Bytes = 0 }},
		{"wrong_dimensions", func(report *browser.Report) { report.Artifacts[0].Width = 0 }},
		{"failed_assertion", func(report *browser.Report) { report.Steps[1].Status = browser.StatusFail }},
		{"wrong_count", func(report *browser.Report) { report.Assertions++ }},
		{"missing_assertion", func(report *browser.Report) { report.Steps[1].Action = "click"; report.Assertions-- }},
		{"unknown_status", func(report *browser.Report) { report.Status = "green" }},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			eng := browserEvidenceEngine(t.TempDir(), "incomplete")
			evidence := passingBrowserEvidence()
			scenario.change(&evidence)
			if err := eng.RecordBrowserVerification(evidence); err != nil {
				t.Fatal(err)
			}
			report, err := eng.LastAcceptance()
			if err != nil || report.Browser[0].Status != browser.StatusUnverified {
				t.Fatalf("incomplete browser pass accepted: %+v %v", report, err)
			}
		})
	}
}

func TestBrowserEvidenceFinishCountsAndToolMutation(t *testing.T) {
	t.Setenv("COVE_CONFIG_DIR", t.TempDir())
	eng := newPatternEngine(t, &seqProvider{}, nil, &mockTool{name: "write", result: "written"}, &mockTool{name: "read_tool", readOnly: true, result: "ok"})
	eng.verifyGate = NewVerifyGate(nil, t.TempDir())
	var lines []string
	eng.SetOutput(LineSink(func(line string) { lines = append(lines, line) }))
	eng.beginAcceptance(api.Message{Content: "browser task"})
	for _, evidence := range []browser.Report{passingBrowserEvidence(), {Version: 1, Status: browser.StatusFail, Reason: "assertion_failed"}, {Version: 1, Status: browser.StatusUnverified, Reason: "chrome_unavailable"}} {
		if err := eng.RecordBrowserVerification(evidence); err != nil {
			t.Fatal(err)
		}
	}
	eng.finishAcceptance(nil)
	if !strings.Contains(strings.Join(lines, "\n"), "通过 1，失败 1，未验证 1") {
		t.Fatalf("browser finish counts missing: %v", lines)
	}
	limits := eng.newTurnLimits()
	eng.noteVerifyEvidence(limits, "read_tool", nil, "ok", false)
	report, err := eng.LastAcceptance()
	if err != nil || report.Browser[0].Status != browser.StatusPass {
		t.Fatalf("read-only tool invalidated browser pass: %+v %v", report, err)
	}
	eng.noteVerifyEvidence(limits, "write", map[string]any{"filePath": "source.go"}, "written", false)
	report, err = browserEvidenceEngine(eng.verifyGate.workDir, eng.SessionID()).LastAcceptance()
	if err != nil || report.Browser[0].Status != browser.StatusUnverified || report.Browser[1].Status != browser.StatusFail || report.Browser[2].Status != browser.StatusUnverified {
		t.Fatalf("tool mutation did not persist downgrade: %+v %v", report, err)
	}
}

func TestBrowserEvidenceOldRunningDiskReportIsNotCurrentTask(t *testing.T) {
	t.Setenv("COVE_CONFIG_DIR", t.TempDir())
	project := t.TempDir()
	old := browserEvidenceEngine(project, "resumed")
	old.beginAcceptance(api.Message{Content: "old interrupted task"})
	old.recordAcceptanceResults([]string{"build"}, []VerifyResult{{Passed: false}})
	if err := saveAcceptance(old.acceptance); err != nil {
		t.Fatal(err)
	}
	eng := browserEvidenceEngine(project, "resumed")
	if err := eng.RecordBrowserVerification(browser.Report{Version: 1, Status: browser.StatusUnverified, Reason: "chrome_unavailable"}); err != nil {
		t.Fatal(err)
	}
	report, err := eng.LastAcceptance()
	if err != nil || report.ID == old.acceptance.ID || report.Outcome != "finished" || report.Request != "browser verification" || report.FinishedAt.IsZero() || len(report.Checks) != 1 || report.Checks[0].Status != "failed" || report.Browser[0].Status != browser.StatusUnverified {
		t.Fatalf("old disk running report treated as this turn: %+v %v", report, err)
	}
}
