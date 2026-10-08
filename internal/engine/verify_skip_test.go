package engine

import (
	"context"
	"strings"
	"testing"

	"github.com/liuzhixin405/cove-agent/internal/api"
)

// countingVerifyGate is a gate for cmds whose commands never start a
// process: every run passes and is counted.
func countingVerifyGate(cmds ...string) (*VerifyGate, *int) {
	g := NewVerifyGate(cmds, "")
	g.ledgerPath = ""
	n := 0
	g.runner = func(context.Context, string, string) (string, int, error) {
		n++
		return "ok", 0, nil
	}
	return g, &n
}

func TestVerifyGateRunSkipsAlreadyPassed(t *testing.T) {
	g, n := countingVerifyGate("go build ./...", "go vet ./...")
	results, passed := g.Run(context.Background(), func(cmd string) bool { return cmd == "go build ./..." })
	if !passed || len(results) != 2 {
		t.Fatalf("passed=%v results=%v", passed, results)
	}
	if *n != 1 {
		t.Fatalf("executor ran %d commands, want 1 (build already passed)", *n)
	}
	if !results[0].Passed || !results[0].Skipped {
		t.Fatalf("result[0] = %+v, want a skipped pass", results[0])
	}
}

func TestVerifyEvidenceMatching(t *testing.T) {
	eng := newPatternEngine(t, &seqProvider{}, nil,
		&mockTool{name: "write", result: "written"},
		&mockTool{name: "read_tool", readOnly: true, result: "ok"})
	shell := func(cmd string) map[string]any { return map[string]any{"command": cmd} }

	cases := []struct {
		name  string
		steps []step
		cmd   string
		want  bool
	}{
		{"passed", steps(step{"bash", shell("go build ./..."), "ok"}), "go build ./...", true},
		{"extra spaces", steps(step{"bash", shell("  go   build   ./... "), ""}), "go build ./...", true},
		{"powershell", steps(step{"powershell", shell("go build ./..."), "ok"}), "go build ./...", true},
		{"failed", steps(step{"bash", shell("go build ./..."), "x.go:1: undefined\n[exit code: 1]"}), "go build ./...", false},
		{"error", steps(step{"bash", shell("go build ./..."), "Error: timed out"}), "go build ./...", false},
		{"compound", steps(step{"bash", shell("go build ./... && go test ./..."), "ok"}), "go build ./...", false},
		{"never ran", nil, "go build ./...", false},
		{"edited after", steps(step{"bash", shell("go build ./..."), "ok"}, step{"write", map[string]any{"filePath": "a.go"}, "written"}), "go build ./...", false},
		{"read after", steps(step{"bash", shell("go build ./..."), "ok"}, step{"read_tool", nil, "ok"}), "go build ./...", true},
		{"fail after pass", steps(step{"bash", shell("go build ./..."), "ok"}, step{"bash", shell("go build ./..."), "[exit code: 2]"}), "go build ./...", false},
	}
	for _, c := range cases {
		l := eng.newTurnLimits()
		for _, s := range c.steps {
			eng.noteVerifyEvidence(l, s.tool, s.input, s.result, strings.HasPrefix(s.result, "Error"))
		}
		if got := l.verifyPassed(c.cmd); got != c.want {
			t.Errorf("%s: verifyPassed(%q) = %v, want %v", c.name, c.cmd, got, c.want)
		}
	}
}

type step struct {
	tool   string
	input  map[string]any
	result string
}

func steps(s ...step) []step { return s }

// verifySkipRun runs a turn that runs `go build ./...` with shellResult,
// then declares itself done; it returns how often the gate ran a command.
func verifySkipRun(t *testing.T, shellResult string) int {
	t.Helper()
	prov := &seqProvider{reply: func(_ context.Context, n int, _ api.ChatRequest) (*api.ChatResponse, error) {
		if n == 0 {
			return toolCallResp("c0", "bash", map[string]any{"command": "go build ./..."}), nil
		}
		return &api.ChatResponse{Content: "构建通过，任务已经全部完成，没有其他需要处理的事项了。"}, nil
	}}
	eng := newPatternEngine(t, prov, func(c *Config) { c.LoopDetectionDisabled = true; c.DoneCheck = "off" },
		&mockTool{name: "bash", result: shellResult})
	g, n := countingVerifyGate("go build ./...")
	eng.verifyGate = g
	if _, err := run(t, eng, "构建一下"); err != nil {
		t.Fatal(err)
	}
	return *n
}

func TestVerifyGateSkipsCommandPassedThisTurn(t *testing.T) {
	if n := verifySkipRun(t, "ok"); n != 0 {
		t.Fatalf("gate ran %d commands, want 0: go build ./... already passed this turn", n)
	}
}

func TestVerifyGateRerunsCommandThatFailedThisTurn(t *testing.T) {
	if n := verifySkipRun(t, "x.go:1: undefined\n[exit code: 1]"); n != 1 {
		t.Fatalf("gate ran %d commands, want 1", n)
	}
}

func TestAcceptanceRecordsFailedAndUnexecutedChecksSeparately(t *testing.T) {
	t.Setenv("COVE_CONFIG_DIR", t.TempDir())
	prov := &seqProvider{reply: func(context.Context, int, api.ChatRequest) (*api.ChatResponse, error) {
		return &api.ChatResponse{Content: "done"}, nil
	}}
	eng := newPatternEngine(t, prov, func(cfg *Config) { cfg.DoneCheck = "off" })
	gate := NewVerifyGate([]string{"build", "test"}, "")
	gate.maxRetries, gate.ledgerPath = 0, ""
	gate.runner = func(context.Context, string, string) (string, int, error) { return "compilation failed", 1, nil }
	eng.verifyGate = gate
	if _, err := run(t, eng, "implement feature"); err != nil {
		t.Fatal(err)
	}
	report, err := eng.LastAcceptance()
	if err != nil || report == nil || len(report.Checks) != 2 || report.Checks[0].Status != "failed" || report.Checks[1].Status != "unverified" || report.Checks[0].Result.Output != "compilation failed" {
		t.Fatalf("acceptance = %+v, %v", report, err)
	}
	report.Checks[0].Result.Output = "tampered"
	again, _ := eng.LastAcceptance()
	if again.Checks[0].Result.Output != "compilation failed" {
		t.Fatal("caller mutated live acceptance evidence")
	}
	eng.acceptance = nil
	reloaded, err := eng.LastAcceptance()
	if err != nil || reloaded.ID != again.ID || reloaded.Checks[1].Status != "unverified" {
		t.Fatalf("persisted report = %+v, %v", reloaded, err)
	}
	eng.verifyGate = NewVerifyGate(nil, "")
	if _, err := run(t, eng, "another task"); err != nil {
		t.Fatal(err)
	}
	next, _ := eng.LastAcceptance()
	if next.ID == again.ID || len(next.Checks) != 0 || !strings.Contains(next.Reason, "不能据此宣称验收通过") {
		t.Fatalf("new task reused old verification: %+v", next)
	}
}

func TestAcceptanceRecordsTimeoutAndReusedEvidence(t *testing.T) {
	eng := newPatternEngine(t, &seqProvider{}, nil)
	eng.beginAcceptance(api.Message{Role: "user", Content: "task"})
	eng.recordAcceptanceResults([]string{"build", "test", "lint"}, []VerifyResult{{Command: "build", Passed: true, Skipped: true}, {Command: "test", TimedOut: true, ExitCode: -1}})
	report, err := eng.LastAcceptance()
	if err != nil || len(report.Checks) != 3 || report.Checks[0].Status != "passed" || !strings.Contains(report.Checks[0].Reason, "复用") || report.Checks[1].Status != "unverified" || report.Checks[2].Status != "unverified" {
		t.Fatalf("timeout/reuse acceptance = %+v, %v", report, err)
	}
}

func TestAcceptanceEvidenceInvalidatedByLaterWrite(t *testing.T) {
	eng := newPatternEngine(t, &seqProvider{}, nil, &mockTool{name: "write", result: "written"}, &mockTool{name: "read_tool", readOnly: true, result: "ok"})
	eng.beginAcceptance(api.Message{Role: "user", Content: "task"})
	eng.recordAcceptanceResults([]string{"build"}, []VerifyResult{{Command: "build", Passed: true}})
	limits := eng.newTurnLimits()
	eng.noteVerifyEvidence(limits, "read_tool", nil, "ok", false)
	report, _ := eng.LastAcceptance()
	if report.Checks[0].Status != "passed" {
		t.Fatal("read-only tool invalidated evidence")
	}
	eng.noteVerifyEvidence(limits, "write", map[string]any{"filePath": "a.go"}, "written", false)
	report, _ = eng.LastAcceptance()
	if report.Checks[0].Status != "unverified" || report.Checks[0].Result != nil {
		t.Fatalf("old pass survived a later write: %+v", report.Checks[0])
	}
}

func TestAcceptanceStateClearedOnConversationReset(t *testing.T) {
	eng := newPatternEngine(t, &seqProvider{}, nil)
	eng.beginAcceptance(api.Message{Role: "user", Content: "old task"})
	eng.resetConversationState()
	if eng.acceptance != nil {
		t.Fatal("conversation reset retained previous acceptance state")
	}
}

func TestRollbackInvalidatesPersistedAcceptanceWithoutLiveCache(t *testing.T) {
	t.Setenv("COVE_CONFIG_DIR", t.TempDir())
	eng := newPatternEngine(t, &seqProvider{}, nil)
	eng.beginAcceptance(api.Message{Role: "user", Content: "checked task"})
	eng.recordAcceptanceResults([]string{"build"}, []VerifyResult{{Command: "build", Passed: true}})
	eng.finishAcceptance(nil)
	eng.acceptance = nil
	eng.invalidateSavedAcceptance()
	eng.acceptance = nil
	report, err := eng.LastAcceptance()
	if err != nil || report == nil || report.Checks[0].Status != "unverified" || report.Checks[0].Result != nil || !strings.Contains(report.Checks[0].Reason, "已回滚") {
		t.Fatalf("persisted pass survived rollback: %+v, %v", report, err)
	}
}
