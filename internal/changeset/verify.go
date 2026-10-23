package changeset

import (
	"context"
	"fmt"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/liuzhixin405/cove-agent/internal/workspace"
)

// Verify runs the four shared checks in race.Service.Select's order, then
// Baseline.Apply. It writes no audit record and takes no lock.
//
// Order: applicability, baseline (clean tree and no drift), verification of
// the last attempt, patch size and hash. The project is not touched unless
// every check passes.
func Verify(ctx context.Context, project string, cs Changeset, patch []byte) error {
	if !cs.Applicable {
		if cs.NotApplicable != "" {
			return fmt.Errorf("%w: %s", ErrNotApplicable, cs.NotApplicable)
		}
		return ErrNotApplicable
	}
	current, err := workspace.Capture(ctx, project)
	if err != nil {
		return err
	}
	if !sameBaseline(current, cs.Baseline) {
		return ErrBaselineDrift
	}
	checks := LastAttemptChecks(cs.Checks)
	if len(checks) == 0 {
		return ErrNoChecks
	}
	for _, check := range checks {
		if check.Status != "passed" || check.ExitCode != 0 {
			return ErrCheckFailed
		}
	}
	if len(patch) != cs.PatchBytes || cs.PatchHash == "" || workspace.Hash(patch) != cs.PatchHash {
		return ErrPatchMismatch
	}
	// Apply through the freshly captured baseline: Baseline.Apply re-checks
	// with exact equality, and the recorded Root may differ from Capture's
	// only in case or cleaning, which sameBaseline already accepted.
	return current.Apply(ctx, patch, cs.PatchHash)
}

// sameBaseline compares Root after filepath.Clean (case-insensitively on
// Windows) and Commit and Branch exactly.
func sameBaseline(a, b workspace.Baseline) bool {
	if a.Commit != b.Commit || a.Branch != b.Branch {
		return false
	}
	ra, rb := filepath.Clean(a.Root), filepath.Clean(b.Root)
	if runtime.GOOS == "windows" {
		return strings.EqualFold(ra, rb)
	}
	return ra == rb
}
