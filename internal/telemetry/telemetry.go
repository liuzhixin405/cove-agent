package telemetry

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/liuzhixin405/cove-agent/internal/fsatomic"
	"github.com/liuzhixin405/cove-agent/internal/log"
)

// Event represents a single telemetry event.
type Event struct {
	Type      string    `json:"type"`
	Timestamp time.Time `json:"timestamp"`
	Data      any       `json:"data,omitempty"`
}

// Recorder collects telemetry events with local aggregation.
// All data stays local by default; opt-in for remote reporting.
type Recorder struct {
	mu       sync.Mutex
	events   []Event
	filePath string
	enabled  bool
}

// NewRecorder creates a telemetry recorder with local storage.
func NewRecorder() *Recorder {
	home, _ := os.UserHomeDir()
	path := ""
	if home != "" {
		// Without a home directory there is nowhere to keep the history;
		// "./.cove/telemetry.json" would be written into the project.
		path = filepath.Join(home, ".cove", "telemetry.json")
	}
	return &Recorder{
		filePath: path,
		enabled:  false, // opt-in only
	}
}

// Enable turns on telemetry recording.
//
// enabled is guarded by r.mu like every other field: Record is called from
// background goroutines, so toggling the flag without the lock raced them.
func (r *Recorder) Enable() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.enabled = true
}

// Disable turns off telemetry recording.
func (r *Recorder) Disable() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.enabled = false
}

// Record adds a telemetry event.
func (r *Recorder) Record(eventType string, data any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.enabled {
		return
	}

	// Cap events at 1000 to prevent disk bloat
	if len(r.events) >= 1000 {
		r.events = r.events[500:]
	}

	r.events = append(r.events, Event{
		Type:      eventType,
		Timestamp: time.Now(),
		Data:      data,
	})
}

// RecordUsage captures common usage metrics.
func (r *Recorder) RecordUsage(model string, tokensIn, tokensOut int, cost float64, duration time.Duration) {
	r.Record("usage", map[string]any{
		"model":       model,
		"tokens_in":   tokensIn,
		"tokens_out":  tokensOut,
		"cost":        cost,
		"duration_ms": duration.Milliseconds(),
	})
}

// RecordToolCall captures a tool usage event.
func (r *Recorder) RecordToolCall(toolName string, success bool, duration time.Duration) {
	r.Record("tool_call", map[string]any{
		"tool":        toolName,
		"success":     success,
		"duration_ms": duration.Milliseconds(),
	})
}

// Flush writes accumulated events to disk.
//
// The file is replaced atomically. It used to be rewritten in place with
// os.WriteFile, so a crash or a concurrent reader mid-write could leave a torn
// file; the next Flush then failed to parse it and silently threw away the
// whole history. A file that still fails to parse is now moved aside to
// telemetry.json.bak with a warning, and history starts fresh.
func (r *Recorder) Flush() error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if len(r.events) == 0 || r.filePath == "" {
		return nil
	}

	// Append-only: read existing, merge, write back
	existing, err := readEvents(r.filePath)
	if errors.Is(err, errCorrupt) {
		bak := r.filePath + ".bak"
		if rerr := os.Rename(r.filePath, bak); rerr != nil {
			log.Warnf("telemetry: %s is corrupt (%v) and could not be backed up (%v); starting a new history", r.filePath, err, rerr)
		} else {
			log.Warnf("telemetry: %s is corrupt (%v); kept it as %s and started a new history", r.filePath, err, bak)
		}
		existing = nil
	}
	all := append(existing, r.events...)
	// Cap at 5000
	if len(all) > 5000 {
		all = all[len(all)-5000:]
	}

	if err := os.MkdirAll(filepath.Dir(r.filePath), 0755); err != nil {
		return err
	}
	data, err := json.Marshal(all)
	if err != nil {
		return err
	}

	// Clear the buffer only once the events are on disk; clearing it first
	// lost them whenever the write failed.
	if err := fsatomic.WriteFile(r.filePath, data, 0644); err != nil {
		return err
	}
	r.events = nil
	return nil
}

// Stats returns current in-memory event counts by type.
func (r *Recorder) Stats() map[string]int {
	r.mu.Lock()
	defer r.mu.Unlock()
	counts := make(map[string]int)
	for _, e := range r.events {
		counts[e.Type]++
	}
	return counts
}

// errCorrupt marks a telemetry file that was read but does not parse, as
// opposed to one that is missing or unreadable.
var errCorrupt = errors.New("corrupt telemetry file")

func readEvents(path string) ([]Event, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var events []Event
	if err := json.Unmarshal(data, &events); err != nil {
		return nil, fmt.Errorf("%w: %v", errCorrupt, err)
	}
	return events, nil
}
