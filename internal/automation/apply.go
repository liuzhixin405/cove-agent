package automation

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/liuzhixin405/cove-agent/internal/changeset"
)

// ErrAuditNotSaved means the patch is in the project but Review=applied could
// not be recorded; callers must not report the apply as failed.
var ErrAuditNotSaved = errors.New("patch applied, but the review record could not be saved")

// ChangesetID is "inbox:<result-id>".
func ChangesetID(resultID string) string { return "inbox:" + resultID }

// Changeset projects a result. Applicable requires State == "succeeded",
// Error == "", Baseline.Commit and Baseline.Branch set, PatchHash != "", the
// patch file present, and Review not in {rejected, applied}; NotApplicable
// carries the first failing reason. It only describes the result: the
// baseline, check and hash comparisons belong to changeset.Verify.
func (s *Store) Changeset(result Result) changeset.Changeset {
	cs := changeset.Changeset{
		ID:         ChangesetID(result.ID),
		Kind:       changeset.KindMaintenance,
		Title:      result.TaskID,
		Baseline:   result.Baseline,
		PatchHash:  result.PatchHash,
		PatchBytes: result.PatchBytes,
		Review:     changeset.Review{Decision: result.Review},
		Created:    result.Started,
	}
	if cs.Review.Decision == "" {
		cs.Review.Decision = "pending"
	}
	if !result.ReviewedAt.IsZero() {
		at := result.ReviewedAt
		cs.Review.At = &at
	}
	for _, check := range result.Checks {
		status := "passed"
		if check.ExitCode != 0 {
			status = "failed"
		}
		cs.Checks = append(cs.Checks, changeset.Check{
			Argv:     check.Command,
			ExitCode: check.ExitCode,
			Status:   status,
			Attempt:  check.Attempt,
			Output:   outputTail(check.Output),
		})
	}
	cs.NotApplicable = s.notApplicable(result)
	cs.Applicable = cs.NotApplicable == ""
	return cs
}

// notApplicable is "" for an applicable result, otherwise the reason; the
// order puts the most informative reason first.
func (s *Store) notApplicable(result Result) string {
	switch {
	case result.State != "succeeded":
		return fmt.Sprintf("任务未成功（%s），不能应用", result.State)
	case result.Error != "":
		return "任务记录了错误，不能应用：" + result.Error
	case result.Baseline.Commit == "":
		return "此结果产生于支持应用之前，只能导出"
	case result.Baseline.Branch == "":
		return "基线不完整（分离 HEAD 或非标准仓库），只能导出"
	case result.PatchHash == "" && result.Patch == "":
		return "任务没有产生改动，无需应用"
	case result.PatchHash == "":
		return "此结果产生于支持应用之前，只能导出"
	case !validResultID.MatchString(result.ID):
		return "无效的结果 ID"
	}
	if _, err := os.Stat(s.PatchPath(result.ID)); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "补丁文件缺失"
		}
		return "补丁文件不可读：" + err.Error()
	}
	switch result.Review {
	case "rejected":
		return "已 rejected；如需应用请先 review accepted"
	case "applied":
		return "已应用"
	}
	return ""
}

// Apply reads the result, runs verify (normally changeset.Verify) and, on
// success, calls after and records Review=applied, ReviewedAt=now. verify is
// what refuses a non-applicable result. A failure to record returns
// ErrAuditNotSaved wrapping the cause: the patch IS applied.
//
// Lock scope: the result is read under state.lock, verify runs without
// holding it (verify may take seconds of git work), and the outcome is then
// recorded under state.lock again. Cross-process serialisation of applies is
// the caller's changeset.WithProjectLock; a second apply that slipped past it
// would be refused by workspace.Capture on the now-dirty tree.
func (s *Store) Apply(ctx context.Context, resultID string, verify func(cs changeset.Changeset, patch []byte) error, after func()) error {
	if !validResultID.MatchString(resultID) {
		return fmt.Errorf("invalid result id %q", resultID)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	state, err := s.Read()
	if err != nil {
		return err
	}
	var result *Result
	for index := range state.Results {
		if state.Results[index].ID == resultID {
			result = &state.Results[index]
			break
		}
	}
	if result == nil {
		return fmt.Errorf("unknown result %q", resultID)
	}
	cs := s.Changeset(*result)
	var patch []byte
	if cs.Applicable {
		if patch, err = s.ReadPatch(resultID); err != nil {
			return err
		}
	}
	if err := verify(cs, patch); err != nil {
		return err
	}
	if after != nil {
		after()
	}
	now := time.Now().UTC()
	if err := s.update(func(state *Snapshot) error {
		for index := range state.Results {
			if state.Results[index].ID == resultID {
				state.Results[index].Review, state.Results[index].ReviewedAt = "applied", now
				return nil
			}
		}
		return fmt.Errorf("unknown result %q", resultID)
	}); err != nil {
		return fmt.Errorf("%w: %v", ErrAuditNotSaved, err)
	}
	return nil
}
