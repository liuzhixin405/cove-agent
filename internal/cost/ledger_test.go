package cost

import (
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func useLedger(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	SetLedgerDir(dir)
	t.Cleanup(func() { SetLedgerDir("") })
	return dir
}

// The ledger's per-call cost and the Tracker's session total are the same
// arithmetic; if they drift, /cost shows a session that does not add up to
// its own calls.
func TestBillAgreesWithTracker(t *testing.T) {
	cases := []struct{ in, out, hit, miss, write int }{
		{1000, 200, 0, 0, 0},
		{10000, 500, 8000, 2000, 500},
		{10000, 500, 12000, 0, 0}, // hit over input: clamped
		{10000, 500, 3000, 0, 0},  // miss unreported: the rest
	}
	for _, c := range cases {
		for _, model := range []string{"claude-opus-5-5", "deepseek-v4-pro", "unknown-model"} {
			tr := NewTracker(0)
			tr.AddWithCacheWrite(model, c.in, c.out, c.hit, c.miss, c.write)
			u := Bill(model, c.in, c.out, c.hit, c.miss, c.write)
			tot := tr.Totals()
			if math.Abs(tot.Cost-u.Cost) > 1e-12 || tot.PromptCacheHit != u.CacheHit || tot.PromptCacheMiss != u.CacheMiss {
				t.Errorf("%s %+v: tracker %+v, bill %+v", model, c, tot, u)
			}
		}
	}
}

// One file per month, named by the entry's local month; a month query
// reads only its file's entries and LedgerStart is the earliest entry.
func TestRecordUsageWritesMonthFilesAndLoadUsageFiltersByPeriod(t *testing.T) {
	dir := useLedger(t)
	sep := time.Date(2026, 9, 15, 12, 0, 0, 0, time.Local)
	oct := time.Date(2026, 10, 3, 9, 0, 0, 0, time.Local)
	RecordUsage(UsageEntry{Time: oct, Provider: "bedrock", Model: "m", Input: 10, Output: 1, Cost: 0.01})
	RecordUsage(UsageEntry{Time: sep, Provider: "anthropic", Model: "m", Input: 20, Output: 2, Cost: 0.02})
	RecordUsage(UsageEntry{Time: oct.Add(time.Hour), Provider: "bedrock", Model: "fast", Input: 5, Output: 1, Cost: 0.001})

	for _, name := range []string{"2026-09.jsonl", "2026-10.jsonl"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Errorf("missing %s: %v", name, err)
		}
	}
	p, _ := ParsePeriod("2026-10", oct)
	got := LoadUsage(p.From, p.To)
	if len(got) != 2 || got[0].Model != "m" || got[1].Model != "fast" {
		t.Fatalf("October = %+v", got)
	}
	if all := LoadUsage(time.Time{}, oct.AddDate(0, 1, 0)); len(all) != 3 || all[0].Provider != "anthropic" {
		t.Fatalf("all = %+v", all)
	}
	if s := LedgerStart(); !s.Equal(sep) {
		t.Fatalf("LedgerStart = %v, want %v", s, sep)
	}
}

func TestParsePeriod(t *testing.T) {
	now := time.Date(2026, 10, 21, 18, 30, 0, 0, time.Local)
	cases := []struct {
		arg        string
		from       time.Time
		label      string
		wantErr    bool
		openEnded  bool
		toIsMonth1 bool
	}{
		{arg: "", from: time.Date(2026, 10, 1, 0, 0, 0, 0, time.Local), label: "2026-10", openEnded: true},
		{arg: "month", from: time.Date(2026, 10, 1, 0, 0, 0, 0, time.Local), label: "2026-10", openEnded: true},
		{arg: "2026-09", from: time.Date(2026, 9, 1, 0, 0, 0, 0, time.Local), label: "2026-09", toIsMonth1: true},
		{arg: "30d", from: now.AddDate(0, 0, -30), label: "近 30 天", openEnded: true},
		{arg: "24h", from: now.Add(-24 * time.Hour), label: "近 24 小时", openEnded: true},
		{arg: "today", from: time.Date(2026, 10, 21, 0, 0, 0, 0, time.Local), label: "今天", openEnded: true},
		{arg: "all", label: "全部", openEnded: true},
		{arg: "yesterday", wantErr: true},
		{arg: "0d", wantErr: true},
	}
	for _, c := range cases {
		p, err := ParsePeriod(c.arg, now)
		if (err != nil) != c.wantErr {
			t.Errorf("%q: err = %v", c.arg, err)
			continue
		}
		if c.wantErr {
			continue
		}
		if !p.From.Equal(c.from) || p.Label != c.label {
			t.Errorf("%q: from %v label %q, want %v %q", c.arg, p.From, p.Label, c.from, c.label)
		}
		if c.openEnded && !p.To.After(now) {
			t.Errorf("%q: To %v must include now", c.arg, p.To)
		}
		if c.toIsMonth1 && !p.To.Equal(time.Date(2026, 10, 1, 0, 0, 0, 0, time.Local)) {
			t.Errorf("%q: To = %v", c.arg, p.To)
		}
	}
}

// Rows are per provider and model, so the same model via two providers is
// two lines; the total covers every entry; the costliest row leads.
func TestAggregateGroupsByProviderAndModel(t *testing.T) {
	entries := []UsageEntry{
		{Provider: "bedrock", Model: "claude", Input: 100, Output: 10, CacheHit: 50, Cost: 0.5},
		{Provider: "anthropic", Model: "claude", Input: 100, Output: 10, Cost: 0.7},
		{Provider: "bedrock", Model: "claude", Input: 100, Output: 10, Cost: 0.4},
		{Provider: "bedrock", Model: "haiku", Input: 1, Output: 1, Cost: 0.001},
	}
	rows, total := Aggregate(entries)
	if len(rows) != 3 {
		t.Fatalf("rows = %+v", rows)
	}
	if rows[0].Provider != "bedrock" || rows[0].Model != "claude" || rows[0].Calls != 2 || rows[0].Input != 200 || rows[0].CacheHit != 50 || math.Abs(rows[0].Cost-0.9) > 1e-9 {
		t.Errorf("first row = %+v", rows[0])
	}
	if rows[1].Provider != "anthropic" || rows[2].Model != "haiku" {
		t.Errorf("order = %+v", rows)
	}
	if total.Calls != 4 || total.Input != 301 || math.Abs(total.Cost-1.601) > 1e-9 {
		t.Errorf("total = %+v", total)
	}
}

// Session-level records from cost_history.json fill in the months before
// the ledger existed and stop at its first entry, so a session that both
// wrote its total at exit and had its calls logged is not billed twice.
func TestRenderUsageAddsLegacyRecordsOnlyBeforeTheLedgerBegan(t *testing.T) {
	useLedger(t)
	now := time.Date(2026, 10, 21, 18, 0, 0, 0, time.Local)
	RecordUsage(UsageEntry{Time: now.Add(-2 * time.Hour), Provider: "bedrock", Model: "claude", Input: 1000, Output: 100, Cost: 0.3})
	legacy := []CostRecord{
		{SessionID: "old", Model: "deepseek", Input: 5000, Output: 500, Cost: 0.2, Timestamp: now.AddDate(0, 0, -3)},
		{SessionID: "same-day", Model: "claude", Input: 1000, Output: 100, Cost: 0.3, Timestamp: now.Add(-time.Hour)}, // after the ledger began
		{SessionID: "last-month", Model: "qwen", Input: 10, Output: 1, Cost: 0.001, Timestamp: now.AddDate(0, -1, 0)},
	}
	out, err := renderUsage(nil, now, legacy)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"用量统计 · 2026-10", "bedrock", "claude", "legacy", "deepseek", "合计", "$0.50", "逐次账本自 2026-10-21 起"} {
		if !strings.Contains(out, want) {
			t.Errorf("report lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "qwen") || strings.Count(out, "claude") != 1 {
		t.Errorf("last month's record or the double-counted session leaked in:\n%s", out)
	}
	all, _ := renderUsage([]string{"all"}, now, legacy)
	if !strings.Contains(all, "qwen") || !strings.Contains(all, "全部") {
		t.Errorf("/cost all lacks last month's legacy record:\n%s", all)
	}
	if _, err := renderUsage([]string{"30d", "7d"}, now, nil); err == nil {
		t.Error("two periods must be refused")
	}
}

func TestRenderUsageExportWritesCSV(t *testing.T) {
	dir := useLedger(t)
	now := time.Date(2026, 10, 21, 18, 0, 0, 0, time.Local)
	RecordUsage(UsageEntry{Time: now.Add(-time.Hour), Provider: "bedrock", Model: "claude", Session: "s1", Source: "session", Input: 1000, Output: 100, Cost: 0.3})
	out, err := renderUsage([]string{"export"}, now, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "已导出 1 条") {
		t.Fatalf("no export line:\n%s", out)
	}
	matches, _ := filepath.Glob(filepath.Join(dir, "export-*.csv"))
	if len(matches) != 1 {
		t.Fatalf("csv files = %v", matches)
	}
	data, _ := os.ReadFile(matches[0])
	if lines := strings.Split(strings.TrimSpace(string(data)), "\n"); len(lines) != 2 || !strings.HasPrefix(lines[0], "time,provider,model") || !strings.Contains(lines[1], "bedrock,claude,s1,session,1000,100") {
		t.Fatalf("csv:\n%s", data)
	}
}

// Thirteen months are kept; the first write of a process removes older
// month files.
func TestPruneRemovesMonthFilesOlderThanThirteenMonths(t *testing.T) {
	dir := useLedger(t)
	for _, name := range []string{"2025-08.jsonl", "2025-09.jsonl", "2025-10.jsonl", "notes.txt"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("{}\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	pruneLedger(dir, time.Date(2026, 10, 21, 0, 0, 0, 0, time.Local))
	for name, want := range map[string]bool{"2025-08.jsonl": false, "2025-09.jsonl": false, "2025-10.jsonl": true, "notes.txt": true} {
		_, err := os.Stat(filepath.Join(dir, name))
		if (err == nil) != want {
			t.Errorf("%s exists=%v, want %v", name, err == nil, want)
		}
	}
}

func TestFmtTokens(t *testing.T) {
	for n, want := range map[int]string{0: "0", 9999: "9999", 10000: "10.0k", 301000: "301.0k", 19076654: "19.08M"} {
		if got := fmtTokens(n); got != want {
			t.Errorf("fmtTokens(%d) = %q, want %q", n, got, want)
		}
	}
}
