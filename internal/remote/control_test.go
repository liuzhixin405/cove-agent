package remote

import (
	"encoding/json"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestDriftAndReplay(t *testing.T) {
	h := NewHub()
	s := Snapshot{Scope: Scope{Session: "session", Project: "project", Task: "one", Version: 1}}
	h.Publish(s)
	a := Action{ID: "request", Scope: s.Scope, Kind: "cancel"}
	if _, err := h.Submit(a); err != nil {
		t.Fatal(err)
	}
	if _, err := h.Submit(a); err != ErrReplay {
		t.Fatalf("replay: %v", err)
	}
	s.Scope.Task = "two"
	calls := 0
	h.Drain(func() Snapshot { return s }, func(Action) error { calls++; return nil })
	r, _ := h.Result(a.ID)
	if calls != 0 || r.State != "rejected" || r.Error != ErrDrift.Error() {
		t.Fatalf("calls=%d result=%+v", calls, r)
	}
}

func TestConcurrentSnapshotsActions(t *testing.T) {
	h, s := testHub()
	pending, _, err := h.PendingApproval(s.Scope, "write", json.RawMessage(`{}`), "exact summary", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	var workers sync.WaitGroup
	for index := 0; index < 32; index++ {
		workers.Add(1)
		go func(index int) {
			defer workers.Done()
			for count := 0; count < 100; count++ {
				snapshot := h.Snapshot()
				if snapshot.Pending == nil || snapshot.Pending.ID != pending.ID {
					t.Error("pending disappeared")
					return
				}
				snapshot.Pending.Summary = "mutated client copy"
			}
			if _, err := h.Submit(Action{ID: fmt.Sprintf("request-%d", index), Scope: s.Scope, Kind: "cancel"}); err != nil {
				t.Error(err)
			}
		}(index)
	}
	for count := 0; count < 100; count++ {
		h.Publish(s)
	}
	workers.Wait()
	if h.Snapshot().Pending.Summary != "exact summary" {
		t.Fatal("snapshot aliased internal approval")
	}
	calls := 0
	h.Drain(func() Snapshot { return s }, func(Action) error { calls++; return nil })
	if calls != 32 {
		t.Fatalf("calls=%d", calls)
	}
}

func TestConcurrentPermitSingleUse(t *testing.T) {
	h, s := testHub()
	input := json.RawMessage(`{}`)
	pending, delivery, err := h.PendingApproval(s.Scope, "write", input, "summary", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.Submit(Action{ID: "approve", Scope: s.Scope, Kind: "approve", ApprovalID: pending.ID}); err != nil {
		t.Fatal(err)
	}
	h.Drain(func() Snapshot { return s }, func(Action) error { return nil })
	permit := <-delivery
	var workers sync.WaitGroup
	var accepted atomic.Int32
	for count := 0; count < 32; count++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			if permit.Consume(s.Scope, "write", input) {
				accepted.Add(1)
			}
		}()
	}
	workers.Wait()
	if accepted.Load() != 1 {
		t.Fatalf("accepted=%d", accepted.Load())
	}
}

func TestUnchangedSnapshotDoesNotPublishEvents(t *testing.T) {
	hub, snapshot := testHub()
	before := hub.Snapshot()
	for count := 0; count < 100; count++ {
		hub.Publish(snapshot)
		hub.CancelApproval("not-pending")
		hub.RevokeApproval()
	}
	after := hub.Snapshot()
	if before.EventID != after.EventID || !before.Updated.Equal(after.Updated) {
		t.Fatalf("unchanged snapshot advanced event=%d -> %d", before.EventID, after.EventID)
	}
}
