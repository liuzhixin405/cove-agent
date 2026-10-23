package race

import (
	"slices"
	"time"

	"github.com/liuzhixin405/cove-agent/internal/changeset"
)

// ChangesetID is "race:<run>/<a|b>".
func ChangesetID(run, name string) string { return "race:" + run + "/" + name }

// Changesets projects a report's two candidates. Applicable mirrors the
// conditions Select enforces before changeset.Verify: status
// completed/timeout/cancelled, not yet selected, Correct, no Error or
// CleanupError, generation passed, and the verification list equal to
// Spec.Verify with every command passed.
func Changesets(report *Report) [2]changeset.Changeset {
	var out [2]changeset.Changeset
	for i := range report.Candidates {
		out[i] = candidateChangeset(report, &report.Candidates[i])
	}
	return out
}

func candidateChangeset(report *Report, candidate *Candidate) changeset.Changeset {
	cs := changeset.Changeset{
		ID:         ChangesetID(report.ID, candidate.ID),
		Kind:       changeset.KindRace,
		Title:      candidate.Prompt,
		Baseline:   report.Baseline,
		PatchHash:  candidate.PatchHash,
		PatchBytes: candidate.PatchBytes,
		Review:     changeset.Review{Decision: "pending"},
		Created:    report.Started,
	}
	if report.Selected == candidate.ID {
		cs.Review = changeset.Review{Decision: "applied", At: report.SelectedAt}
	}
	for _, verify := range candidate.Verification {
		cs.Checks = append(cs.Checks, changeset.Check{
			Argv:     verify.Argv,
			ExitCode: verify.Execution.ExitCode,
			Status:   verify.Execution.Status,
			Attempt:  1,
			Duration: time.Duration(verify.Execution.DurationMS) * time.Millisecond,
			Output:   verify.Execution.Output,
		})
	}
	switch {
	case report.Status == "running" || report.Selected != "" || (report.Status != "completed" && report.Status != "timeout" && report.Status != "cancelled"):
		cs.NotApplicable = "run is incomplete or already selected"
	case !candidate.Correct || candidate.Error != "" || candidate.CleanupError != "" || candidate.Generation.Status != "passed" || len(report.Spec.Verify) == 0 || len(candidate.Verification) != len(report.Spec.Verify):
		cs.NotApplicable = "candidate did not pass shared verification"
	default:
		cs.Applicable = true
		for i, verify := range candidate.Verification {
			if verify.Execution.Status != "passed" || verify.Execution.ExitCode != 0 || !slices.Equal(verify.Argv, report.Spec.Verify[i]) {
				cs.Applicable, cs.NotApplicable = false, "failed or mismatched shared verification"
				break
			}
		}
	}
	return cs
}
