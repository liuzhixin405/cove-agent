package race

import (
	"testing"

	"github.com/liuzhixin405/cove-agent/internal/changeset"
)

func TestChangesetsApplicability(t *testing.T) {
	report := &Report{Version: Version, ID: "run-1", Status: "completed", Spec: Spec{Verify: [][]string{{"go", "test"}}},
		Candidates: [2]Candidate{{ID: "a", Correct: true, Generation: Execution{Status: "passed"}, Verification: []Verification{{Argv: []string{"go", "test"}, Execution: Execution{Status: "passed"}}}, PatchHash: "h", PatchBytes: 3},
			{ID: "b", Correct: false}}}
	cs := Changesets(report)
	if cs[0].ID != "race:run-1/a" || !cs[0].Applicable || cs[0].Kind != changeset.KindRace || len(cs[0].Checks) != 1 || cs[0].Checks[0].Status != "passed" {
		t.Fatalf("a = %+v", cs[0])
	}
	if cs[1].Applicable || cs[1].NotApplicable == "" {
		t.Fatalf("b = %+v", cs[1])
	}
	// No shared checks: Verify would fail with ErrNoChecks, so the list must
	// not offer the candidate either.
	saved := report.Spec.Verify
	report.Spec.Verify, report.Candidates[0].Verification = nil, nil
	if Changesets(report)[0].Applicable {
		t.Fatal("candidate applicable without shared verification")
	}
	report.Spec.Verify, report.Candidates[0].Verification = saved, []Verification{{Argv: []string{"go", "test"}, Execution: Execution{Status: "passed"}}}
	report.Selected = "a"
	if Changesets(report)[0].Applicable {
		t.Fatal("selected run still applicable")
	}
	report.Selected, report.Status = "", "running"
	if Changesets(report)[0].Applicable {
		t.Fatal("running run applicable")
	}
}
