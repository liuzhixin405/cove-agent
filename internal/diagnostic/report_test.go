package diagnostic

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/liuzhixin405/cove-agent/internal/api"
)

const llamaOverflow = `{"error":{"code":400,"message":"request (17964 tokens) exceeds the available context size (16384 tokens), try increasing it","type":"exceed_context_size_error","n_prompt_tokens":17964,"n_ctx":16384}}`

func resetRuntimeEvents(t *testing.T) {
	t.Helper()
	runtimeMu.Lock()
	runtimeEvents = nil
	runtimeMu.Unlock()
	SetRuntime(nil)
	t.Cleanup(func() {
		runtimeMu.Lock()
		runtimeEvents = nil
		runtimeMu.Unlock()
		SetRuntime(nil)
	})
}

// Errors are coded by their type, never by matching Chinese titles against
// the text: the local server's overflow, the chain marking a provider
// unavailable, a tool call with unparsable arguments and a stalled stage all
// get their code.
func TestClassifyGivesEachErrorItsCode(t *testing.T) {
	overflow := &api.StatusError{Status: 400, Msg: llamaOverflow}
	cases := []struct {
		err  error
		want ErrorCode
	}{
		{fmt.Errorf("api: %w", overflow), ErrAPIContextLength},
		{&api.ToolArgsInvalidError{Tool: "bash"}, ErrToolArgsInvalid},
		{&Stall{Stage: "call model qwen3.6-27b", Idle: 33 * time.Second}, ErrEngineStall},
		{&api.StatusError{Status: 429, Msg: "x"}, ErrAPIRateLimit},
		{&api.StatusError{Status: 401, Msg: "x"}, ErrAPIAuth},
		{&api.StatusError{Status: 400, Msg: "max_tokens too large"}, ErrAPIBadRequest},
		{&api.StatusError{Status: 502, Msg: "x"}, ErrAPIServerError},
		{errors.New("read tcp: connection reset by peer"), ErrAPIStreamBroken},
		{errors.New("dial tcp 127.0.0.1:1234: connection refused"), ErrAPIUnreachable},
		{context.DeadlineExceeded, ErrAPITimeout},
		{errors.New("something odd"), ""},
	}
	for _, c := range cases {
		if got, _ := Classify(c.err, Context{}); got != c.want {
			t.Errorf("Classify(%v) = %q, want %q", c.err, got, c.want)
		}
	}
	if code, _ := Classify(nil, Context{}); code != "" {
		t.Errorf("nil error coded %q", code)
	}
}

// Report records one event carrying the code, the call's context and the
// error text, at the code's severity.
func TestReportRecordsACodedEvent(t *testing.T) {
	resetRuntimeEvents(t)
	err := fmt.Errorf("api: %w", &api.StatusError{Status: 400, Msg: llamaOverflow})
	ev := ReportError(err, Context{Provider: "openai-compatible", Model: "qwen3.6-27b"})
	if ev.Code != ErrAPIContextLength || ev.Model != "qwen3.6-27b" || ev.Provider != "openai-compatible" {
		t.Fatalf("event = %+v", ev)
	}
	if ev.Severity != SevError || ev.Category != CatAPI || ev.Source != "report" {
		t.Errorf("severity/category/source = %v/%v/%q", ev.Severity, ev.Category, ev.Source)
	}
	if !strings.Contains(ev.Message, "16384") {
		t.Errorf("message lost the error text: %q", ev.Message)
	}
	got := RecentRuntime()
	if len(got) != 1 || got[0].Code != ErrAPIContextLength {
		t.Fatalf("recorded = %+v", got)
	}
}

// An error of no known kind is still recorded, uncoded, as before.
func TestReportRecordsUnknownErrorsUncoded(t *testing.T) {
	resetRuntimeEvents(t)
	ev := ReportError(errors.New("something odd"), Context{Tool: "bash"})
	if ev.Code != "" || ev.Category != CatTool || ev.Severity != SevError {
		t.Errorf("event = %+v", ev)
	}
	if ReportError(nil, Context{}) != (RuntimeEvent{}) {
		t.Error("nil error produced an event")
	}
}

// The log sink still records Warn/Error lines, uncoded and marked as coming
// from the log, so they can be told apart from structured reports.
func TestRecordRuntimeNoLongerGuessesCodesFromText(t *testing.T) {
	resetRuntimeEvents(t)
	RecordRuntime(SevError, CatAPI, "模型调用失败: 请求参数错误 400")
	got := RecentRuntime()
	if len(got) != 1 || got[0].Code != "" {
		t.Fatalf("free text was coded: %+v", got)
	}
	if got[0].Source != "log" {
		t.Errorf("source = %q, want log", got[0].Source)
	}
}

// Aggregation: coded events group by code and model whatever their token
// counts say; uncoded ones by their text with numbers and paths blanked;
// recovered events attach to the coded entry they fixed.
func TestSummarizeRuntimeGroupsByCodeAndModel(t *testing.T) {
	resetRuntimeEvents(t)
	base := time.Date(2026, 9, 27, 13, 17, 0, 0, time.Local)
	events := []RuntimeEvent{
		{Time: base, Severity: SevError, Category: CatAPI, Code: ErrAPIContextLength, Model: "qwen", Message: "request (16569 tokens) exceeds", Source: "report"},
		{Time: base.Add(time.Minute), Severity: SevError, Category: CatAPI, Code: ErrAPIContextLength, Model: "qwen", Message: "request (17964 tokens) exceeds", Source: "report"},
		{Time: base.Add(2 * time.Minute), Severity: SevRecovered, Category: CatAPI, Code: ErrAPIContextLength, Model: "qwen", Message: "窗口 32000 → 16384", Source: "remedy"},
		{Time: base, Severity: SevError, Category: CatAPI, Code: ErrAPIContextLength, Model: "other", Message: "x", Source: "report"},
		{Time: base, Severity: SevWarning, Category: CatEngine, Message: "loop detected (layer 2): read D:\\a\\b.go 3 times", Source: "log"},
		{Time: base, Severity: SevWarning, Category: CatEngine, Message: "loop detected (layer 2): read D:\\c\\d.go 7 times", Source: "log"},
	}
	sums := SummarizeRuntime(events)
	if len(sums) != 3 {
		t.Fatalf("got %d summaries: %+v", len(sums), sums)
	}
	first := sums[0]
	if first.Code != ErrAPIContextLength || first.Model != "qwen" || first.Count != 2 {
		t.Errorf("first summary = %+v", first)
	}
	if !first.Last.Equal(base.Add(time.Minute)) {
		t.Errorf("Last = %v", first.Last)
	}
	if len(first.Applied) != 1 || first.Applied[0] != "窗口 32000 → 16384" {
		t.Errorf("Applied = %v", first.Applied)
	}
	if first.Recovery == "" || first.Message != Lookup(ErrAPIContextLength).Message {
		t.Errorf("summary lacks the catalogue's title/recovery: %+v", first)
	}
	var loops *RuntimeSummary
	for i := range sums {
		if sums[i].Code == "" {
			loops = &sums[i]
		}
	}
	if loops == nil || loops.Count != 2 {
		t.Fatalf("uncoded events not merged by normalised text: %+v", sums)
	}
	if strings.Contains(loops.Message, "D:\\") || strings.Contains(loops.Message, "7") {
		t.Errorf("normalised message still carries a path or number: %q", loops.Message)
	}
}

// The catalogue no longer claims fixes it cannot make.
func TestCatalogueHasNoAutoFixFlags(t *testing.T) {
	for code, def := range AllErrors() {
		if strings.Contains(def.Recovery, "已自动调整") || strings.Contains(def.Recovery, "已自动切换") {
			t.Errorf("%s still promises an automatic fix: %q", code, def.Recovery)
		}
	}
	for _, code := range []ErrorCode{ErrAPIContextLength, ErrAPIProviderUnavailable, ErrToolArgsInvalid, ErrEngineStall} {
		if Lookup(code) == nil || Lookup(code).Recovery == "" {
			t.Errorf("%s missing or without recovery hint", code)
		}
	}
}

// The start-up hint lists coded ERROR+ problems with a hint; a remedied
// code stays (remedies hold for one session only), while an uncoded line
// and a warning do not count. Most recent first, with count and last time.
func TestUnresolvedListsCodedProblemsMostRecentFirst(t *testing.T) {
	now := time.Date(2026, 10, 21, 18, 0, 0, 0, time.Local)
	at := func(h int) time.Time { return now.Add(-time.Duration(h) * time.Hour) }
	events := []RuntimeEvent{
		{Time: at(5), Severity: SevError, Code: ErrAPIContextLength, Model: "a", Source: "report"},
		{Time: at(5), Severity: SevRecovered, Code: ErrAPIContextLength, Model: "a", Source: "remedy"},
		{Time: at(3), Severity: SevError, Code: ErrAPIProviderUnavailable, Model: "a", Source: "report"},
		{Time: at(2), Severity: SevError, Code: ErrAPIProviderUnavailable, Model: "a", Source: "report"},
		{Time: at(1), Severity: SevWarning, Code: ErrToolArgsInvalid, Model: "a", Source: "report"},
		{Time: at(1), Severity: SevError, Message: "loop detected", Source: "log"},
	}
	got := unresolvedIn(events, now, nil)
	if len(got) != 2 {
		t.Fatalf("unresolved = %+v, want E2009 then E2008", got)
	}
	if got[0].Code != ErrAPIProviderUnavailable || got[0].Count != 2 || !got[0].Last.Equal(at(2)) || got[0].Recovery == "" {
		t.Errorf("first = %+v", got[0])
	}
	if got[1].Code != ErrAPIContextLength || got[1].Count != 1 {
		t.Errorf("second = %+v", got[1])
	}
}

// Three sources of stale hints: a problem older than a week, one recorded
// before the person last viewed /diagnose errors, and one on a model that
// has completed a call since. Each used to be announced at every start.
func TestUnresolvedSkipsOldViewedAndSupersededProblems(t *testing.T) {
	now := time.Date(2026, 10, 21, 18, 0, 0, 0, time.Local)
	at := func(h int) time.Time { return now.Add(-time.Duration(h) * time.Hour) }
	events := []RuntimeEvent{
		// Eight days old: outside the window.
		{Time: at(8 * 24), Severity: SevError, Code: ErrAPIProviderUnavailable, Model: "qwen-test", Source: "report"},
		// Viewed at -4h: only what comes after counts.
		{Time: at(6), Severity: SevError, Code: ErrAPIContextLength, Model: "a", Source: "report"},
		{Time: at(4), Severity: SevInfo, Message: "viewed /diagnose errors", Source: sourceAck},
		{Time: at(3), Severity: SevError, Code: ErrAPIContextLength, Model: "a", Source: "report"},
		// Model b succeeded at -1h, after its failure at -2h.
		{Time: at(2), Severity: SevError, Code: ErrAPIProviderUnavailable, Model: "b", Source: "report"},
		// Model c succeeded at -3h, before its failure at -1h: still open.
		{Time: at(1), Severity: SevError, Code: ErrAPIProviderUnavailable, Model: "c", Source: "report"},
	}
	lastOK := map[string]time.Time{"b": at(1), "c": at(3)}
	got := unresolvedIn(events, now, lastOK)
	if len(got) != 2 {
		t.Fatalf("unresolved = %+v, want c's E2009 and a's E2008", got)
	}
	if got[0].Model != "c" || got[1].Model != "a" || got[1].Count != 1 {
		t.Errorf("got %+v", got)
	}
	if n := len(unresolvedIn(events[:2], now, nil)); n != 1 {
		t.Errorf("without the ack, the -6h problem must count: %d", n)
	}
}

// The ack marker is bookkeeping, not a problem: it neither shows as a
// summary line nor counts toward the event total.
func TestAckMarkerIsNeitherSummarisedNorCounted(t *testing.T) {
	events := []RuntimeEvent{
		{Time: time.Now(), Severity: SevError, Message: "loop detected", Source: "log"},
		{Time: time.Now(), Severity: SevInfo, Message: "viewed /diagnose errors", Source: sourceAck},
	}
	if s := SummarizeRuntime(events); len(s) != 1 || s[0].Message != "loop detected" {
		t.Errorf("summaries = %+v", s)
	}
	if v := WithoutAcks(events); len(v) != 1 || v[0].Source != "log" {
		t.Errorf("WithoutAcks = %+v", v)
	}
}

// Recovery hints recorded at run time describe what the user can do now;
// "will retry automatically" is false once the turn has already failed.
func TestRuntimeRecoveryHintsDoNotPromiseRetries(t *testing.T) {
	for _, code := range []ErrorCode{ErrAPITimeout, ErrAPIRateLimit, ErrAPIServerError, ErrAPIStreamBroken, ErrAPIUnreachable} {
		if def := Lookup(code); def == nil || strings.Contains(def.Recovery, "自动重试") {
			t.Errorf("%s recovery promises an automatic retry: %+v", code, def)
		}
	}
}

// Blanking numbers and paths must not eat the Chinese words after a path or
// merge distinct HTTP statuses.
func TestNormaliseMessageKeepsMeaning(t *testing.T) {
	got := normaliseMessage("写入 /x/a.json失败：磁盘已满")
	if !strings.Contains(got, "失败：磁盘已满") || strings.Contains(got, "a.json") {
		t.Errorf("path blanking ate the sentence or kept the path: %q", got)
	}
	if normaliseMessage("HTTP 404 from server") == normaliseMessage("HTTP 500 from server") {
		t.Error("distinct HTTP statuses merged")
	}
	if normaliseMessage(`read D:\a\b.go 3 times`) != normaliseMessage(`read D:\c\d.go 7 times`) {
		t.Error("same problem with different path and count not merged")
	}
	if normaliseMessage("/diagnose failed") == normaliseMessage("# failed") {
		t.Error("a slash command was blanked as a path")
	}
}

// A group whose first event is a remedy result still takes the severity of
// the problems that follow, instead of showing as FIXED.
func TestSummarySeverityIgnoresRecoveredEvents(t *testing.T) {
	events := []RuntimeEvent{
		{Severity: SevRecovered, Code: ErrAPIContextLength, Model: "m", Message: "窗口 32000 → 16384", Source: "remedy"},
		{Severity: SevError, Code: ErrAPIContextLength, Model: "m", Message: "overflow", Source: "report"},
	}
	sums := SummarizeRuntime(events)
	if len(sums) != 1 || sums[0].Severity != SevError {
		t.Fatalf("summaries = %+v", sums)
	}
}

// /diagnose errors shows this session's events and the log's, once each.
func TestMergeEventsDeduplicates(t *testing.T) {
	at := time.Date(2026, 9, 27, 13, 0, 0, 0, time.Local)
	shared := RuntimeEvent{Time: at, Severity: SevError, Message: "same", Source: "report"}
	logged := []RuntimeEvent{{Time: at.Add(-time.Hour), Severity: SevError, Message: "old", Source: "report"}, shared}
	inMemory := []RuntimeEvent{shared, {Time: at.Add(time.Minute), Severity: SevWarning, Message: "new", Source: "log"}}
	got := MergeEvents(logged, inMemory)
	if len(got) != 3 {
		t.Fatalf("merged = %d events, want 3: %+v", len(got), got)
	}
	if got[0].Message != "old" || got[2].Message != "new" {
		t.Errorf("not in time order: %+v", got)
	}
}
