package delegate

import (
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
)

// RegressionRun is executable evidence from one go test -json process.
type RegressionRun struct {
	Diagnostics string `json:"diagnostics,omitempty"`
	ExitCode    int    `json:"exit_code"`
	Output      string `json:"output"`
	Error       string `json:"error,omitempty"`
}

// RegressionEvidence compares the same regression tests on two implementations.
type RegressionEvidence struct {
	Baseline string        `json:"baseline,omitempty"`
	Current  string        `json:"current,omitempty"`
	Files    []string      `json:"files,omitempty"`
	Package  string        `json:"package,omitempty"`
	Pattern  string        `json:"pattern,omitempty"`
	Status   string        `json:"status"`
	Reason   string        `json:"reason"`
	Tests    []string      `json:"tests,omitempty"`
	Before   RegressionRun `json:"before"`
	After    RegressionRun `json:"after"`
}

func testOutcomes(output string) (map[string]string, error) {
	decoder := json.NewDecoder(strings.NewReader(output))
	outcomes := make(map[string]string)
	for {
		var event struct{ Action, Package, Test, Output string }
		if err := decoder.Decode(&event); err != nil {
			if err == io.EOF {
				return outcomes, nil
			}
			return nil, fmt.Errorf("invalid go test JSON: %w", err)
		}
		if event.Action == "output" && strings.Contains(event.Output, "panic: test timed out after") {
			return nil, fmt.Errorf("Go test timed out; no completed evidence")
		}
		if event.Package != "" && event.Test != "" && (event.Action == "pass" || event.Action == "fail" || event.Action == "skip") {
			outcomes[event.Package+"/"+event.Test] = event.Action
		}
	}
}

// CompareRegression never treats build failures, skipped tests or interrupted
// processes as proof that a regression test catches the original defect.
func CompareRegression(before, after RegressionRun) RegressionEvidence {
	evidence := RegressionEvidence{Status: "unverified", Before: before, After: after}
	if before.Error != "" || after.Error != "" {
		evidence.Reason = "verification process did not finish normally"
		return evidence
	}
	oldTests, err := testOutcomes(before.Output)
	if err != nil {
		evidence.Reason = err.Error()
		return evidence
	}
	newTests, err := testOutcomes(after.Output)
	if err != nil {
		evidence.Reason = err.Error()
		return evidence
	}
	if after.ExitCode != 0 {
		evidence.Status, evidence.Reason = "failed", "tests fail on the fixed implementation"
		return evidence
	}
	if before.ExitCode == 0 {
		evidence.Status, evidence.Reason = "failed", "tests also pass on the original implementation"
		return evidence
	}
	if before.ExitCode != 1 {
		evidence.Reason = "original test process did not exit with a normal test failure"
		return evidence
	}
	for name, outcome := range oldTests {
		if outcome == "fail" {
			if newTests[name] != "pass" {
				evidence.Reason = "a failing original test was not passed on the fixed implementation: " + name
				return evidence
			}
			evidence.Tests = append(evidence.Tests, name)
		}
	}
	if len(evidence.Tests) == 0 {
		evidence.Reason = "no actual failing test on the original implementation (build failure or no tests)"
		return evidence
	}
	sort.Strings(evidence.Tests)
	evidence.Status, evidence.Reason = "passed", "the same tests fail on the original implementation and pass on the fixed implementation"
	return evidence
}
