package cost

import (
	"encoding/csv"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/liuzhixin405/cove-agent/internal/textutil"
)

// UsageEntry is one billed model call in the usage ledger: who served it,
// what it consumed and what it cost. The ledger is ~/.cove/usage/YYYY-MM.jsonl,
// one file per month, so a month's spend by provider and model is a read of
// one file. cost_history.json holds one total per session under the main
// model's name, written at exit: it cannot say what a month cost on Bedrock
// versus the fast model, and a killed process never wrote it.
type UsageEntry struct {
	Time       time.Time `json:"t"`
	Provider   string    `json:"provider"`
	Model      string    `json:"model"`
	Session    string    `json:"session,omitempty"`
	Source     string    `json:"source,omitempty"` // "session", "dream", "legacy"
	Input      int       `json:"in"`
	Output     int       `json:"out"`
	CacheHit   int       `json:"cache_hit,omitempty"`
	CacheMiss  int       `json:"cache_miss,omitempty"`
	CacheWrite int       `json:"cache_write,omitempty"`
	Cost       float64   `json:"cost"`
}

// Bill prices one call at model's rate the way the Tracker does: the
// cache counts are clamped to the input total (a provider may report a
// hit count without a miss count, or counts that exceed the input), cache
// writes are billed at the write rate instead of the input rate, and the
// rest at the input, cache-read and output rates. The Tracker's session
// total and the ledger's per-call cost come from this one place so they
// agree to the cent.
func Bill(model string, input, output, cacheHit, cacheMiss, cacheWrite int) UsageEntry {
	if cacheHit < 0 {
		cacheHit = 0
	}
	if cacheMiss < 0 {
		cacheMiss = 0
	}
	if cacheHit > input {
		cacheHit = input
	}
	if cacheHit+cacheMiss > input {
		cacheMiss = input - cacheHit
	}
	if cacheHit+cacheMiss < input {
		cacheMiss += input - (cacheHit + cacheMiss)
	}
	p := priceFor(model)
	if p.InputCacheHit == 0 {
		p.InputCacheHit = p.Input
	}
	if p.InputCacheWrite == 0 {
		p.InputCacheWrite = p.Input * cacheWriteMultiplier
	}
	if cacheWrite < 0 {
		cacheWrite = 0
	}
	if cacheWrite > cacheMiss {
		cacheWrite = cacheMiss
	}
	cost := (float64(cacheMiss-cacheWrite)/1e6)*p.Input + (float64(cacheWrite)/1e6)*p.InputCacheWrite +
		(float64(cacheHit)/1e6)*p.InputCacheHit + (float64(output)/1e6)*p.Output
	return UsageEntry{Model: model, Input: input, Output: output, CacheHit: cacheHit, CacheMiss: cacheMiss, CacheWrite: cacheWrite, Cost: cost}
}

// ledgerKeepMonths is how many month files the ledger keeps; older ones
// are removed the first time the process records a call.
const ledgerKeepMonths = 13

var (
	ledgerMu        sync.Mutex
	ledgerDir       string // "-" disables the ledger
	ledgerDirOnce   sync.Once
	ledgerPrune     sync.Once
	monthFileRe     = regexp.MustCompile(`^(\d{4})-(\d{2})\.jsonl$`)
	monthFileLayout = "2006-01"
)

// SetLedgerDir points the ledger at dir ("" disables it). Tests use it; the
// default is ~/.cove/usage, and nothing under `go test` unless set.
func SetLedgerDir(dir string) {
	ledgerMu.Lock()
	defer ledgerMu.Unlock()
	ledgerDirOnce.Do(func() {})
	if dir == "" {
		dir = "-"
	}
	ledgerDir = dir
}

func usageDir() string {
	ledgerDirOnce.Do(func() {
		ledgerDir = "-"
		if testing.Testing() {
			return
		}
		home, err := os.UserHomeDir()
		if err != nil || home == "" {
			return
		}
		ledgerDir = filepath.Join(home, ".cove", "usage")
	})
	return ledgerDir
}

func monthFile(dir string, t time.Time) string {
	return filepath.Join(dir, t.Local().Format(monthFileLayout)+".jsonl")
}

// RecordUsage appends e to the month file of its time (now when unset).
// It never fails loudly: a ledger that cannot be written is dropped, like
// the trace.
func RecordUsage(e UsageEntry) {
	if e.Time.IsZero() {
		e.Time = time.Now()
	}
	ledgerMu.Lock()
	defer ledgerMu.Unlock()
	dir := usageDir()
	if dir == "-" {
		return
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return
	}
	ledgerPrune.Do(func() { pruneLedger(dir, e.Time) })
	line, err := json.Marshal(e)
	if err != nil {
		return
	}
	f, err := os.OpenFile(monthFile(dir, e.Time), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return
	}
	_, _ = f.Write(append(line, '\n'))
	_ = f.Close()
}

// pruneLedger removes month files older than ledgerKeepMonths before now.
func pruneLedger(dir string, now time.Time) {
	keepFrom := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, now.Location()).AddDate(0, -(ledgerKeepMonths - 1), 0)
	for _, name := range monthFiles(dir) {
		if m, ok := monthOf(name); ok && m.Before(keepFrom) {
			_ = os.Remove(filepath.Join(dir, name))
		}
	}
}

// monthFiles lists the ledger's month files, oldest first.
func monthFiles(dir string) []string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var names []string
	for _, e := range entries {
		if !e.IsDir() && monthFileRe.MatchString(e.Name()) {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	return names
}

// monthOf is the first instant of the month a ledger file name covers.
func monthOf(name string) (time.Time, bool) {
	m := monthFileRe.FindStringSubmatch(name)
	if m == nil {
		return time.Time{}, false
	}
	t, err := time.ParseInLocation(monthFileLayout, m[1]+"-"+m[2], time.Local)
	return t, err == nil
}

// LoadUsage returns the ledger's entries with from <= Time < to, oldest
// first. A zero from means the beginning of the ledger.
func LoadUsage(from, to time.Time) []UsageEntry {
	ledgerMu.Lock()
	dir := usageDir()
	ledgerMu.Unlock()
	if dir == "-" {
		return nil
	}
	var out []UsageEntry
	for _, name := range monthFiles(dir) {
		m, ok := monthOf(name)
		if !ok || !m.Before(to) || (!from.IsZero() && m.AddDate(0, 1, 0).Before(from)) {
			continue
		}
		out = append(out, readUsageFile(filepath.Join(dir, name), from, to)...)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Time.Before(out[j].Time) })
	return out
}

func readUsageFile(path string, from, to time.Time) []UsageEntry {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var out []UsageEntry
	for _, line := range strings.Split(string(data), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var e UsageEntry
		if json.Unmarshal([]byte(line), &e) != nil || e.Time.IsZero() {
			continue
		}
		if e.Time.Before(from) || !e.Time.Before(to) {
			continue
		}
		out = append(out, e)
	}
	return out
}

// LedgerStart is the time of the ledger's first entry; zero when it is
// empty. Session totals from cost_history.json count only before it, so
// nothing is billed twice once per-call recording began.
func LedgerStart() time.Time {
	ledgerMu.Lock()
	dir := usageDir()
	ledgerMu.Unlock()
	if dir == "-" {
		return time.Time{}
	}
	for _, name := range monthFiles(dir) {
		if es := readUsageFile(filepath.Join(dir, name), time.Time{}, time.Date(9999, 1, 1, 0, 0, 0, 0, time.UTC)); len(es) > 0 {
			return es[0].Time
		}
	}
	return time.Time{}
}

// legacyEntries turns the per-session records of cost_history.json that
// predate the ledger into entries, so /cost all and the months before
// per-call recording still add up. They carry the session's main model and
// no provider: "legacy" says so.
func legacyEntries(records []CostRecord, before time.Time) []UsageEntry {
	var out []UsageEntry
	for _, r := range records {
		if !before.IsZero() && !r.Timestamp.Before(before) {
			continue
		}
		out = append(out, UsageEntry{Time: r.Timestamp, Provider: "legacy", Model: r.Model, Session: r.SessionID, Source: "legacy",
			Input: r.Input, Output: r.Output, CacheHit: r.CacheHit, CacheMiss: r.CacheMiss, Cost: r.Cost})
	}
	return out
}

// Period is the span a usage query covers, [From, To).
type Period struct {
	From, To time.Time
	Label    string
}

// ParsePeriod reads a /cost period argument: "" or "month" for the current
// calendar month, "2026-09" for that month, "30d" / "24h" for the last so
// many days or hours, "all" for everything.
func ParsePeriod(arg string, now time.Time) (Period, error) {
	arg = strings.ToLower(strings.TrimSpace(arg))
	monthStart := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, now.Location())
	switch {
	case arg == "" || arg == "month":
		return Period{From: monthStart, To: now.Add(time.Second), Label: monthStart.Format("2006-01")}, nil
	case arg == "all":
		return Period{To: now.Add(time.Second), Label: "全部"}, nil
	case arg == "today":
		d := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
		return Period{From: d, To: now.Add(time.Second), Label: "今天"}, nil
	}
	if t, err := time.ParseInLocation("2006-01", arg, now.Location()); err == nil {
		return Period{From: t, To: t.AddDate(0, 1, 0), Label: t.Format("2006-01")}, nil
	}
	if n, err := strconv.Atoi(strings.TrimSuffix(arg, "d")); err == nil && strings.HasSuffix(arg, "d") && n > 0 {
		return Period{From: now.AddDate(0, 0, -n), To: now.Add(time.Second), Label: fmt.Sprintf("近 %d 天", n)}, nil
	}
	if n, err := strconv.Atoi(strings.TrimSuffix(arg, "h")); err == nil && strings.HasSuffix(arg, "h") && n > 0 {
		return Period{From: now.Add(-time.Duration(n) * time.Hour), To: now.Add(time.Second), Label: fmt.Sprintf("近 %d 小时", n)}, nil
	}
	return Period{}, fmt.Errorf("无法识别的时间范围 %q：可用 month、2026-09、30d、24h、today、all", arg)
}

// UsageRow is one provider+model line of a usage report, or its total.
type UsageRow struct {
	Provider, Model string
	Calls           int
	Input, Output   int
	CacheHit        int
	CacheMiss       int
	Cost            float64
}

// Aggregate sums entries by provider and model, highest cost first, and
// returns the grand total.
func Aggregate(entries []UsageEntry) (rows []UsageRow, total UsageRow) {
	by := map[string]*UsageRow{}
	for _, e := range entries {
		k := e.Provider + "\x00" + e.Model
		r, ok := by[k]
		if !ok {
			r = &UsageRow{Provider: e.Provider, Model: e.Model}
			by[k] = r
		}
		for _, x := range []*UsageRow{r, &total} {
			x.Calls++
			x.Input += e.Input
			x.Output += e.Output
			x.CacheHit += e.CacheHit
			x.CacheMiss += e.CacheMiss
			x.Cost += e.Cost
		}
	}
	for _, r := range by {
		rows = append(rows, *r)
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Cost != rows[j].Cost {
			return rows[i].Cost > rows[j].Cost
		}
		if rows[i].Provider != rows[j].Provider {
			return rows[i].Provider < rows[j].Provider
		}
		return rows[i].Model < rows[j].Model
	})
	return rows, total
}

// fmtTokens writes a token count the way a person reads one: 9,999 stays
// exact, then k and M with one or two decimals.
func fmtTokens(n int) string {
	switch {
	case n < 10_000:
		return strconv.Itoa(n)
	case n < 1_000_000:
		return strconv.FormatFloat(float64(n)/1e3, 'f', 1, 64) + "k"
	default:
		return strconv.FormatFloat(float64(n)/1e6, 'f', 2, 64) + "M"
	}
}

// pad right-pads s to width display cells (textutil.Width, so CJK headers
// line up with ASCII values).
func pad(s string, width int) string {
	if w := textutil.Width(s); w < width {
		return s + strings.Repeat(" ", width-w)
	}
	return s
}

func lpad(s string, width int) string {
	if w := textutil.Width(s); w < width {
		return strings.Repeat(" ", width-w) + s
	}
	return s
}

// FormatUsage renders rows as a table under a heading for p.
func FormatUsage(p Period, rows []UsageRow, total UsageRow) string {
	var sb strings.Builder
	span := ""
	if !p.From.IsZero() {
		span = fmt.Sprintf("（%s 至 %s）", p.From.Format("2006-01-02"), p.To.Add(-time.Second).Format("2006-01-02"))
	}
	fmt.Fprintf(&sb, "\x1b[1m用量统计 · %s\x1b[0m%s\n", p.Label, span)
	if len(rows) == 0 {
		sb.WriteString("  这段时间没有记录到模型调用。\n")
		return sb.String()
	}
	pw, mw := textutil.Width("供应商"), textutil.Width("模型")
	for _, r := range rows {
		if w := textutil.Width(r.Provider); w > pw {
			pw = w
		}
		if w := textutil.Width(r.Model); w > mw {
			mw = w
		}
	}
	line := func(prov, model, calls, in, out, hit, cost string) {
		fmt.Fprintf(&sb, "  %s  %s  %s  %s  %s  %s  %s\n", pad(prov, pw), pad(model, mw), lpad(calls, 6), lpad(in, 8), lpad(out, 8), lpad(hit, 8), lpad(cost, 10))
	}
	sb.WriteString("\x1b[2m")
	line("供应商", "模型", "调用", "输入", "输出", "缓存命中", "费用")
	sb.WriteString("\x1b[0m")
	for _, r := range rows {
		line(r.Provider, r.Model, strconv.Itoa(r.Calls), fmtTokens(r.Input), fmtTokens(r.Output), fmtTokens(r.CacheHit), "$"+ftoa(r.Cost))
	}
	sb.WriteString("\x1b[1m")
	line("合计", "", strconv.Itoa(total.Calls), fmtTokens(total.Input), fmtTokens(total.Output), fmtTokens(total.CacheHit), "$"+ftoa(total.Cost))
	sb.WriteString("\x1b[0m")
	return sb.String()
}

// ExportUsageCSV writes entries to path, one row per call, for a
// spreadsheet.
func ExportUsageCSV(entries []UsageEntry, path string) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	w := csv.NewWriter(f)
	_ = w.Write([]string{"time", "provider", "model", "session", "source", "input", "output", "cache_hit", "cache_miss", "cache_write", "cost_usd"})
	for _, e := range entries {
		_ = w.Write([]string{e.Time.Format(time.RFC3339), e.Provider, e.Model, e.Session, e.Source,
			strconv.Itoa(e.Input), strconv.Itoa(e.Output), strconv.Itoa(e.CacheHit), strconv.Itoa(e.CacheMiss), strconv.Itoa(e.CacheWrite),
			strconv.FormatFloat(e.Cost, 'f', 6, 64)})
	}
	w.Flush()
	return w.Error()
}

// RenderUsage is the body of /cost after the session line: the ledger's
// entries for the period in args (default: this month), plus the
// per-session records from before the ledger began, by provider and
// model. An "export" argument also writes the period's calls to a CSV
// under the ledger directory and says where.
func RenderUsage(args []string, now time.Time) (string, error) {
	return renderUsage(args, now, NewCostHistory().Records)
}

func renderUsage(args []string, now time.Time, legacy []CostRecord) (string, error) {
	export := false
	period := ""
	for _, a := range args {
		switch strings.ToLower(a) {
		case "export", "csv":
			export = true
		default:
			if period != "" {
				return "", fmt.Errorf("只能给一个时间范围，收到 %q 和 %q", period, a)
			}
			period = a
		}
	}
	p, err := ParsePeriod(period, now)
	if err != nil {
		return "", err
	}
	entries := LoadUsage(p.From, p.To)
	start := LedgerStart()
	for _, e := range legacyEntries(legacy, start) {
		if !e.Time.Before(p.From) && e.Time.Before(p.To) {
			entries = append(entries, e)
		}
	}
	sort.SliceStable(entries, func(i, j int) bool { return entries[i].Time.Before(entries[j].Time) })
	rows, total := Aggregate(entries)
	out := FormatUsage(p, rows, total)
	if !start.IsZero() {
		out += fmt.Sprintf("\x1b[2m逐次账本自 %s 起（~/.cove/usage/）；更早只有会话级记录，供应商标为 legacy。可用 /cost 2026-09、/cost 30d、/cost all、/cost export。\x1b[0m\n", start.Format("2006-01-02"))
	}
	if export {
		dir := usageDir()
		if dir == "-" {
			return out, fmt.Errorf("账本目录不可用，无法导出")
		}
		path := filepath.Join(dir, "export-"+strings.NewReplacer(" ", "", "/", "-").Replace(p.Label)+"-"+now.Format("20060102-150405")+".csv")
		if err := ExportUsageCSV(entries, path); err != nil {
			return out, fmt.Errorf("导出失败: %w", err)
		}
		out += fmt.Sprintf("\x1b[32m✓ 已导出 %d 条到 %s\x1b[0m\n", len(entries), path)
	}
	return out, nil
}
