package automation

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/liuzhixin405/cove-agent/internal/filelock"
)

const jobLease = 2 * time.Hour

var ErrBusy = errors.New("automation already running or uncertain; inspect and review its inbox result before another run")

type Runner interface {
	Run(context.Context, string, Spec, float64) (Check, error)
}

type Executor struct {
	Store  *Store
	Runner Runner
}

func (s *Store) claim(id, trigger, eventKey, sessionID string, now time.Time) (Spec, Result, error) {
	var selected Spec
	result := Result{ID: "run-" + rand.Text(), TaskID: id, Trigger: trigger, EventKey: eventKey, State: "running", Review: "pending", Started: now, LeaseUntil: now.Add(jobLease), Input: "committed HEAD only; uncommitted and untracked workspace files are excluded"}
	err := s.update(func(state *Snapshot) error {
		for index := range state.Results {
			previous := &state.Results[index]
			if previous.State == "running" && !now.Before(previous.LeaseUntil) {
				previous.State, previous.Error = "uncertain", "worker lease expired; outcome unknown, not retried"
			}
		}
		for index := range state.Specs {
			spec := &state.Specs[index]
			if spec.ID != id {
				continue
			}
			if err := validate(*spec); err != nil {
				return err
			}
			if !spec.Enabled {
				return errors.New("automation disabled; explicitly enable it in a replacement spec")
			}
			if spec.SessionID != "" && spec.SessionID != sessionID {
				return errors.New("automation belongs to a different session")
			}
			for _, previous := range state.Results {
				if previous.TaskID != id {
					continue
				}
				if previous.State == "running" || (previous.State == "uncertain" && previous.Review == "pending") {
					return ErrBusy
				}
				if trigger == "event" && previous.EventKey == eventKey && previous.Trigger == "event" {
					return errors.New("event already claimed")
				}
			}
			if trigger == "schedule" && (spec.EverySeconds == 0 || now.Before(spec.NextRun)) {
				return errors.New("automation is not due")
			}
			if trigger == "event" && (spec.Event == "" || eventKey == "") {
				return errors.New("event name and unique event key are required")
			}
			selected = *spec
			result.SessionID = spec.SessionID
			if trigger == "schedule" {
				spec.NextRun = now.Add(time.Duration(spec.EverySeconds) * time.Second)
			}
			state.Results = append(state.Results, result)
			return nil
		}
		return fmt.Errorf("unknown automation %q", id)
	})
	return selected, result, err
}

func (s *Store) Recover(now time.Time) error {
	return s.update(func(state *Snapshot) error {
		for index := range state.Results {
			result := &state.Results[index]
			if result.State == "running" && !now.Before(result.LeaseUntil) {
				result.State, result.Error = "uncertain", "worker lease expired; outcome unknown, not retried"
			}
		}
		return nil
	})
}

func (s *Store) finish(result Result) error {
	return s.update(func(state *Snapshot) error {
		for index := range state.Results {
			if state.Results[index].ID == result.ID {
				if state.Results[index].State != "running" {
					return errors.New("claim no longer belongs to this worker")
				}
				state.Results[index] = result
				return nil
			}
		}
		return errors.New("result claim missing")
	})
}

func (e *Executor) Run(ctx context.Context, id, sessionID string) (Result, error) {
	return e.run(ctx, id, "manual", "", sessionID, time.Now().UTC())
}

func (e *Executor) RunDue(ctx context.Context, now time.Time, sessionID string) ([]Result, error) {
	if err := e.Store.Recover(now); err != nil {
		return nil, err
	}
	state, err := e.Store.Read()
	if err != nil {
		return nil, err
	}
	var results []Result
	var failures []error
	for _, spec := range state.Specs {
		if !spec.Enabled || spec.EverySeconds == 0 || now.Before(spec.NextRun) || (spec.SessionID != "" && spec.SessionID != sessionID) {
			continue
		}
		if err := ctx.Err(); err != nil {
			return results, errors.Join(append(failures, err)...)
		}
		result, err := e.run(ctx, spec.ID, "schedule", "", sessionID, now)
		if result.ID != "" {
			results = append(results, result)
		}
		if err != nil {
			failures = append(failures, fmt.Errorf("%s: %w", spec.ID, err))
		}
	}
	return results, errors.Join(failures...)
}

func (e *Executor) Trigger(ctx context.Context, event, key, sessionID string) ([]Result, error) {
	if event == "" || key == "" {
		return nil, errors.New("event name and unique event key are required")
	}
	state, err := e.Store.Read()
	if err != nil {
		return nil, err
	}
	var results []Result
	var failures []error
	for _, spec := range state.Specs {
		if !spec.Enabled || spec.Event != event || (spec.SessionID != "" && spec.SessionID != sessionID) {
			continue
		}
		result, err := e.run(ctx, spec.ID, "event", key, sessionID, time.Now().UTC())
		if result.ID != "" {
			results = append(results, result)
		}
		if err != nil {
			failures = append(failures, fmt.Errorf("%s: %w", spec.ID, err))
		}
	}
	return results, errors.Join(failures...)
}

func (e *Executor) run(ctx context.Context, id, trigger, eventKey, sessionID string, now time.Time) (Result, error) {
	if e.Store == nil || e.Runner == nil {
		return Result{}, errors.New("automation requires a store and explicit runner")
	}
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	release, err := filelock.Acquire(filepath.Join(e.Store.dir, "job.lock"), 0, jobLease)
	if err != nil {
		return Result{}, err
	}
	defer release()
	if err := e.Store.Recover(now); err != nil {
		return Result{}, err
	}
	spec, result, err := e.Store.claim(id, trigger, eventKey, sessionID, now)
	if err != nil {
		return Result{}, err
	}
	jobCtx, cancel := context.WithTimeout(ctx, time.Duration(spec.TimeoutSeconds)*time.Second)
	defer cancel()
	result.State = "failed"
	var runErr error
	for attempt := 0; attempt <= spec.Retries; attempt++ {
		result.Attempts++
		work, base, cleanup, err := isolatedWorktree(jobCtx, e.Store.project)
		if err != nil {
			runErr = err
			break
		}
		result.Base = base
		check, err := e.Runner.Run(jobCtx, work, spec, spec.BudgetUSD/float64(spec.Retries+1))
		result.Checks = append(result.Checks, check)
		result.Output = check.Output
		if err == nil && check.ExitCode != 0 {
			err = fmt.Errorf("runner exited %d", check.ExitCode)
		}
		if err == nil {
			for _, argv := range spec.Verify {
				verification, verifyErr := runCommand(jobCtx, work, nil, argv)
				result.Checks = append(result.Checks, verification)
				if verifyErr != nil || verification.ExitCode != 0 {
					err = errors.Join(verifyErr, fmt.Errorf("verification %v exited %d", argv, verification.ExitCode))
					break
				}
			}
		}
		captureCtx, stopCapture := context.WithTimeout(context.Background(), 15*time.Second)
		patch, patchErr := capturePatch(captureCtx, work, base)
		stopCapture()
		result.Patch = patch
		cleanupErr := cleanup()
		runErr = errors.Join(err, patchErr, cleanupErr)
		if jobCtx.Err() != nil {
			result.State, runErr = "interrupted", errors.Join(runErr, jobCtx.Err())
			break
		}
		if patchErr != nil || cleanupErr != nil {
			break
		}
		if runErr == nil {
			result.State = "succeeded"
			break
		}
	}
	if jobCtx.Err() != nil {
		result.State = "interrupted"
		runErr = errors.Join(runErr, jobCtx.Err())
	}
	if runErr != nil {
		result.Error = runErr.Error()
	}
	result.Finished = time.Now().UTC()
	if err := e.Store.finish(result); err != nil {
		return result, errors.Join(runErr, err)
	}
	return result, runErr
}

func isolatedWorktree(ctx context.Context, project string) (string, string, func() error, error) {
	root, err := runCommand(ctx, project, nil, []string{"git", "rev-parse", "--show-toplevel"})
	if err != nil || root.ExitCode != 0 {
		return "", "", nil, errors.New("isolated automation requires a Git repository with a committed HEAD; original workspace was not modified")
	}
	resolved, err := filepath.EvalSymlinks(strings.TrimSpace(root.Output))
	if err != nil {
		return "", "", nil, err
	}
	if !strings.EqualFold(filepath.Clean(resolved), filepath.Clean(project)) {
		return "", "", nil, errors.New("automation project must be the Git repository root")
	}
	head, err := runCommand(ctx, project, nil, []string{"git", "rev-parse", "HEAD"})
	if err != nil || head.ExitCode != 0 {
		return "", "", nil, errors.New("isolated automation requires a committed HEAD")
	}
	base := strings.TrimSpace(head.Output)
	tmp, err := os.MkdirTemp("", "cove-automation-")
	if err != nil {
		return "", "", nil, err
	}
	work := filepath.Join(tmp, "worktree")
	cleanup := func() error {
		cleanCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		check, err := runCommand(cleanCtx, project, nil, []string{"git", "worktree", "remove", "--force", work})
		if err != nil || check.ExitCode != 0 {
			return fmt.Errorf("worktree cleanup failed (%s), retained at %s: %w", check.Output, tmp, err)
		}
		return os.RemoveAll(tmp)
	}
	check, err := runCommand(ctx, project, nil, []string{"git", "worktree", "add", "--detach", work, base})
	if err != nil || check.ExitCode != 0 {
		_ = os.RemoveAll(tmp)
		return "", "", nil, fmt.Errorf("git worktree add failed: %s: %w", check.Output, err)
	}
	return work, base, cleanup, nil
}

func capturePatch(ctx context.Context, work, base string) (string, error) {
	check, err := runCommand(ctx, work, nil, []string{"git", "add", "--all", "--", "."})
	if err != nil || check.ExitCode != 0 {
		return "", fmt.Errorf("cannot collect changed and untracked files: %s: %w", check.Output, err)
	}
	check, err = runCommand(ctx, work, nil, []string{"git", "diff", "--cached", "--binary", "--no-ext-diff", base, "--"})
	if err != nil || check.ExitCode != 0 {
		return "", fmt.Errorf("cannot capture patch: %s: %w", check.Output, err)
	}
	return check.Output, nil
}
