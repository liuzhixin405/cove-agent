package delegate

import (
	"context"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/liuzhixin405/cove-agent/internal/textutil"
)

type Activity struct {
	Event
	Started  time.Time
	Ended    time.Time
	LastStep string
}

type activityContextKey struct{}

func ReportWaiting(ctx context.Context, waiting bool, toolName string) {
	if emit, ok := ctx.Value(activityContextKey{}).(func(Event)); ok {
		stage := "tool"
		if waiting {
			stage = "waiting"
		}
		emit(Event{Stage: stage, Tool: toolName})
	}
}

type ActivityStore struct {
	mu      sync.Mutex
	entries map[string]Activity
	wake    chan struct{}
}

func (s *ActivityStore) Wake() <-chan struct{} {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.wake == nil {
		s.wake = make(chan struct{}, 1)
	}
	return s.wake
}

func (s *ActivityStore) Update(event Event) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.entries == nil {
		s.entries = make(map[string]Activity)
	}
	activity, exists := s.entries[event.ID]
	if !exists {
		activity.Started = event.At
	}
	if !activity.Ended.IsZero() {
		return
	}
	event.Task = textutil.ClipRunes(strings.Join(strings.Fields(event.Task), " "), 240)
	event.Summary = textutil.ClipRunes(event.Summary, 240)
	activity.Event = event
	if event.Stage == "tool" && event.Summary != "" {
		activity.LastStep = event.Summary
	}
	if event.Stage == "finished" || event.Stage == "cancelled" || event.Stage == "timed_out" {
		activity.Ended = event.At
	}
	s.entries[event.ID] = activity
	for len(s.entries) > 128 {
		oldestID := ""
		var oldest time.Time
		for id, entry := range s.entries {
			if !entry.Ended.IsZero() && (oldestID == "" || entry.Ended.Before(oldest)) {
				oldestID, oldest = id, entry.Ended
			}
		}
		if oldestID == "" {
			break
		}
		delete(s.entries, oldestID)
	}
	if s.wake != nil {
		select {
		case s.wake <- struct{}{}:
		default:
		}
	}
}

func (s *ActivityStore) Snapshot() []Activity {
	s.mu.Lock()
	defer s.mu.Unlock()
	entries := make([]Activity, 0, len(s.entries))
	for _, entry := range s.entries {
		entries = append(entries, entry)
	}
	sort.Slice(entries, func(left, right int) bool {
		if entries[left].Started.Equal(entries[right].Started) {
			return entries[left].ID < entries[right].ID
		}
		return entries[left].Started.Before(entries[right].Started)
	})
	return entries
}

func (s *ActivityStore) Reset() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.entries = nil
	if s.wake != nil {
		select {
		case s.wake <- struct{}{}:
		default:
		}
	}
}
