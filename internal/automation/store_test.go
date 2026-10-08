package automation

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func testStore(t *testing.T) *Store {
	t.Helper()
	store, err := Open(t.TempDir(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return store
}

func testSpec() Spec {
	return Spec{ID: "maintenance", Prompt: "fix tests", Enabled: true, TimeoutSeconds: 30, BudgetUSD: 1, MaxTurns: 3}
}

func TestStorePersistenceAndValidation(t *testing.T) {
	store := testStore(t)
	spec := testSpec()
	spec.EverySeconds = 60
	now := time.Now().UTC()
	if err := store.Add(spec, now); err != nil {
		t.Fatal(err)
	}
	snapshot, err := store.Read()
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Version != Version || len(snapshot.Specs) != 1 || !snapshot.Specs[0].NextRun.Equal(now.Add(time.Minute)) {
		t.Fatalf("snapshot = %+v", snapshot)
	}
	if err := store.Add(spec, now); err == nil {
		t.Fatal("duplicate spec accepted")
	}
	spec.ID = "../escape"
	if err := store.Add(spec, now); err == nil {
		t.Fatal("unsafe id accepted")
	}
	data, err := os.ReadFile(store.Path())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"version": 1`) {
		t.Fatal("missing version")
	}
	if err := store.Remove("maintenance"); err != nil {
		t.Fatal(err)
	}
	snapshot, err = store.Read()
	if err != nil || len(snapshot.Specs) != 0 {
		t.Fatalf("remove: %+v %v", snapshot, err)
	}
}

func TestStoreRefusesStateInsideOriginalProject(t *testing.T) {
	project := t.TempDir()
	_, err := Open(filepath.Join(project, "state", "nested"), project)
	if err == nil || !strings.Contains(err.Error(), "outside the original project") {
		t.Fatalf("unsafe state accepted: %v", err)
	}
	entries, err := os.ReadDir(project)
	if err != nil || len(entries) != 0 {
		t.Fatalf("refusal wrote original: %v %v", entries, err)
	}
}

func TestStoreReadDoesNotWriteState(t *testing.T) {
	store := testStore(t)
	if _, err := store.Read(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(store.Path()); !os.IsNotExist(err) {
		t.Fatalf("read created state: %v", err)
	}
	if err := store.Add(testSpec(), time.Now()); err != nil {
		t.Fatal(err)
	}
	stamp := time.Now().Add(-time.Hour).Truncate(time.Second)
	if err := os.Chtimes(store.Path(), stamp, stamp); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Read(); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(store.Path())
	if err != nil || !info.ModTime().Equal(stamp) {
		t.Fatalf("read rewrote state: %v %v", info, err)
	}
}

func TestStoreReviewBoundaries(t *testing.T) {
	store := testStore(t)
	if err := store.Add(testSpec(), time.Now()); err != nil {
		t.Fatal(err)
	}
	_, result, err := store.claim("maintenance", "manual", "", "", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Review(result.ID, "accepted"); err == nil {
		t.Fatal("active worker reviewed")
	}
	if err := store.update(func(state *Snapshot) error {
		state.Results[0].LeaseUntil = time.Now().Add(-time.Hour)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	for _, request := range [][2]string{{result.ID, "invalid"}, {"unknown", "accepted"}} {
		if err := store.Review(request[0], request[1]); err == nil {
			t.Fatalf("invalid review accepted: %v", request)
		}
	}
	state, err := store.Read()
	if err != nil || state.Results[0].State != "running" || state.Results[0].Review != "pending" {
		t.Fatalf("invalid review mutated state: %+v %v", state, err)
	}
	if err := store.Review(result.ID, "rejected"); err != nil {
		t.Fatal(err)
	}
	state, err = store.Read()
	if err != nil || state.Results[0].State != "uncertain" || state.Results[0].Review != "rejected" {
		t.Fatalf("expired review not persisted: %+v %v", state, err)
	}
}
