package diagnostic

import (
	"bufio"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/liuzhixin405/cove-agent/internal/log"
)

// RuntimeEvent is one recorded runtime problem (error, warning, stall, etc.).
// Unlike DiagError (which describes a known error catalogue entry), a
// RuntimeEvent is an observed occurrence captured while the agent is running so
// that a later pass — or the user via /diagnose — can review and act on it.
type RuntimeEvent struct {
	Time     time.Time `json:"time"`
	Severity Severity  `json:"severity"`
	Category Category  `json:"category"`
	Message  string    `json:"message"`
	Code     ErrorCode `json:"code,omitempty"` // set when classified
	// Model, Provider and Tool are the call's context when known.
	Model    string `json:"model,omitempty"`
	Provider string `json:"provider,omitempty"`
	Tool     string `json:"tool,omitempty"`
	// Source says how the event was made: "report" (a classified error),
	// "remedy" (what a remedy did), "log" (a Warn/Error log line).
	Source string `json:"source,omitempty"`
}

const maxRuntimeEvents = 200

// maxRuntimeLogBytes caps errors.log before it is rotated to errors.log.1.
const maxRuntimeLogBytes = 1 << 20

var (
	runtimeMu     sync.Mutex
	runtimeEvents []RuntimeEvent

	// runtimePath is resolved lazily on first use and memoized. RecordRuntime is
	// called from every background goroutine in the process (and from the log
	// sink), so the memoization must be synchronized: sync.Once gives the
	// single-initialization guarantee plus the happens-before edge that a plain
	// `if runtimePath != ""` check lacked.
	runtimePathOnce sync.Once
	runtimePath     string // persistent append-only log; "-" disables persistence
)

// runtimeLogPath returns (and memoizes) the path of the persistent runtime
// error log under the user's cove directory.
func runtimeLogPath() string {
	runtimePathOnce.Do(resolveRuntimeLogPath)
	return runtimePath
}

func resolveRuntimeLogPath() {
	// Never persist while running under `go test`: test fixtures deliberately
	// trigger errors (panics, rejected permissions, unknown tools) and must not
	// pollute the user's real ~/.cove/errors.log.
	if testing.Testing() {
		runtimePath = "-"
		return
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		runtimePath = "-" // sentinel: persistence disabled
		return
	}
	dir := filepath.Join(home, ".cove")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		runtimePath = "-" // cannot create dir: disable persistence
		return
	}
	runtimePath = filepath.Join(dir, "errors.log")
}

// RecordRuntime captures a free-form runtime problem (a Warn/Error log line)
// in memory and in the persistent log. It is uncoded: codes come from
// Report, which classifies the error itself instead of guessing from text.
func RecordRuntime(sev Severity, cat Category, message string) {
	record(RuntimeEvent{Time: time.Now(), Severity: sev, Category: cat, Message: message, Source: "log"})
}

// record appends ev to the ring buffer and the persistent log.
func record(ev RuntimeEvent) {
	runtimeMu.Lock()
	runtimeEvents = append(runtimeEvents, ev)
	if len(runtimeEvents) > maxRuntimeEvents {
		runtimeEvents = runtimeEvents[len(runtimeEvents)-maxRuntimeEvents:]
	}
	runtimeMu.Unlock()

	if p := runtimeLogPath(); p != "-" {
		// Every Warn/Error from any goroutine lands here and the file was
		// never trimmed, while LoadRuntimeLog reads it whole. Past the cap the
		// current file becomes errors.log.1 (replacing the previous one), so
		// at most two capped files exist. A concurrent cove doing the same
		// makes one rename fail, which is harmless.
		if info, err := os.Stat(p); err == nil && info.Size() >= maxRuntimeLogBytes {
			_ = os.Rename(p, p+".1")
		}
		if line, err := json.Marshal(ev); err == nil {
			// Tool failure output and provider error bodies land here:
			// private, like trace.jsonl and the input history.
			if f, ferr := os.OpenFile(p, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600); ferr == nil {
				_, _ = f.Write(append(line, '\n'))
				_ = f.Close()
			}
		}
	}
}

// AttachToLogger wires the log package so every Warn/Error log entry is also
// persisted to the runtime error log. Call once at startup. This ensures
// background-task failures (which previously only used log.*f to a possibly
// silent stderr) are captured in the file for later troubleshooting.
// It uses AddSink, not SetSink: a front end registers its own sink to DISPLAY
// warnings, and with the old single-slot SetSink whichever ran last silently
// disabled the other — wiring the UI would have quietly stopped error
// persistence.
func AttachToLogger() {
	log.AddSink(func(level log.Level, msg string) {
		sev := SevWarning
		if level >= log.Error {
			sev = SevError
		}
		RecordRuntime(sev, CatEngine, msg)
	})
}

// RecentRuntime returns a copy of the recorded runtime events, newest last.
func RecentRuntime() []RuntimeEvent {
	runtimeMu.Lock()
	defer runtimeMu.Unlock()
	out := make([]RuntimeEvent, len(runtimeEvents))
	copy(out, runtimeEvents)
	return out
}

// LoadRuntimeLog reads the persistent runtime log so events from previous runs
// are available (e.g. to /diagnose right after a restart following a hang).
func LoadRuntimeLog() []RuntimeEvent {
	p := runtimeLogPath()
	if p == "-" {
		return nil
	}
	f, err := os.Open(p)
	if err != nil {
		return nil
	}
	defer func() { _ = f.Close() }()
	return parseRuntimeLog(f)
}

// parseRuntimeLog reads JSON-lines events from r. A line that is not an
// event, or is too long to be one, is skipped and reading goes on: a
// bufio.Scanner used to stop at the first oversized line and every event
// after it was lost.
func parseRuntimeLog(r io.Reader) []RuntimeEvent {
	var events []RuntimeEvent
	br := bufio.NewReaderSize(r, 64*1024)
	for {
		line, err := br.ReadBytes('\n')
		if len(line) > 0 && len(line) <= maxRuntimeLogBytes {
			var ev RuntimeEvent
			if json.Unmarshal(line, &ev) == nil && !ev.Time.IsZero() {
				events = append(events, ev)
			}
		}
		if err != nil {
			return events
		}
	}
}

// RuntimeSummary is one line of /diagnose errors: a coded problem for one
// model (or an uncoded message), how often and how recently it happened,
// the catalogue's hint, and what a remedy did about it.
type RuntimeSummary struct {
	Code      ErrorCode
	Message   string // the catalogue title when coded, else the normalised text
	Model     string
	Count     int
	Last      time.Time
	Severity  Severity
	Recovery  string
	Applied   []string // remedy results, oldest first
	Detail    string   // the latest event's own text (coded entries)
	HasRemedy bool
}

// SummarizeRuntime aggregates events for display: coded events by code and
// model, whatever their numbers say; uncoded ones by their text with
// numbers and paths blanked, so one problem is one line. Recovered events
// attach to the entry of the same code and model. Ordered by severity then
// frequency; an entry that only holds remedy results sorts first, since
// SevRecovered is the highest value.
func SummarizeRuntime(events []RuntimeEvent) []RuntimeSummary {
	byKey := map[string]*RuntimeSummary{}
	var order []string
	get := func(key string, ev RuntimeEvent) *RuntimeSummary {
		s, ok := byKey[key]
		if !ok {
			s = &RuntimeSummary{Code: ev.Code, Model: ev.Model, Severity: SevInfo}
			if ev.Severity != SevRecovered {
				s.Severity = ev.Severity
			}
			if def := registry[ev.Code]; ev.Code != "" && def != nil {
				s.Message, s.Recovery, s.HasRemedy = def.Message, def.Recovery, def.Remedy != nil
			} else {
				s.Message = normaliseMessage(ev.Message)
			}
			byKey[key] = s
			order = append(order, key)
		}
		return s
	}
	for _, ev := range events {
		key := string(ev.Code) + "|" + ev.Model
		if ev.Code == "" {
			key = "|" + normaliseMessage(ev.Message)
		}
		s := get(key, ev)
		if ev.Severity == SevRecovered {
			s.Applied = append(s.Applied, ev.Message)
			continue
		}
		s.Count++
		if ev.Time.After(s.Last) {
			s.Last, s.Detail = ev.Time, ev.Message
		}
		if ev.Severity > s.Severity {
			s.Severity = ev.Severity
		}
	}
	out := make([]RuntimeSummary, 0, len(order))
	for _, k := range order {
		if s := byKey[k]; s.Count > 0 || len(s.Applied) > 0 {
			out = append(out, *s)
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Severity != out[j].Severity {
			return out[i].Severity > out[j].Severity
		}
		return out[i].Count > out[j].Count
	})
	return out
}

var (
	// pathRe matches a file path: a drive or root followed by at least one
	// separator-delimited segment, ASCII path characters only, so the
	// Chinese words glued to a path's end survive and "/diagnose" (one
	// segment, no separator) is not a path.
	pathRe = regexp.MustCompile(`(?:[A-Za-z]:\\|/|\\\\)[A-Za-z0-9_.~-]+(?:[\\/][A-Za-z0-9_.~-]+)+`)
	// countRe matches the numbers that vary between occurrences of one
	// problem: long ones (token counts, byte sizes, ids) and any number
	// followed by a unit; three-digit HTTP statuses and version digits stay.
	countRe = regexp.MustCompile(`\d{4,}|\d+(?:\.\d+)?\s*(?:tokens?|bytes?|ms|s|秒|次|条|行|个|times?|KB|MB|GB)\b`)
)

// normaliseMessage blanks the parts of a free-form message that differ
// between occurrences of one problem: paths, then counts.
func normaliseMessage(s string) string {
	s = pathRe.ReplaceAllString(s, "#")
	s = countRe.ReplaceAllString(s, "#")
	return strings.Join(strings.Fields(s), " ")
}

// MergeEvents joins the persisted log's events with this session's, once
// each (an event this session wrote is in both), in time order.
func MergeEvents(logged, inMemory []RuntimeEvent) []RuntimeEvent {
	seen := map[string]bool{}
	key := func(ev RuntimeEvent) string {
		return ev.Time.Format(time.RFC3339Nano) + "|" + string(ev.Code) + "|" + ev.Message
	}
	out := make([]RuntimeEvent, 0, len(logged)+len(inMemory))
	for _, list := range [][]RuntimeEvent{logged, inMemory} {
		for _, ev := range list {
			k := key(ev)
			if seen[k] {
				continue
			}
			seen[k] = true
			out = append(out, ev)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Time.Before(out[j].Time) })
	return out
}

// runtimeArchiveDir returns the directory where archived logs are stored.
func runtimeArchiveDir() string {
	p := runtimeLogPath()
	if p == "-" {
		return "-"
	}
	dir := filepath.Join(filepath.Dir(p), "errors-archive")
	_ = os.MkdirAll(dir, 0o755)
	return dir
}

// ArchiveRuntimeLog moves the current runtime log into the archive directory
// (timestamped) and clears the in-memory buffer, beginning a fresh error cycle.
// It returns the archive path, or an empty string if there was nothing to
// archive. Call this once the reported problems have been fixed.
func ArchiveRuntimeLog() (string, error) {
	p := runtimeLogPath()
	if p == "-" {
		return "", nil
	}
	if _, err := os.Stat(p); err != nil {
		if os.IsNotExist(err) {
			// Nothing persisted yet; still clear memory for a clean cycle.
			runtimeMu.Lock()
			runtimeEvents = nil
			runtimeMu.Unlock()
			return "", nil
		}
		return "", err
	}
	dir := runtimeArchiveDir()
	if dir == "-" {
		return "", nil
	}
	dest := filepath.Join(dir, "errors-"+time.Now().Format("20060102-150405")+".log")
	if err := os.Rename(p, dest); err != nil {
		return "", err
	}
	runtimeMu.Lock()
	runtimeEvents = nil
	runtimeMu.Unlock()
	return dest, nil
}

// UnresolvedFromLog counts, in the persisted log, the coded problems of
// severity ERROR or worse that have a hint; the start-up hint points at
// /diagnose errors when it is not 0. A remedy's result does not settle a
// problem here: remedies act for one session (a learned window is gone at
// the next start), so what they fixed is exactly what the person still has
// to make permanent.
func UnresolvedFromLog() int { return unresolvedIn(LoadRuntimeLog()) }

func unresolvedIn(events []RuntimeEvent) int {
	type key struct {
		code  ErrorCode
		model string
	}
	open := map[key]bool{}
	for _, ev := range events {
		if ev.Code == "" {
			continue
		}
		k := key{ev.Code, ev.Model}
		// SevRecovered sorts above SevFatal, so it is excluded explicitly.
		if ev.Severity >= SevError && ev.Severity != SevRecovered {
			if def := registry[ev.Code]; def != nil && def.Recovery != "" {
				open[k] = true
			}
		}
	}
	return len(open)
}
