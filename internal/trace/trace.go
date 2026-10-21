// Package trace is cove's interaction log: one JSON line per model call,
// tool call, compaction and turn, with sizes, durations and outcomes, at
// ~/.cove/trace.jsonl. errors.log records only what went wrong; a task that
// "just never finished" left nothing to read. The trace makes "what did the
// engine do, in what order, at what cost" a query instead of a
// reconstruction from session files. It is always on, capped in size and
// rotated once (trace.jsonl.1), and never records message text: sizes,
// names and the head of an error only.
package trace

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"testing"
	"time"
)

// maxBytes is the size at which trace.jsonl is rotated to trace.jsonl.1.
const maxBytes = 4 << 20

var (
	mu       sync.Mutex
	path     string
	resolved bool
	// clock is what stamps events; tests replace it.
	clock = time.Now
)

// SetPath makes events go to p ("" disables tracing). Tests use it; the
// default is ~/.cove/trace.jsonl, and nothing under `go test` unless set.
func SetPath(p string) {
	mu.Lock()
	defer mu.Unlock()
	path = p
	resolved = true
}

// Path returns where events are written, "" when tracing is off.
func Path() string {
	mu.Lock()
	defer mu.Unlock()
	resolveLocked()
	return path
}

func resolveLocked() {
	if resolved {
		return
	}
	resolved = true
	if testing.Testing() {
		path = ""
		return
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		path = ""
		return
	}
	dir := filepath.Join(home, ".cove")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		path = ""
		return
	}
	path = filepath.Join(dir, "trace.jsonl")
}

// Event is one trace line as read back by Tail.
type Event struct {
	Time   time.Time
	Kind   string
	Fields map[string]any
}

// Write appends one event of kind with fields. Keys "t" and "kind" are
// reserved. It never fails loudly: a trace that cannot be written is
// dropped.
func Write(kind string, fields map[string]any) {
	mu.Lock()
	defer mu.Unlock()
	resolveLocked()
	if path == "" {
		return
	}
	line := make(map[string]any, len(fields)+2)
	for k, v := range fields {
		line[k] = v
	}
	line["t"] = clock().Format("2006-01-02T15:04:05.000Z07:00")
	line["kind"] = kind
	data, err := json.Marshal(line)
	if err != nil {
		return
	}
	rotateLocked(len(data) + 1)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return
	}
	_, _ = f.Write(append(data, '\n'))
	_ = f.Close()
}

// rotateLocked moves trace.jsonl to trace.jsonl.1 when adding n bytes would
// take it over maxBytes.
func rotateLocked(n int) {
	info, err := os.Stat(path)
	if err != nil || info.Size()+int64(n) <= maxBytes {
		return
	}
	_ = os.Remove(path + ".1")
	_ = os.Rename(path, path+".1")
}

// Tail returns the last n events (oldest first), from the current file and,
// when it holds fewer than n, the rotated one.
func Tail(n int) ([]Event, error) {
	p := Path()
	if p == "" || n <= 0 {
		return nil, nil
	}
	events := readEvents(p)
	if len(events) < n {
		events = append(readEvents(p+".1"), events...)
	}
	sort.SliceStable(events, func(i, j int) bool { return events[i].Time.Before(events[j].Time) })
	if len(events) > n {
		events = events[len(events)-n:]
	}
	return events, nil
}

// LastModelSuccess returns, per model, when the trace (current file and
// its rotated predecessor) last saw a model call complete: a "model" event
// without an "error" field. The start-up hint uses it to drop problems the
// same model has got past since.
func LastModelSuccess() map[string]time.Time {
	p := Path()
	if p == "" {
		return nil
	}
	out := map[string]time.Time{}
	for _, ev := range append(readEvents(p+".1"), readEvents(p)...) {
		if ev.Kind != "model" || ev.Time.IsZero() {
			continue
		}
		if _, failed := ev.Fields["error"]; failed {
			continue
		}
		model, _ := ev.Fields["model"].(string)
		if model != "" && ev.Time.After(out[model]) {
			out[model] = ev.Time
		}
	}
	return out
}

func readEvents(p string) []Event {
	data, err := os.ReadFile(p)
	if err != nil {
		return nil
	}
	var out []Event
	start := 0
	for i := 0; i <= len(data); i++ {
		if i < len(data) && data[i] != '\n' {
			continue
		}
		line := data[start:i]
		start = i + 1
		if len(line) == 0 {
			continue
		}
		var m map[string]any
		if json.Unmarshal(line, &m) != nil {
			continue
		}
		ev := Event{Fields: m}
		if k, ok := m["kind"].(string); ok {
			ev.Kind = k
		}
		if ts, ok := m["t"].(string); ok {
			ev.Time, _ = time.Parse("2006-01-02T15:04:05.000Z07:00", ts)
		}
		delete(m, "kind")
		delete(m, "t")
		out = append(out, ev)
	}
	return out
}
