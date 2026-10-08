package remote

import (
	"encoding/json"
	"testing"
	"time"
)

func TestApprovalBoundSingleUse(t *testing.T) {
	for _, change := range []string{"none", "scope", "tool", "input", "expiry", "deny"} {
		t.Run(change, func(t *testing.T) {
			h, s := testHub()
			input := json.RawMessage(`{"path":"a","value":1}`)
			pending, delivery, err := h.PendingApproval(s.Scope, "write", input, "write a", time.Minute)
			if err != nil {
				t.Fatal(err)
			}
			kind := "approve"
			if change == "deny" {
				kind = "deny"
			}
			if _, err := h.Submit(Action{ID: "approval", Scope: s.Scope, Kind: kind, ApprovalID: pending.ID}); err != nil {
				t.Fatal(err)
			}
			h.Drain(func() Snapshot { return s }, func(Action) error { t.Fatal("approval went to generic apply"); return nil })
			permit := <-delivery
			scope, tool := s.Scope, "write"
			switch change {
			case "scope":
				scope.Task = "new-task"
			case "tool":
				tool = "shell"
			case "input":
				input = json.RawMessage(`{"path":"b","value":1}`)
			case "expiry":
				permit.pending.Expires = time.Now().Add(-time.Second)
			default:
				input = json.RawMessage(`{"value":1,"path":"a"}`)
			}
			if got := permit.Consume(scope, tool, input); got != (change == "none") {
				t.Fatalf("authorized=%t", got)
			}
			if permit.Consume(s.Scope, "write", json.RawMessage(`{"path":"a","value":1}`)) {
				t.Fatal("permit reused")
			}
		})
	}
}

func TestApprovalReplacementAndDrift(t *testing.T) {
	h, s := testHub()
	old, oldDelivery, err := h.PendingApproval(s.Scope, "write", json.RawMessage(`{"path":"a"}`), "a", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	newPending, delivery, err := h.PendingApproval(s.Scope, "write", json.RawMessage(`{"path":"b"}`), "b", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if <-oldDelivery != nil {
		t.Fatal("replaced pending not revoked")
	}
	if _, err := h.Submit(Action{ID: "stale-button", Scope: s.Scope, Kind: "approve", ApprovalID: old.ID}); err != nil {
		t.Fatal(err)
	}
	h.Drain(func() Snapshot { return s }, func(Action) error { t.Fatal("unexpected apply"); return nil })
	result, _ := h.Result("stale-button")
	if result.State != "rejected" {
		t.Fatal("old approval approved changed operation")
	}
	if h.Snapshot().Pending.ID != newPending.ID {
		t.Fatal("new pending lost")
	}
	s.Scope.Project = "different"
	h.Publish(s)
	if <-delivery != nil || h.Snapshot().Pending != nil {
		t.Fatal("drift did not revoke approval")
	}
}

func TestDeliveredPermitRevoked(t *testing.T) {
	for _, reason := range []string{"cancel", "stop", "replacement", "drift"} {
		t.Run(reason, func(t *testing.T) {
			h, s := testHub()
			input := json.RawMessage(`{"path":"a"}`)
			pending, delivery, err := h.PendingApproval(s.Scope, "write", input, "a", time.Minute)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := h.Submit(Action{ID: "approve", Scope: s.Scope, Kind: "approve", ApprovalID: pending.ID}); err != nil {
				t.Fatal(err)
			}
			h.Drain(func() Snapshot { return s }, func(Action) error { return nil })
			permit := <-delivery
			switch reason {
			case "cancel":
				h.CancelApproval(pending.ID)
			case "stop":
				h.Close()
			case "replacement":
				if _, _, err := h.PendingApproval(s.Scope, "write", input, "a", time.Minute); err != nil {
					t.Fatal(err)
				}
			case "drift":
				changed := s
				changed.Scope.Version++
				h.Publish(changed)
			}
			if permit.Consume(s.Scope, "write", input) {
				t.Fatal("revoked permit authorized execution")
			}
		})
	}
}

func TestApprovalPendingScopeAndExpiryFailClosed(t *testing.T) {
	for _, change := range []string{"session", "project", "task", "version", "expiry"} {
		t.Run(change, func(t *testing.T) {
			hub, snapshot := testHub()
			pending, delivery, err := hub.PendingApproval(snapshot.Scope, "write", json.RawMessage(`{"path":"a"}`), "write a", time.Minute)
			if err != nil {
				t.Fatal(err)
			}
			switch change {
			case "session":
				snapshot.Scope.Session = "another-session"
			case "project":
				snapshot.Scope.Project = "another-project"
			case "task":
				snapshot.Scope.Task = "another-task"
			case "version":
				snapshot.Scope.Version++
			case "expiry":
				hub.mu.Lock()
				hub.approval.pending.Expires = time.Now().Add(-time.Second)
				hub.mu.Unlock()
			}
			hub.Publish(snapshot)
			select {
			case permit := <-delivery:
				if permit != nil {
					t.Fatal("invalid pending produced a permit")
				}
			default:
				t.Fatal("invalid pending did not release the real permission waiter")
			}
			if hub.Snapshot().Pending != nil {
				t.Fatal("invalid pending stayed visible")
			}
			if _, err := hub.Submit(Action{ID: "stale", Scope: snapshot.Scope, Kind: "approve", ApprovalID: pending.ID}); err != nil {
				t.Fatal(err)
			}
			hub.Drain(func() Snapshot { return snapshot }, func(Action) error { t.Fatal("approval reached generic apply"); return nil })
			result, _ := hub.Result("stale")
			if result.State != "rejected" || result.Error != ErrDrift.Error() {
				t.Fatalf("invalid pending result=%+v", result)
			}
		})
	}
}
