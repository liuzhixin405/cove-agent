package trace

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestWriteAppendsOneJSONLinePerEventAndTailReadsThemBack(t *testing.T) {
	p := filepath.Join(t.TempDir(), "trace.jsonl")
	SetPath(p)
	t.Cleanup(func() { SetPath("") })

	Write("model", map[string]any{"model": "qwen", "ms": 1200, "tokens": 17773})
	Write("tool", map[string]any{"name": "write", "error": true})

	data, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 2 {
		t.Fatalf("want 2 lines, got %d:\n%s", len(lines), data)
	}
	if !strings.Contains(lines[0], `"kind":"model"`) || !strings.Contains(lines[0], `"tokens":17773`) {
		t.Fatalf("first line lacks kind/fields: %s", lines[0])
	}
	events, err := Tail(10)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 || events[0].Kind != "model" || events[1].Kind != "tool" {
		t.Fatalf("Tail = %+v", events)
	}
	if events[1].Fields["error"] != true {
		t.Fatalf("tool event fields = %v", events[1].Fields)
	}
	if got, _ := Tail(1); len(got) != 1 || got[0].Kind != "tool" {
		t.Fatalf("Tail(1) = %+v, want the newest event", got)
	}
}

func TestRotatesAtTheSizeCapAndTailSpansBothFiles(t *testing.T) {
	p := filepath.Join(t.TempDir(), "trace.jsonl")
	SetPath(p)
	t.Cleanup(func() { SetPath("") })
	base := time.Date(2026, 9, 27, 13, 0, 0, 0, time.Local)
	n := 0
	clock = func() time.Time { n++; return base.Add(time.Duration(n) * time.Millisecond) }
	t.Cleanup(func() { clock = time.Now })

	big := strings.Repeat("x", 1<<20) // 1MB per event → rotation after 4
	for i := 0; i < 5; i++ {
		Write("blob", map[string]any{"i": i, "pad": big})
	}
	if _, err := os.Stat(p + ".1"); err != nil {
		t.Fatalf("rotated file missing: %v", err)
	}
	info, _ := os.Stat(p)
	if info.Size() > maxBytes {
		t.Fatalf("current file %d bytes exceeds the cap", info.Size())
	}
	events, _ := Tail(5)
	if len(events) != 5 {
		t.Fatalf("Tail across both files returned %d events, want 5", len(events))
	}
	if events[0].Fields["i"].(float64) != 0 || events[4].Fields["i"].(float64) != 4 {
		t.Fatalf("events out of order: %v %v", events[0].Fields["i"], events[4].Fields["i"])
	}
}

func TestDisabledUnderGoTestUnlessAPathIsSet(t *testing.T) {
	SetPath("")
	resolved = false
	t.Cleanup(func() { SetPath("") })
	if Path() != "" {
		t.Fatalf("Path under go test = %q, want disabled", Path())
	}
	Write("model", map[string]any{"x": 1}) // must not panic or write anywhere
}

// The start-up hint drops a problem once the same model has completed a
// call since, so a failed call (an "error" field) must not count as one,
// and the rotated file counts too.
func TestLastModelSuccessIgnoresFailedCallsAndReadsTheRotatedFile(t *testing.T) {
	p := filepath.Join(t.TempDir(), "trace.jsonl")
	SetPath(p)
	t.Cleanup(func() { SetPath("") })
	if err := os.WriteFile(p+".1", []byte(`{"t":"2026-10-01T10:00:00.000+08:00","kind":"model","model":"old","in":1,"out":1}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	Write("model", map[string]any{"model": "a", "in": 10, "out": 2, "stop": "end_turn"})
	Write("model", map[string]any{"model": "b", "error": "400 bad request"})
	Write("tool", map[string]any{"name": "bash"})

	got := LastModelSuccess()
	if _, ok := got["b"]; ok {
		t.Fatalf("failed call counted as a success: %v", got)
	}
	if got["a"].IsZero() {
		t.Fatalf("successful call missing: %v", got)
	}
	if got["old"].Year() != 2026 || got["old"].Month() != 10 {
		t.Fatalf("rotated file not read: %v", got)
	}
}
