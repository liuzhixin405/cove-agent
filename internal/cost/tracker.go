package cost

import (
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/liuzhixin405/cove-agent/internal/fsatomic"
)

type Price struct {
	Input         float64
	InputCacheHit float64
	Output        float64
	// InputCacheWrite is the rate for tokens written into the prompt cache;
	// 0 means cacheWriteMultiplier × Input.
	InputCacheWrite float64
}

// cacheWriteMultiplier is Anthropic's price for a 5-minute prompt-cache write
// relative to plain input.
const cacheWriteMultiplier = 1.25

var Prices = map[string]Price{
	// Current Claude models, per-MTok list prices from Anthropic's model table
	// (as of 2026-06). Cache reads are 0.1x input unless the model publishes its
	// own cache-read rate. Keys are matched as the longest substring of the
	// model ID, so dated or platform-prefixed IDs resolve to the same entry.
	"claude-fable-5-1":  {Input: 10.0, InputCacheHit: 0.25, Output: 50.0},
	"claude-fable-5":    {Input: 10.0, InputCacheHit: 1.0, Output: 50.0},
	"claude-opus-5-5":   {Input: 4.0, InputCacheHit: 0.20, Output: 20.0},
	"claude-opus-5":     {Input: 5.0, InputCacheHit: 0.50, Output: 25.0},
	"claude-opus-4-8":   {Input: 5.0, InputCacheHit: 0.50, Output: 25.0},
	"claude-opus-4-7":   {Input: 5.0, InputCacheHit: 0.50, Output: 25.0},
	"claude-opus-4-6":   {Input: 5.0, InputCacheHit: 0.50, Output: 25.0},
	"claude-sonnet-5":   {Input: 2.0, InputCacheHit: 0.20, Output: 10.0},
	"claude-sonnet-4-6": {Input: 3.0, InputCacheHit: 0.30, Output: 15.0},
	"claude-haiku-4-5":  {Input: 1.0, InputCacheHit: 0.10, Output: 5.0},

	// The Claude 4 / 4.1 / 4.5 generation. These were missing and fell to
	// defaultPrice, under-billing them 7-35x. "claude-opus-4" and
	// "claude-sonnet-4" are prefixes of the newer entries above; the
	// longest-substring match keeps those on their own rates, and catches the
	// dated IDs (claude-opus-4-20250514, claude-sonnet-4-5-20250929).
	"claude-opus-4-5":   {Input: 5.0, InputCacheHit: 0.50, Output: 25.0},
	"claude-opus-4-1":   {Input: 15.0, InputCacheHit: 1.5, Output: 75.0},
	"claude-opus-4":     {Input: 15.0, InputCacheHit: 1.5, Output: 75.0},
	"claude-sonnet-4-5": {Input: 3.0, InputCacheHit: 0.30, Output: 15.0},
	"claude-sonnet-4":   {Input: 3.0, InputCacheHit: 0.30, Output: 15.0},

	"claude-3-7-sonnet": {Input: 3.0, InputCacheHit: 0.30, Output: 15.0},
	"claude-3-5-sonnet": {Input: 3.0, InputCacheHit: 0.30, Output: 15.0},
	"claude-3-5-haiku":  {Input: 0.8, InputCacheHit: 0.08, Output: 4.0},
	"claude-3-opus":     {Input: 15.0, InputCacheHit: 1.5, Output: 75.0},
	"deepseek-chat":     {Input: 0.14, InputCacheHit: 0.14 * 0.1, Output: 0.28},
	"deepseek-reasoner": {Input: 0.14, InputCacheHit: 0.14 * 0.1, Output: 0.28},
	// DeepSeek V4 at the peak rate (api-docs.deepseek.com). Off-peak is
	// exactly half; billing at peak keeps max_budget_usd a real upper bound.
	// "deepseek-v4-flash" is the retired alias of deepseek-flash.
	"deepseek-v4-pro":   {Input: 1.32, InputCacheHit: 0.044, Output: 3.96},
	"deepseek-flash":    {Input: 0.30, InputCacheHit: 0.006, Output: 1.20},
	"deepseek-v4-flash": {Input: 0.30, InputCacheHit: 0.006, Output: 1.20},
	"gpt-4o":            {Input: 2.5, InputCacheHit: 1.25, Output: 10.0},
	"gpt-4o-mini":       {Input: 0.15, InputCacheHit: 0.075, Output: 0.6},
	"o3-mini":           {Input: 1.1, InputCacheHit: 1.1, Output: 4.4},
	// OpenAI list prices (platform.openai.com/docs/pricing). These used to
	// fall to defaultPrice ($0.435/$0.87), an order of magnitude low for
	// gpt-5 and o1, so max_budget_usd was not a bound for them. Longest
	// substring wins, so gpt-5-mini and o4-mini resolve to their own rows.
	"gpt-5":        {Input: 1.25, InputCacheHit: 0.125, Output: 10.0},
	"gpt-5-mini":   {Input: 0.25, InputCacheHit: 0.025, Output: 2.0},
	"gpt-5-nano":   {Input: 0.05, InputCacheHit: 0.005, Output: 0.4},
	"gpt-4.1":      {Input: 2.0, InputCacheHit: 0.5, Output: 8.0},
	"gpt-4.1-mini": {Input: 0.4, InputCacheHit: 0.1, Output: 1.6},
	"gpt-4.1-nano": {Input: 0.1, InputCacheHit: 0.025, Output: 0.4},
	"o1":           {Input: 15.0, InputCacheHit: 7.5, Output: 60.0},
	"o3":           {Input: 2.0, InputCacheHit: 0.5, Output: 8.0},
	"o4-mini":      {Input: 1.1, InputCacheHit: 0.275, Output: 4.4},
}

// defaultPrice is used when a model name does not match any entry in Prices.
// Provides a conservative non-zero fallback so unknown models still produce a
// cost estimate (per-million-token USD rates).
var defaultPrice = Price{Input: 0.435, InputCacheHit: 0.003625, Output: 0.87}

// unknownClaudePrice is the rate for a claude-* model that matches no entry
// in Prices: the most expensive known Claude tier (Opus list price). A new
// Claude ID used to fall to defaultPrice and be billed at a few percent of
// its real cost, so max_budget_usd was no ceiling at all on it. Over-counting
// an unknown model is the safe direction; a wrong-but-low estimate is not.
var unknownClaudePrice = Price{Input: 15.0, InputCacheHit: 1.5, Output: 75.0}

// priceFor is the rate for model: its exact Prices entry, else the longest
// Prices key it contains, else unknownClaudePrice for a Claude model and
// defaultPrice for anything else.
func priceFor(model string) Price {
	if p, ok := Prices[model]; ok {
		return p
	}
	// Dated model names ("gpt-4o-mini-2024-07-18") contain several price
	// keys, so the match has to be the most specific one. Taking the first
	// hit instead would depend on Go's randomized map iteration order and
	// bill the same request at different rates on different runs.
	longest := ""
	for k := range Prices {
		if len(k) > len(longest) && strings.Contains(model, k) {
			longest = k
		}
	}
	if longest != "" {
		return Prices[longest]
	}
	if strings.Contains(strings.ToLower(model), "claude") {
		return unknownClaudePrice
	}
	return defaultPrice
}

// Tracker accumulates token usage and spend for the session.
//
// Every field is behind mu and reachable only through the accessors below.
// AddDetailed runs on the engine goroutine after each model call, while the
// REPL/TUI status line, /cost, /status and the budget guard all read the
// totals from their own goroutines — with plain exported fields those were
// unsynchronized reads of a value the budget guard then acts on.
type Tracker struct {
	mu                   sync.Mutex
	totalInput           int
	totalOutput          int
	totalPromptCacheHit  int
	totalPromptCacheMiss int
	totalCost            float64
	maxBudget            float64
}

// Totals is a point-in-time snapshot of a Tracker.
type Totals struct {
	Input           int
	Output          int
	PromptCacheHit  int
	PromptCacheMiss int
	Cost            float64
	MaxBudget       float64
}

func NewTracker(maxBudget float64) *Tracker {
	return &Tracker{maxBudget: maxBudget}
}

// Totals returns a consistent snapshot of every counter.
func (t *Tracker) Totals() Totals {
	t.mu.Lock()
	defer t.mu.Unlock()
	return Totals{
		Input:           t.totalInput,
		Output:          t.totalOutput,
		PromptCacheHit:  t.totalPromptCacheHit,
		PromptCacheMiss: t.totalPromptCacheMiss,
		Cost:            t.totalCost,
		MaxBudget:       t.maxBudget,
	}
}

// SetMaxBudget updates the spend ceiling (0 = unlimited).
func (t *Tracker) SetMaxBudget(v float64) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.maxBudget = v
}

func (t *Tracker) Add(model string, input, output int) {
	t.AddDetailed(model, input, output, 0, 0)
}

func (t *Tracker) AddDetailed(model string, input, output, cacheHit, cacheMiss int) {
	t.AddWithCacheWrite(model, input, output, cacheHit, cacheMiss, 0)
}

// AddWithCacheWrite is AddDetailed for a response that also reports prompt
// cache writes: cacheWrite of the cacheMiss tokens were written into the
// cache and are billed at InputCacheWrite (1.25× input by default) instead of
// the input rate. They used to be billed as plain input.
func (t *Tracker) AddWithCacheWrite(model string, input, output, cacheHit, cacheMiss, cacheWrite int) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.totalInput += input
	t.totalOutput += output
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
	t.totalPromptCacheHit += cacheHit
	t.totalPromptCacheMiss += cacheMiss
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
	t.totalCost += (float64(cacheMiss-cacheWrite)/1e6)*p.Input + (float64(cacheWrite)/1e6)*p.InputCacheWrite +
		(float64(cacheHit)/1e6)*p.InputCacheHit + (float64(output)/1e6)*p.Output
}

func (t *Tracker) OverBudget() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.maxBudget <= 0 {
		return false
	}
	return t.totalCost >= t.maxBudget
}

// SuggestedBudget returns an auto-increased budget target based on current spend.
// The result is rounded up to 2 decimals and is always higher than TotalCost.
func (t *Tracker) SuggestedBudget() float64 {
	t.mu.Lock()
	base := t.totalCost
	t.mu.Unlock()
	if base < 0 {
		base = 0
	}
	// Keep a practical headroom so one extra turn can complete.
	target := base * 1.2
	if target-base < 2 {
		target = base + 2
	}
	return math.Ceil(target*100) / 100
}

func (t *Tracker) Summary() string {
	tot := t.Totals()
	sb := &strings.Builder{}
	sb.WriteString(strconv.Itoa(tot.Input) + " in")
	if tot.PromptCacheHit > 0 || tot.PromptCacheMiss > 0 {
		sb.WriteString(" (cache hit " + strconv.Itoa(tot.PromptCacheHit) + ", miss " + strconv.Itoa(tot.PromptCacheMiss) + ")")
	}
	sb.WriteString(" | " + strconv.Itoa(tot.Output) + " out | $" + ftoa(tot.Cost))
	if tot.MaxBudget > 0 {
		sb.WriteString(" / $" + ftoa(tot.MaxBudget))
	}
	return sb.String()
}

func ftoa(f float64) string {
	if f == 0 {
		return "0.00"
	}
	if f < 0.0001 {
		return "0.00"
	}
	if f < 0.01 {
		return strconv.FormatFloat(f, 'f', 4, 64)
	}
	return strconv.FormatFloat(f, 'f', 2, 64)
}

// --- Cost Persistence ---

// CostRecord represents a persisted cost entry for a session.
type CostRecord struct {
	SessionID string    `json:"session_id"`
	Model     string    `json:"model"`
	Input     int       `json:"input"`
	Output    int       `json:"output"`
	CacheHit  int       `json:"cache_hit"`
	CacheMiss int       `json:"cache_miss"`
	Cost      float64   `json:"cost"`
	Timestamp time.Time `json:"timestamp"`
}

// CostHistory manages persistent cost records across sessions.
type CostHistory struct {
	path    string
	Records []CostRecord `json:"records"`
}

// NewCostHistory loads or creates a cost history file.
func NewCostHistory() *CostHistory {
	home, _ := os.UserHomeDir()
	dir := filepath.Join(home, ".cove")
	_ = os.MkdirAll(dir, 0700)
	path := filepath.Join(dir, "cost_history.json")

	h := &CostHistory{path: path}
	h.load()
	return h
}

func (h *CostHistory) load() {
	data, err := os.ReadFile(h.path)
	if err != nil {
		return
	}
	// Save writes {"records": [...]}, but this used to decode a bare array, so
	// every reload failed, started empty, and the next Save dropped the old
	// records. Accept the object form, and the bare array for older files.
	var file struct {
		Records []CostRecord `json:"records"`
	}
	if err := json.Unmarshal(data, &file); err == nil {
		h.Records = file.Records
		return
	}
	if err := json.Unmarshal(data, &h.Records); err != nil {
		h.Records = nil
	}
}

// Save persists the current cost history to disk. The replace is atomic so a
// crash mid-write cannot truncate the history.
func (h *CostHistory) Save() error {
	data, err := json.MarshalIndent(h, "", "  ")
	if err != nil {
		return err
	}
	return fsatomic.WriteFile(h.path, data, 0600)
}

// Add records a new cost entry.
func (h *CostHistory) Add(sessionID, model string, t *Tracker) {
	if t == nil {
		return
	}
	tot := t.Totals()
	h.Records = append(h.Records, CostRecord{
		SessionID: sessionID,
		Model:     model,
		Input:     tot.Input,
		Output:    tot.Output,
		CacheHit:  tot.PromptCacheHit,
		CacheMiss: tot.PromptCacheMiss,
		Cost:      tot.Cost,
		Timestamp: time.Now(),
	})
	// Keep at most last 100 records
	if len(h.Records) > 100 {
		h.Records = h.Records[len(h.Records)-100:]
	}
}

// TotalAllTime returns the sum of all historical costs.
func (h *CostHistory) TotalAllTime() float64 {
	var total float64
	for _, r := range h.Records {
		total += r.Cost
	}
	return total
}

// Last7Days returns the sum of costs in the last 7 days.
func (h *CostHistory) Last7Days() float64 {
	var total float64
	cutoff := time.Now().AddDate(0, 0, -7)
	for _, r := range h.Records {
		if r.Timestamp.After(cutoff) {
			total += r.Cost
		}
	}
	return total
}

// Last24Hours returns the sum of costs in the last 24 hours.
func (h *CostHistory) Last24Hours() float64 {
	var total float64
	cutoff := time.Now().Add(-24 * time.Hour)
	for _, r := range h.Records {
		if r.Timestamp.After(cutoff) {
			total += r.Cost
		}
	}
	return total
}
