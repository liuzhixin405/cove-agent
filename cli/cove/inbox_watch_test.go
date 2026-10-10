package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/liuzhixin405/cove-agent/internal/automation"
)

func writeInboxState(t *testing.T, path string, ids ...string) {
	t.Helper()
	snap := automation.Snapshot{Version: automation.Version, Project: "p"}
	for _, id := range ids {
		snap.Results = append(snap.Results, automation.Result{ID: id, State: "completed"})
	}
	data, _ := json.Marshal(snap)
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	// mtime granularity on some file systems is a second; force a change.
	future := time.Now().Add(time.Duration(len(ids)) * time.Second)
	_ = os.Chtimes(path, future, future)
}

func TestInboxWatcherReportsOnlyNewResults(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	writeInboxState(t, path, "r1")
	w := &inboxWatcher{interval: 0, known: map[string]bool{}, resolve: func(string) string { return path }}
	if n := w.pollFor("p", time.Now()); n != 0 {
		t.Fatalf("first poll seeds the known set, got %d", n)
	}
	if n := w.poll(time.Now()); n != 0 {
		t.Fatalf("unchanged file reported %d", n)
	}
	writeInboxState(t, path, "r1", "r2", "r3")
	if n := w.poll(time.Now()); n != 2 {
		t.Fatalf("two new results, got %d", n)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if n := w.poll(time.Now()); n != 0 {
		t.Fatalf("missing file must be quiet, got %d", n)
	}
	writeInboxState(t, path, "r4")
	if n := w.poll(time.Now()); n != 1 {
		t.Fatalf("recreated file: %d", n)
	}
	var nilWatcher *inboxWatcher
	if nilWatcher.pollFor("p", time.Now()) != 0 {
		t.Fatal("a nil watcher is quiet")
	}
}

func TestInboxWatcherHonoursInterval(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	writeInboxState(t, path, "r1")
	w := &inboxWatcher{interval: 5 * time.Second, known: map[string]bool{}, resolve: func(string) string { return path }}
	now := time.Now()
	w.pollFor("p", now)
	writeInboxState(t, path, "r1", "r2")
	if n := w.poll(now.Add(time.Second)); n != 0 {
		t.Fatal("polled inside the interval")
	}
	if n := w.poll(now.Add(6 * time.Second)); n != 1 {
		t.Fatalf("after the interval: %d", n)
	}
}

// The watcher follows the working directory and picks up a state file that
// appears later (/automations add after start, /cd to another project).
func TestInboxWatcherFollowsProjectAndLateStateFile(t *testing.T) {
	root := t.TempDir()
	w := &inboxWatcher{interval: 0, known: map[string]bool{}, resolve: func(project string) string {
		return filepath.Join(root, filepath.Base(project), "state.json")
	}}
	a, b := filepath.Join(root, "projA"), filepath.Join(root, "projB")
	if n := w.pollFor(a, time.Now()); n != 0 {
		t.Fatalf("no state file yet: %d", n)
	}
	if err := os.MkdirAll(a, 0700); err != nil {
		t.Fatal(err)
	}
	writeInboxState(t, filepath.Join(a, "state.json"), "r1")
	if n := w.pollFor(a, time.Now()); n != 1 {
		t.Fatalf("late state file must be seen: %d", n)
	}
	if err := os.MkdirAll(b, 0700); err != nil {
		t.Fatal(err)
	}
	writeInboxState(t, filepath.Join(b, "state.json"), "x1", "x2")
	if n := w.pollFor(b, time.Now()); n != 0 {
		t.Fatalf("switching project must start from its current results, got %d", n)
	}
	writeInboxState(t, filepath.Join(b, "state.json"), "x1", "x2", "x3")
	if n := w.pollFor(b, time.Now()); n != 1 {
		t.Fatalf("new result in the new project: %d", n)
	}
}
