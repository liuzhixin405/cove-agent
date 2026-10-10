package main

import (
	"strings"
	"testing"

	"github.com/liuzhixin405/cove-agent/internal/api"
	"github.com/liuzhixin405/cove-agent/internal/session"
)

func saveHistoryRecord(t *testing.T, store *session.Store, id, cwd string) {
	t.Helper()
	r := &session.Record{ID: id, Model: "claude-opus-5", Cwd: cwd, Title: id,
		Messages: []api.Message{{Role: "user", Content: "问题 " + id}, {Role: "assistant", Content: "回答"}}}
	if err := store.Save(r); err != nil {
		t.Fatal(err)
	}
}

// /history clear deletes the current project's sessions, keeps the session
// in use and leaves other projects alone.
func TestHistoryClearInDeletesOnlyThisProjectAndKeepsTheActiveSession(t *testing.T) {
	dir := t.TempDir()
	store := session.NewStoreAt(dir)
	here := t.TempDir()
	there := t.TempDir()
	saveHistoryRecord(t, store, "a", here)
	saveHistoryRecord(t, store, "b", here)
	saveHistoryRecord(t, store, "active", here)
	saveHistoryRecord(t, store, "other", there)

	deleted, err := historyClearIn(store, here, false, "active")
	if err != nil {
		t.Fatalf("historyClearIn: %v", err)
	}
	if deleted != 2 {
		t.Fatalf("deleted %d, want 2", deleted)
	}
	records, _ := store.List()
	ids := map[string]bool{}
	for _, r := range records {
		ids[r.ID] = true
	}
	if len(ids) != 2 || !ids["active"] || !ids["other"] {
		t.Fatalf("remaining sessions = %v, want active and other", ids)
	}
}

// With all=true every project's sessions go, still except the active one.
func TestHistoryClearInAllProjects(t *testing.T) {
	dir := t.TempDir()
	store := session.NewStoreAt(dir)
	saveHistoryRecord(t, store, "a", t.TempDir())
	saveHistoryRecord(t, store, "b", t.TempDir())
	saveHistoryRecord(t, store, "active", t.TempDir())

	deleted, err := historyClearIn(store, t.TempDir(), true, "active")
	if err != nil {
		t.Fatalf("historyClearIn: %v", err)
	}
	if deleted != 2 {
		t.Fatalf("deleted %d, want 2", deleted)
	}
	records, _ := store.List()
	if len(records) != 1 || records[0].ID != "active" {
		t.Fatalf("remaining = %+v, want only active", records)
	}
}

// Sessions the list hides (no genuine turns) are deleted too: the person
// asked for an empty history, not an empty view of it.
func TestHistoryClearInDeletesHiddenSessionsToo(t *testing.T) {
	dir := t.TempDir()
	store := session.NewStoreAt(dir)
	here := t.TempDir()
	r := &session.Record{ID: "empty", Model: "claude-opus-5", Cwd: here, Title: "New session"}
	if err := store.Save(r); err != nil {
		t.Fatal(err)
	}
	deleted, err := historyClearIn(store, here, false, "")
	if err != nil {
		t.Fatalf("historyClearIn: %v", err)
	}
	if deleted != 1 {
		t.Fatalf("deleted %d, want 1", deleted)
	}
}

// Interactive /history clear asks through the confirmation box instead of
// demanding a typed "confirm"; headless (no asker) keeps the typed word.
func TestHistoryClearAsksInteractively(t *testing.T) {
	buf := captureOut(t)
	eng := steerTestEngine(t)
	cwd := currentProjectDir()
	saveHistoryRecord(t, eng.Store(), "a", cwd)
	saveHistoryRecord(t, eng.Store(), "b", cwd)
	asked := 0
	var proceed func()
	handleHistoryClear(eng, false, false, func(scope string, n int, onYes func()) {
		asked++
		proceed = onYes
		if scope != "当前项目" || n != 2 {
			t.Errorf("ask(%q, %d), want (当前项目, 2)", scope, n)
		}
	})
	if asked != 1 || proceed == nil || strings.Contains(buf.String(), "已删除") {
		t.Fatalf("asked = %d, out = %q", asked, buf.String())
	}
	proceed()
	if !strings.Contains(buf.String(), "已删除 2 个会话") {
		t.Fatalf("out = %q", buf.String())
	}
	saveHistoryRecord(t, eng.Store(), "c", cwd)
	handleHistoryClear(eng, false, false, nil)
	if !strings.Contains(buf.String(), "确认请输入: /history clear confirm") {
		t.Fatalf("headless wording missing: %q", buf.String())
	}
}
