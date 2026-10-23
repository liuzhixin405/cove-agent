// Package changeset is the shared model for reviewable patches produced by
// different sources (race candidates, maintenance results, current-turn
// edits). It depends only on basic packages; sources adapt their own records
// to Changeset, never the other way round.
package changeset

import (
	"context"
	"errors"
	"time"

	"github.com/liuzhixin405/cove-agent/internal/workspace"
)

// Kind names the source a changeset came from.
type Kind string

const (
	KindRace        Kind = "race"
	KindMaintenance Kind = "maintenance"
	KindTurn        Kind = "turn"
)

// Check is one recorded verification command.
type Check struct {
	Argv     []string      `json:"argv"`
	ExitCode int           `json:"exit_code"`
	Status   string        `json:"status"` // passed | failed | skipped | unverified
	Attempt  int           `json:"attempt,omitempty"`
	Duration time.Duration `json:"duration,omitempty"`
	Output   string        `json:"output,omitempty"`
}

// Review is the user's recorded decision on a changeset.
type Review struct {
	Decision string     `json:"decision"` // pending | accepted | rejected | applied
	At       *time.Time `json:"at,omitempty"`
}

// Changeset describes a patch against a recorded baseline together with the
// evidence needed to decide whether it may be applied.
type Changeset struct {
	ID            string
	Kind          Kind
	Title         string
	Baseline      workspace.Baseline
	PatchHash     string
	PatchBytes    int
	Checks        []Check
	Review        Review
	Applicable    bool
	NotApplicable string
	Created       time.Time
}

// PatchLoader reads a changeset's patch bytes on demand.
type PatchLoader func(ctx context.Context) ([]byte, error)

// Source is implemented by every producer of changesets.
type Source interface {
	List(ctx context.Context, project string) ([]Changeset, error)
	Load(ctx context.Context, project, id string) (Changeset, PatchLoader, error)
	Apply(ctx context.Context, project, id string) error
	Review(ctx context.Context, project, id, decision string) error
}

var (
	ErrNotApplicable = errors.New("changeset is not applicable")
	ErrBaselineDrift = errors.New("project scope, branch, or base commit drift")
	ErrNoChecks      = errors.New("no verification recorded")
	ErrCheckFailed   = errors.New("verification did not pass")
	ErrPatchMismatch = errors.New("patch size or hash mismatch")
)

// LastAttemptChecks returns the checks whose Attempt equals the highest
// Attempt present; with no Attempt markers (all zero) it returns all checks.
func LastAttemptChecks(checks []Check) []Check {
	last := 0
	for _, check := range checks {
		last = max(last, check.Attempt)
	}
	if last == 0 {
		return checks
	}
	var out []Check
	for _, check := range checks {
		if check.Attempt == last {
			out = append(out, check)
		}
	}
	return out
}
