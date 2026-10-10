package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"

	"github.com/liuzhixin405/cove-agent/internal/automation"
	"github.com/liuzhixin405/cove-agent/internal/config"
)

// inboxWatcher notices maintenance results another process wrote (cove
// --automation tick from the OS scheduler): the REPL had no way to know, so
// /inbox had to be run on a hunch. It checks the state file's mtime at most
// every interval and counts result IDs it has not seen. It follows the
// working directory (/cd) and a state file that appears after start
// (/automations add); it used to be resolved once, at startup.
type inboxWatcher struct {
	// resolve maps a project directory to its state file ("" when unknown).
	resolve  func(project string) string
	interval time.Duration
	project  string
	path     string
	last     time.Time
	mtime    time.Time
	known    map[string]bool
	seeded   bool
}

// inboxWatchInterval is how often the state file's mtime is checked.
const inboxWatchInterval = 5 * time.Second

// newInboxWatcher returns a watcher that resolves each project's state file
// under the config directory without creating anything.
func newInboxWatcher() *inboxWatcher {
	return &inboxWatcher{interval: inboxWatchInterval, known: map[string]bool{}, resolve: func(project string) string {
		dir, err := config.ConfigDir()
		if err != nil {
			return ""
		}
		external, err := automation.ExternalPath(project, dir)
		if err != nil {
			return ""
		}
		path, err := automation.StatePath(filepath.Join(external, "automations"), project)
		if err != nil {
			return ""
		}
		return path
	}}
}

// pollFor returns how many results appeared since the last poll for the
// project at cwd; 0 inside the interval, when the file is unchanged, missing
// or unreadable. A project switch (or the first poll) seeds the known set
// from the file's current results and reports nothing.
func (w *inboxWatcher) pollFor(cwd string, now time.Time) int {
	if w == nil || now.Sub(w.last) < w.interval {
		return 0
	}
	w.last = now
	if cwd != w.project || !w.seeded {
		// A new project (or the first poll): what the file holds now is
		// old news; only results written from here on are announced. A
		// file that does not exist yet seeds nothing, so everything in it
		// when it appears is new.
		w.project = cwd
		w.path = w.resolve(cwd)
		w.known = map[string]bool{}
		w.mtime = time.Time{}
		w.seeded = true
		if w.path == "" {
			return 0
		}
		if snap, info, ok := w.read(); ok {
			w.mtime = info.ModTime()
			for _, r := range snap.Results {
				w.known[r.ID] = true
			}
		}
		return 0
	}
	if w.path == "" {
		return 0
	}
	snap, info, ok := w.read()
	if !ok || info.ModTime().Equal(w.mtime) {
		return 0
	}
	w.mtime = info.ModTime()
	fresh := 0
	for _, r := range snap.Results {
		if !w.known[r.ID] {
			w.known[r.ID] = true
			fresh++
		}
	}
	return fresh
}

// read parses the state file; ok is false when it is missing or unreadable.
func (w *inboxWatcher) read() (automation.Snapshot, os.FileInfo, bool) {
	var snap automation.Snapshot
	info, err := os.Stat(w.path)
	if err != nil {
		return snap, nil, false
	}
	data, err := os.ReadFile(w.path)
	if err != nil {
		return snap, nil, false
	}
	if err := json.Unmarshal(data, &snap); err != nil {
		return snap, nil, false
	}
	return snap, info, true
}

// poll is pollFor on the watcher's current project (tests).
func (w *inboxWatcher) poll(now time.Time) int {
	if w == nil {
		return 0
	}
	return w.pollFor(w.project, now)
}
