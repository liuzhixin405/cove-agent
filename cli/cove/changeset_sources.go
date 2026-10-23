package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/liuzhixin405/cove-agent/internal/automation"
	"github.com/liuzhixin405/cove-agent/internal/changeset"
	"github.com/liuzhixin405/cove-agent/internal/race"
	"github.com/liuzhixin405/cove-agent/internal/session"
	"github.com/liuzhixin405/cove-agent/internal/workspace"
)

// turnNotApplicable is why the current turn's edits are never applied: they
// are already in the working tree.
const turnNotApplicable = "本轮改动已在工作区，用 /undo 回退"

// changesetSources maps each kind to its adapter. The race adapter uses the
// registered /race command's service, so /race select and /changes apply
// share one race.Service (and its selection lock).
func (fe *frontend) changesetSources() map[changeset.Kind]changeset.Source {
	sources := map[changeset.Kind]changeset.Source{
		changeset.KindMaintenance: inboxSource{fe: fe},
		changeset.KindTurn:        turnSource{fe: fe},
	}
	if fe != nil && fe.reg != nil {
		if entry, ok := fe.reg.Find("race"); ok {
			if cmd, ok := entry.(*RaceCommand); ok {
				sources[changeset.KindRace] = raceSource{cmd: cmd}
			}
		}
	}
	return sources
}

// parseChangesetID splits "race:<run>/<a|b>" | "inbox:<id>" | "turn" into
// kind and source-local id ("<run>/<a|b>", "<id>", "turn").
func parseChangesetID(id string) (changeset.Kind, string, error) {
	invalid := fmt.Errorf("无效的变更集 ID %q；应为 race:<运行ID>/<a|b>、inbox:<结果ID> 或 turn", id)
	switch {
	case id == "turn":
		return changeset.KindTurn, "turn", nil
	case strings.HasPrefix(id, "inbox:"):
		local := strings.TrimPrefix(id, "inbox:")
		if local == "" || strings.ContainsAny(local, `/\:`) {
			return "", "", invalid
		}
		return changeset.KindMaintenance, local, nil
	case strings.HasPrefix(id, "race:"):
		local := strings.TrimPrefix(id, "race:")
		if _, _, err := splitRaceLocal(local); err != nil {
			return "", "", invalid
		}
		return changeset.KindRace, local, nil
	}
	return "", "", invalid
}

func splitRaceLocal(local string) (run, name string, err error) {
	run, name, ok := strings.Cut(local, "/")
	if !ok || run == "" || (name != "a" && name != "b") {
		return "", "", fmt.Errorf("无效的竞跑候选 %q；应为 <运行ID>/<a|b>", local)
	}
	return run, name, nil
}

// sameProject compares two project paths after resolving symlinks.
func sameProject(a, b string) bool {
	resolve := func(p string) string {
		if abs, err := filepath.Abs(p); err == nil {
			p = abs
		}
		if real, err := filepath.EvalSymlinks(p); err == nil {
			p = real
		}
		return p
	}
	return session.SameProjectDir(resolve(a), resolve(b))
}

// raceSource adapts race reports. Apply is race.Service.Select, the only
// path that applies a race candidate.
type raceSource struct{ cmd *RaceCommand }

func (s raceSource) List(ctx context.Context, project string) ([]changeset.Changeset, error) {
	service, err := s.cmd.initialize()
	if err != nil {
		return nil, err
	}
	reports, err := service.List()
	if err != nil {
		return nil, err
	}
	var out []changeset.Changeset
	for _, report := range reports {
		if !sameProject(report.Baseline.Root, project) {
			continue
		}
		pair := race.Changesets(report)
		out = append(out, pair[0], pair[1])
	}
	return out, nil
}

func (s raceSource) Load(ctx context.Context, project, id string) (changeset.Changeset, changeset.PatchLoader, error) {
	run, name, err := splitRaceLocal(id)
	if err != nil {
		return changeset.Changeset{}, nil, err
	}
	service, err := s.cmd.initialize()
	if err != nil {
		return changeset.Changeset{}, nil, err
	}
	report, err := service.Load(run)
	if err != nil {
		return changeset.Changeset{}, nil, err
	}
	index := 0
	if name == "b" {
		index = 1
	}
	cs := race.Changesets(report)[index]
	directory := service.Directory
	loader := func(ctx context.Context) ([]byte, error) {
		root, err := os.OpenRoot(directory)
		if err != nil {
			return nil, err
		}
		defer root.Close()
		return root.ReadFile(filepath.Join(run, name+".patch"))
	}
	return cs, loader, nil
}

func (s raceSource) Apply(ctx context.Context, project, id string) error {
	run, name, err := splitRaceLocal(id)
	if err != nil {
		return err
	}
	service, err := s.cmd.initialize()
	if err != nil {
		return err
	}
	_, err = service.Select(ctx, project, run, name)
	return err
}

func (s raceSource) Review(ctx context.Context, project, id, decision string) error {
	return errors.New("race 候选不记录人工结论，用 apply 或忽略")
}

// inboxSource adapts maintenance results.
type inboxSource struct{ fe *frontend }

// open opens the project's maintenance store and, as every /inbox command
// does, marks expired worker claims uncertain first.
func (s inboxSource) open(project string) (*automation.Store, automation.Snapshot, error) {
	store, err := automationStore(project)
	if err != nil {
		return nil, automation.Snapshot{}, err
	}
	if err := store.Recover(time.Now().UTC()); err != nil {
		return nil, automation.Snapshot{}, err
	}
	state, err := store.Read()
	if err != nil {
		return nil, automation.Snapshot{}, err
	}
	return store, state, nil
}

func (s inboxSource) List(ctx context.Context, project string) ([]changeset.Changeset, error) {
	store, state, err := s.open(project)
	if err != nil {
		return nil, err
	}
	out := make([]changeset.Changeset, 0, len(state.Results))
	for _, result := range state.Results {
		out = append(out, store.Changeset(result))
	}
	return out, nil
}

func (s inboxSource) Load(ctx context.Context, project, id string) (changeset.Changeset, changeset.PatchLoader, error) {
	store, state, err := s.open(project)
	if err != nil {
		return changeset.Changeset{}, nil, err
	}
	for _, result := range state.Results {
		if result.ID != id {
			continue
		}
		display := result.Patch
		loader := func(ctx context.Context) ([]byte, error) {
			return inboxPatchBytes(store, id, display)
		}
		return store.Changeset(result), loader, nil
	}
	return changeset.Changeset{}, nil, fmt.Errorf("未知的 inbox 结果 %q", id)
}

// inboxPatchBytes is the exact patch file, or the JSON display copy for a
// result without one.
func inboxPatchBytes(store *automation.Store, id, display string) ([]byte, error) {
	data, err := store.ReadPatch(id)
	if errors.Is(err, os.ErrNotExist) {
		return []byte(display), nil
	}
	return data, err
}

func (s inboxSource) Apply(ctx context.Context, project, id string) error {
	store, err := automationStore(project)
	if err != nil {
		return err
	}
	var after func()
	if s.fe != nil && s.fe.eng != nil {
		after = s.fe.eng.InvalidateWorkspaceEvidence
	}
	return store.Apply(ctx, id, func(cs changeset.Changeset, patch []byte) error {
		return changeset.Verify(ctx, project, cs, patch)
	}, after)
}

func (s inboxSource) Review(ctx context.Context, project, id, decision string) error {
	store, err := automationStore(project)
	if err != nil {
		return err
	}
	return store.Review(id, decision)
}

// turnSource is the read-only diff from this turn's first checkpoint to the
// working tree.
type turnSource struct{ fe *frontend }

// diff is the turn's patch; ok is false when the turn changed nothing or the
// diff cannot be produced (a trimmed checkpoint whose tree fallback failed),
// both meaning "no turn changeset".
func (s turnSource) diff(ctx context.Context) ([]byte, bool) {
	if s.fe == nil || s.fe.eng == nil {
		return nil, false
	}
	base, ok := s.fe.eng.TurnChangeBase()
	if !ok {
		return nil, false
	}
	patch, err := s.fe.eng.DiffSinceCheckpoint(ctx, base)
	if err != nil {
		return nil, false
	}
	return patch, true
}

func (s turnSource) changeset(ctx context.Context, project string, patch []byte) changeset.Changeset {
	cs := changeset.Changeset{
		ID:            "turn",
		Kind:          changeset.KindTurn,
		Title:         "本轮改动",
		PatchBytes:    len(patch),
		Review:        changeset.Review{Decision: "pending"},
		Applicable:    false,
		NotApplicable: turnNotApplicable,
		Created:       time.Now().UTC(),
	}
	if len(patch) > 0 {
		cs.PatchHash = workspace.Hash(patch)
	}
	if baseline, err := workspace.Describe(ctx, project); err == nil {
		cs.Baseline = baseline
	}
	return cs
}

func (s turnSource) List(ctx context.Context, project string) ([]changeset.Changeset, error) {
	patch, ok := s.diff(ctx)
	if !ok {
		return nil, nil
	}
	return []changeset.Changeset{s.changeset(ctx, project, patch)}, nil
}

func (s turnSource) Load(ctx context.Context, project, id string) (changeset.Changeset, changeset.PatchLoader, error) {
	if id != "turn" {
		return changeset.Changeset{}, nil, fmt.Errorf("未知的本轮改动 %q", id)
	}
	patch, ok := s.diff(ctx)
	if !ok {
		return changeset.Changeset{}, nil, errors.New("本轮还没有改动文件")
	}
	return s.changeset(ctx, project, patch), func(context.Context) ([]byte, error) { return patch, nil }, nil
}

func (s turnSource) Apply(ctx context.Context, project, id string) error {
	return errors.New(turnNotApplicable)
}

func (s turnSource) Review(ctx context.Context, project, id, decision string) error {
	return errors.New("本轮改动不记录评审结论；用 /changes diff turn 查看，/undo 回退")
}
