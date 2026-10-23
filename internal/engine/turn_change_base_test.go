package engine

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/liuzhixin405/cove-agent/internal/api"
	"github.com/liuzhixin405/cove-agent/internal/checkpoint"
)

func newTurnBaseEngine(t *testing.T) (*Engine, string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("GIT_AUTHOR_NAME", "cove test")
	t.Setenv("GIT_AUTHOR_EMAIL", "cove@example.test")
	t.Setenv("GIT_COMMITTER_NAME", "cove test")
	t.Setenv("GIT_COMMITTER_EMAIL", "cove@example.test")
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte("package main\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	eng := newTestEngine(&mockProvider{})
	cp, err := checkpoint.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	eng.cpMgr = cp
	return eng, dir
}

func TestTurnChangeBaseIsFirstCheckpointOfTurn(t *testing.T) {
	eng, dir := newTurnBaseEngine(t)
	ctx := context.Background()

	if _, ok := eng.TurnChangeBase(); ok {
		t.Fatal("base set before any checkpoint")
	}
	eng.checkpointBefore([]api.ToolCall{{Name: "write"}})
	first, ok := eng.TurnChangeBase()
	if !ok || first == "" {
		t.Fatal("first checkpoint of the turn not recorded")
	}
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("one\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	eng.checkpointBefore([]api.ToolCall{{Name: "edit"}})
	if got, _ := eng.TurnChangeBase(); got != first {
		t.Fatalf("a later checkpoint moved the base: %s != %s", got, first)
	}
	// Regression verification takes its own checkpoints mid-turn.
	if _, err := eng.cpMgr.Create("regression"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "b.txt"), []byte("two\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, _ := eng.TurnChangeBase(); got != first {
		t.Fatalf("a direct Create moved the base: %s != %s", got, first)
	}

	diff, err := eng.DiffSinceCheckpoint(ctx, first)
	if err != nil {
		t.Fatalf("DiffSinceCheckpoint: %v", err)
	}
	if s := string(diff); !strings.Contains(s, "b/a.txt") || !strings.Contains(s, "b/b.txt") {
		t.Fatalf("diff since the turn base lacks the turn's writes:\n%s", s)
	}

	if _, err := eng.RestoreCheckpoint(""); err != nil {
		t.Fatalf("undo: %v", err)
	}
	if _, ok := eng.TurnChangeBase(); ok {
		t.Fatal("base kept after /undo")
	}

	// A new turn starts without a base, whatever the previous one recorded.
	eng.checkpointBefore([]api.ToolCall{{Name: "write"}})
	if _, ok := eng.TurnChangeBase(); !ok {
		t.Fatal("checkpoint after undo not recorded")
	}
	eng.resetTurnFileState()
	if _, ok := eng.TurnChangeBase(); ok {
		t.Fatal("base kept into a new turn")
	}

	// A read-only batch takes no checkpoint and records nothing.
	eng.checkpointBefore([]api.ToolCall{{Name: "read"}})
	if _, ok := eng.TurnChangeBase(); ok {
		t.Fatal("read-only batch recorded a base")
	}
}

func TestTurnChangeBaseWithoutCheckpointManager(t *testing.T) {
	eng := newTestEngine(&mockProvider{})
	eng.fileMu.Lock()
	eng.turnChangeBase = "deadbeef"
	eng.fileMu.Unlock()
	if _, ok := eng.TurnChangeBase(); ok {
		t.Fatal("base reported without a checkpoint manager")
	}
	if _, err := eng.DiffSinceCheckpoint(context.Background(), "deadbeef"); err == nil {
		t.Fatal("DiffSinceCheckpoint without a checkpoint manager returned no error")
	}
}

// A file-scoped rollback rewrites the tree too, so it drops the base as well.
func TestTurnChangeBaseClearedByFileRollback(t *testing.T) {
	eng, dir := newTurnBaseEngine(t)
	eng.checkpointBefore([]api.ToolCall{{Name: "write"}})
	base, ok := eng.TurnChangeBase()
	if !ok {
		t.Fatal("base not recorded")
	}
	if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte("package changed\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	plan, err := eng.PreviewCheckpointFiles(base, []string{"main.go"})
	if err != nil {
		t.Fatalf("preview: %v", err)
	}
	if _, err := eng.ApplyCheckpointFiles(plan, plan.ID()); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if _, ok := eng.TurnChangeBase(); ok {
		t.Fatal("base kept after a file rollback")
	}
}

// /cd replaces the checkpoint manager; the base belongs to the old chain.
func TestTurnChangeBaseClearedBySetWorkingDir(t *testing.T) {
	eng, _ := newTurnBaseEngine(t)
	eng.checkpointBefore([]api.ToolCall{{Name: "write"}})
	if _, ok := eng.TurnChangeBase(); !ok {
		t.Fatal("base not recorded")
	}
	eng.SetWorkingDir(t.TempDir())
	if _, ok := eng.TurnChangeBase(); ok {
		t.Fatal("base kept after the working directory changed")
	}
}

// orphanCheckpointRef re-creates the project's checkpoint head without
// parents in the shadow store under the test HOME, as a retention trim does:
// every earlier checkpoint stops being an ancestor of the ref.
func orphanCheckpointRef(t *testing.T, dir string) {
	t.Helper()
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	h := sha256.Sum256([]byte(dir))
	ref := "refs/cove/" + hex.EncodeToString(h[:8])
	git := func(args ...string) string {
		cmd := exec.Command("git", args...)
		cmd.Env = append(os.Environ(), "GIT_DIR="+filepath.Join(home, ".cove", "checkpoints", "store"))
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("git %v: %v", args, err)
		}
		return strings.TrimSpace(string(out))
	}
	tree := git("rev-parse", ref+"^{tree}")
	commit := git("-c", "user.name=t", "-c", "user.email=t@t", "commit-tree", tree, "-m", "trimmed")
	git("update-ref", ref, commit)
}

func TestDiffSinceCheckpointSurvivesTrim(t *testing.T) {
	eng, dir := newTurnBaseEngine(t)
	ctx := context.Background()
	eng.checkpointBefore([]api.ToolCall{{Name: "write"}})
	base, ok := eng.TurnChangeBase()
	if !ok {
		t.Fatal("base not recorded")
	}
	eng.fileMu.Lock()
	tree := eng.turnChangeBaseTree
	eng.fileMu.Unlock()
	if tree == "" {
		t.Fatal("base tree not recorded")
	}
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("one\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	eng.checkpointBefore([]api.ToolCall{{Name: "edit"}})
	orphanCheckpointRef(t, dir)

	if _, err := eng.cpMgr.DiffToWorkTree(ctx, base); err == nil {
		t.Fatal("test setup: base is still an ancestor of the ref")
	}
	diff, err := eng.DiffSinceCheckpoint(ctx, base)
	if err != nil {
		t.Fatalf("DiffSinceCheckpoint after trim: %v", err)
	}
	if !strings.Contains(string(diff), "b/a.txt") {
		t.Fatalf("diff after trim lacks a.txt:\n%s", diff)
	}
	if got, _ := eng.TurnChangeBase(); got != base {
		t.Fatalf("TurnChangeBase = %s, want the commit %s", got, base)
	}
	// Only the recorded base falls back to its tree.
	if _, err := eng.DiffSinceCheckpoint(ctx, "0123456789abcdef0123456789abcdef01234567"); err == nil {
		t.Fatal("unknown hash accepted")
	}
	eng.resetTurnFileState()
	eng.fileMu.Lock()
	tree = eng.turnChangeBaseTree
	eng.fileMu.Unlock()
	if tree != "" {
		t.Fatal("base tree kept into a new turn")
	}
}
