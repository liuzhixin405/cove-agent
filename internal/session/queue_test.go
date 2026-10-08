package session

import (
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"

	"github.com/liuzhixin405/cove-agent/internal/api"
)

func TestQueueSnapshotPreservesCurrentPendingAndAttachments(t *testing.T) {
	store := NewQueueStore(t.TempDir())
	cwd := t.TempDir()
	current := api.Message{Role: "user", Content: "interrupted request", Parts: []api.MessagePart{{Type: "text", Text: "attached content"}}}
	pending := []api.Message{{Role: "user", Content: "inspect image", Parts: []api.MessagePart{{Type: "image", MimeType: "image/png", Data: "aW1hZ2U="}}}}
	if err := store.Save(QueueSnapshot{ID: "queue", SessionID: "session", Cwd: cwd, OwnerPID: 123, Current: &current, Pending: pending}); err != nil {
		t.Fatal(err)
	}
	loaded, err := store.Load("queue")
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Version != queueVersion || loaded.SessionID != "session" || loaded.OwnerPID != 123 || loaded.Cwd != NormalizeProjectDir(cwd) || !reflect.DeepEqual(loaded.Current, &current) || !reflect.DeepEqual(loaded.Pending, pending) {
		t.Fatalf("snapshot changed on reload: %+v", loaded)
	}
	listed, err := store.List(cwd)
	if err != nil || len(listed) != 1 || listed[0].ID != "queue" {
		t.Fatalf("list = %+v, %v", listed, err)
	}
	if other, err := store.List(t.TempDir()); err != nil || len(other) != 0 {
		t.Fatalf("another project saw queue: %+v, %v", other, err)
	}
}

func TestQueueSnapshotRejectsUnknownVersionAndUnsafeID(t *testing.T) {
	dir := t.TempDir()
	store := NewQueueStore(dir)
	if err := store.Save(QueueSnapshot{ID: "../escape"}); err == nil {
		t.Fatal("unsafe queue ID accepted")
	}
	if err := os.WriteFile(filepath.Join(dir, "future.json"), []byte(`{"version":99,"id":"future"}`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Load("future"); err == nil {
		t.Fatal("unknown snapshot version accepted")
	}
}

func TestQueueClaimProtectsLiveOwnerAndSession(t *testing.T) {
	store := NewQueueStore(t.TempDir())
	cwd := t.TempDir()
	snapshot := QueueSnapshot{ID: "queue", SessionID: "session", Cwd: cwd, OwnerPID: os.Getpid(), Pending: []api.Message{{Role: "user", Content: "pending"}}}
	if err := store.Save(snapshot); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Claim("queue", cwd, "session"); err == nil {
		t.Fatal("claimed a live process's queue")
	}
	snapshot.OwnerPID = 0
	if err := store.Save(snapshot); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Claim("queue", cwd, "other-session"); err == nil {
		t.Fatal("claimed another session's queue")
	}
	if _, err := store.Claim("queue", t.TempDir(), "session"); err == nil {
		t.Fatal("claimed another project's queue")
	}
	claimed, err := store.Claim("queue", cwd, "session")
	if err != nil || claimed.OwnerPID != os.Getpid() || len(claimed.Pending) != 1 || claimed.Pending[0].Content != "pending" {
		t.Fatalf("claim = %+v, %v", claimed, err)
	}
	if _, err := store.Claim("queue", cwd, "session"); err == nil {
		t.Fatal("queue was claimed twice")
	}
}

func TestQueueConcurrentClaimsOnlyOneSucceeds(t *testing.T) {
	store := NewQueueStore(t.TempDir())
	cwd := t.TempDir()
	if err := store.Save(QueueSnapshot{ID: "queue", Cwd: cwd, SessionID: "session", Pending: []api.Message{{Role: "user", Content: "task"}}}); err != nil {
		t.Fatal(err)
	}
	var workers sync.WaitGroup
	claims := make(chan error, 2)
	for count := 0; count < 2; count++ {
		workers.Go(func() {
			_, err := store.Claim("queue", cwd, "session")
			claims <- err
		})
	}
	workers.Wait()
	close(claims)
	successes := 0
	for err := range claims {
		if err == nil {
			successes++
		}
	}
	if successes != 1 {
		t.Fatalf("successful claimants = %d, want 1", successes)
	}
}
