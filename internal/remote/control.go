package remote

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"reflect"
	"sync"
	"time"
)

var (
	ErrDrift       = errors.New("session, project, task or approval changed")
	ErrReplay      = errors.New("request ID already used")
	ErrUnavailable = errors.New("remote control unavailable")
)

type Scope struct {
	Session string `json:"session"`
	Project string `json:"project"`
	Task    string `json:"task"`
	Version uint64 `json:"version,string"`
}

type Pending struct {
	ID      string    `json:"id"`
	Scope   Scope     `json:"scope"`
	Tool    string    `json:"tool"`
	Digest  string    `json:"digest"`
	Summary string    `json:"summary"`
	Expires time.Time `json:"expires"`
}

type Snapshot struct {
	Scope    Scope     `json:"scope"`
	Running  bool      `json:"running"`
	Paused   bool      `json:"paused"`
	Evidence string    `json:"evidence"`
	Pending  *Pending  `json:"pending,omitempty"`
	EventID  uint64    `json:"event_id"`
	Updated  time.Time `json:"updated"`
}

type Action struct {
	ID         string `json:"id"`
	Scope      Scope  `json:"scope"`
	Kind       string `json:"kind"`
	Text       string `json:"text,omitempty"`
	ApprovalID string `json:"approval_id,omitempty"`
}

type Result struct {
	ID      string `json:"id"`
	State   string `json:"state"`
	Error   string `json:"error,omitempty"`
	EventID uint64 `json:"event_id"`
}

type Hub struct {
	mu       sync.Mutex
	snapshot Snapshot
	queue    []Action
	results  map[string]Result
	next     uint64
	closed   bool
	wake     chan struct{}
	approval *approval
	issued   *Permit
}

type approval struct {
	pending  Pending
	delivery chan *Permit
}

type Permit struct {
	mu       sync.Mutex
	pending  Pending
	approved bool
	used     bool
	revoked  bool
}

func NewHub() *Hub { return NewHubWithWake(make(chan struct{}, 1)) }

func NewHubWithWake(wake chan struct{}) *Hub {
	return &Hub{results: make(map[string]Result), wake: wake}
}
func (h *Hub) Wake() <-chan struct{} { return h.wake }

func (h *Hub) Publish(s Snapshot) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return
	}
	h.publishLocked(s)
}

func (h *Hub) publishLocked(s Snapshot) {
	s.Pending = nil
	if h.issued != nil && (h.issued.pending.Scope != s.Scope || !time.Now().Before(h.issued.pending.Expires)) {
		h.issued.revoke()
		h.issued = nil
	}
	if h.approval != nil {
		if h.approval.pending.Scope != s.Scope || !time.Now().Before(h.approval.pending.Expires) {
			h.approval.delivery <- nil
			h.approval = nil
		} else {
			pending := h.approval.pending
			s.Pending = &pending
		}
	}
	s.EventID, s.Updated = h.snapshot.EventID, h.snapshot.Updated
	if reflect.DeepEqual(s, h.snapshot) {
		return
	}
	h.next++
	s.EventID, s.Updated = h.next, time.Now().UTC()
	h.snapshot = s
}

func (h *Hub) Snapshot() Snapshot {
	h.mu.Lock()
	defer h.mu.Unlock()
	s := h.snapshot
	if s.Pending != nil {
		pending := *s.Pending
		s.Pending = &pending
	}
	return s
}

func (h *Hub) Submit(a Action) (Result, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed || h.snapshot.Scope.Session == "" || len(h.queue) >= 128 || len(h.results) >= 4096 {
		return Result{}, ErrUnavailable
	}
	if _, ok := h.results[a.ID]; ok {
		return Result{}, ErrReplay
	}
	if a.ID == "" || len(a.ID) > 128 || len(a.Text) > 8192 {
		return Result{}, errors.New("invalid action fields")
	}
	switch a.Kind {
	case "steer":
		if a.Text == "" {
			return Result{}, errors.New("steer text required")
		}
	case "cancel", "pause", "approve", "deny":
	default:
		return Result{}, errors.New("unsupported action")
	}
	if a.Scope != h.snapshot.Scope {
		return Result{}, ErrDrift
	}
	h.next++
	r := Result{ID: a.ID, State: "queued", EventID: h.next}
	h.results[a.ID] = r
	h.queue = append(h.queue, a)
	select {
	case h.wake <- struct{}{}:
	default:
	}
	return r, nil
}

func (h *Hub) Result(id string) (Result, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	r, ok := h.results[id]
	return r, ok
}

// Drain must run only on the owning CLI thread. Apply must atomically check
// the supplied scope against runner state before mutating it.
func (h *Hub) Drain(snapshot func() Snapshot, apply func(Action) error) {
	for {
		h.mu.Lock()
		if h.closed || len(h.queue) == 0 {
			h.mu.Unlock()
			return
		}
		a := h.queue[0]
		h.queue = h.queue[1:]
		h.mu.Unlock()
		current := snapshot()
		h.mu.Lock()
		h.publishLocked(current)
		var err error
		if a.Scope != current.Scope {
			err = ErrDrift
		}
		if err == nil && (a.Kind == "approve" || a.Kind == "deny") {
			if h.approval == nil || h.approval.pending.ID != a.ApprovalID {
				err = ErrDrift
			} else {
				h.issued = &Permit{pending: h.approval.pending, approved: a.Kind == "approve"}
				h.approval.delivery <- h.issued
				h.approval = nil
				h.snapshot.Pending = nil
			}
			h.mu.Unlock()
		} else {
			h.mu.Unlock()
			if err == nil {
				err = apply(a)
			}
		}
		h.mu.Lock()
		h.next++
		r := Result{ID: a.ID, State: "applied", EventID: h.next}
		if err != nil {
			r.State, r.Error = "rejected", err.Error()
		}
		h.results[a.ID] = r
		h.mu.Unlock()
	}
}

func operationDigest(tool string, input json.RawMessage) (string, error) {
	var value any
	decoder := json.NewDecoder(bytes.NewReader(input))
	decoder.UseNumber()
	if err := decoder.Decode(&value); err != nil {
		return "", err
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return "", errors.New("trailing JSON")
	}
	canonical, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(append(append([]byte(tool), 0), canonical...))
	return hex.EncodeToString(sum[:]), nil
}

func (h *Hub) PendingApproval(scope Scope, tool string, input json.RawMessage, summary string, ttl time.Duration) (Pending, <-chan *Permit, error) {
	digest, err := operationDigest(tool, input)
	if err != nil {
		return Pending{}, nil, err
	}
	if ttl <= 0 || ttl > 5*time.Minute {
		return Pending{}, nil, errors.New("approval TTL must be within five minutes")
	}
	var secret [32]byte
	if _, err := rand.Read(secret[:]); err != nil {
		return Pending{}, nil, err
	}
	p := Pending{ID: hex.EncodeToString(secret[:]), Scope: scope, Tool: tool, Digest: digest, Summary: summary, Expires: time.Now().Add(ttl)}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed || scope != h.snapshot.Scope {
		return Pending{}, nil, ErrDrift
	}
	if h.approval != nil {
		h.approval.delivery <- nil
	}
	if h.issued != nil {
		h.issued.revoke()
		h.issued = nil
	}
	delivery := make(chan *Permit, 1)
	h.approval = &approval{pending: p, delivery: delivery}
	h.publishLocked(h.snapshot)
	return p, delivery, nil
}

// Consume belongs immediately before execution in the real permission path.
// A delivered permit is not authorization until the exact operation is checked.
func (p *Permit) Consume(scope Scope, tool string, input json.RawMessage) bool {
	if p == nil {
		return false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.used {
		return false
	}
	p.used = true
	digest, err := operationDigest(tool, input)
	return err == nil && p.approved && !p.revoked && scope == p.pending.Scope && tool == p.pending.Tool && digest == p.pending.Digest && time.Now().Before(p.pending.Expires)
}

func (p *Permit) revoke() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.revoked = true
}

func (h *Hub) CancelApproval(id string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	changed := false
	if h.approval != nil && h.approval.pending.ID == id {
		h.approval.delivery <- nil
		h.approval = nil
		h.snapshot.Pending = nil
		changed = true
	}
	if h.issued != nil && h.issued.pending.ID == id {
		h.issued.revoke()
		h.issued = nil
		changed = true
	}
	if changed {
		h.next++
		h.snapshot.EventID, h.snapshot.Updated = h.next, time.Now().UTC()
	}
}

func (h *Hub) RevokeApproval() {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.approval != nil {
		h.approval.delivery <- nil
		h.approval = nil
	}
	if h.issued != nil {
		h.issued.revoke()
		h.issued = nil
	}
	h.publishLocked(h.snapshot)
}

func (h *Hub) Close() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.closed = true
	if h.issued != nil {
		h.issued.revoke()
		h.issued = nil
	}
	if h.approval != nil {
		h.approval.delivery <- nil
		h.approval = nil
	}
	for _, a := range h.queue {
		h.next++
		h.results[a.ID] = Result{ID: a.ID, State: "rejected", Error: "remote stopped", EventID: h.next}
	}
	h.queue = nil
	h.snapshot.Pending = nil
}
