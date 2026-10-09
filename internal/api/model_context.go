package api

import (
	"strings"
	"sync"
)

// contextWindowPattern maps a case-insensitive substring of a model name to
// its approximate context window size, in tokens. These are deliberately
// conservative approximations — a heuristic for sizing Cove's own
// compression threshold, not an authoritative API limit table, and never
// sent to the provider as a hard limit. Order matters: more specific
// patterns are listed before broader ones they'd otherwise be shadowed by.
type contextWindowPattern struct {
	pattern string
	window  int
}

var contextWindowPatterns = []contextWindowPattern{
	// Current 1M-context Claude models, listed before the family fallbacks
	// below that would otherwise shadow them.
	{"claude-fable", 1000000},
	{"claude-mythos", 1000000},
	{"claude-opus-5", 1000000},
	{"claude-opus-4-8", 1000000},
	{"claude-opus-4-7", 1000000},
	{"claude-opus-4-6", 1000000},
	{"claude-sonnet-5", 1000000},
	{"claude-sonnet-4-6", 1000000},
	{"claude-opus", 200000},
	{"claude-sonnet", 200000},
	{"claude-haiku", 200000},
	{"claude-3", 200000},
	// GPT-5 and GPT-4.1 families (platform.openai.com/docs/models); o3/o4
	// moved to 200k. Without entries they fell to the 32k default: history
	// was compacted at 20k tokens and only 8k output requested.
	{"gpt-5", 400000},
	{"gpt-4.1", 1000000},
	{"gpt-4o", 128000},
	{"gpt-4-turbo", 128000},
	{"o1", 200000},
	{"o3", 200000},
	{"o4", 200000},
	// DeepSeek V4: 1M context for both tiers (api-docs.deepseek.com). The
	// current flash name is "deepseek-flash"; "deepseek-v4-flash" is the
	// retired alias, still accepted.
	{"deepseek-v4", 1000000},
	{"deepseek-flash", 1000000},
	{"deepseek-chat", 64000},
	{"deepseek-reasoner", 64000},
	{"glm-4", 128000},
	{"glm", 128000},
	{"kimi", 128000},
	{"moonshot", 128000},
	{"qwen-long", 1000000},
	{"qwen", 32000},
	{"doubao", 32000},
	{"gemini-1.5", 1000000},
	{"gemini", 128000},
}

// defaultContextWindow is used for model names that don't match any known
// pattern — deliberately conservative so an unrecognized model compacts
// history sooner rather than risking an over-budget request.
const defaultContextWindow = 32000

// contextOverrides holds windows learned or configured for a model this
// session (SetModelContextWindow), keyed by lower-cased name. They beat the
// name-based guesses: a local server answering with its n_ctx knows better
// than a substring table.
var (
	contextOverridesMu sync.RWMutex
	contextOverrides   = map[string]int{}
)

// SetModelContextWindow records model's real context window for this
// session; tokens <= 0 removes the record.
func SetModelContextWindow(model string, tokens int) {
	key := strings.ToLower(strings.TrimSpace(model))
	contextOverridesMu.Lock()
	defer contextOverridesMu.Unlock()
	if tokens <= 0 {
		delete(contextOverrides, key)
		return
	}
	contextOverrides[key] = tokens
}

// ClearModelContextWindows forgets every SetModelContextWindow record.
func ClearModelContextWindows() {
	contextOverridesMu.Lock()
	defer contextOverridesMu.Unlock()
	contextOverrides = map[string]int{}
}

// ContextWindowKnown reports whether model's window is known (set this
// session or matched by name) rather than the defaultContextWindow guess.
// Decisions that cut capability for small windows (fewer tools) act only on
// a known window; the guess is not evidence.
func ContextWindowKnown(model string) bool {
	lower := strings.ToLower(strings.TrimSpace(model))
	contextOverridesMu.RLock()
	_, ok := contextOverrides[lower]
	contextOverridesMu.RUnlock()
	if ok {
		return true
	}
	for _, p := range contextWindowPatterns {
		if strings.Contains(lower, p.pattern) {
			return true
		}
	}
	return false
}

// ContextWindowForModel returns an approximate context window size (in
// tokens) for the given model name: a window set for it this session, else
// a substring match against known model families, else defaultContextWindow.
func ContextWindowForModel(model string) int {
	lower := strings.ToLower(strings.TrimSpace(model))
	contextOverridesMu.RLock()
	w, ok := contextOverrides[lower]
	contextOverridesMu.RUnlock()
	if ok {
		return w
	}
	for _, p := range contextWindowPatterns {
		if strings.Contains(lower, p.pattern) {
			return p.window
		}
	}
	return defaultContextWindow
}

// UtilizationRatioForModel returns how much of a model's raw context window
// Cove assumes can be put to effective use. It is the same for every tier:
// the fast tiers in use today (deepseek-flash, Haiku 4.5, ...) are fully
// capable models, and the old "fast models use long context less well"
// discount (0.65) only made Cove throw their history away sooner.
func UtilizationRatioForModel(model string) float64 {
	return 0.85
}

// EffectiveCompactionBudget returns the context size Cove budgets for a
// model: its (approximate) context window scaled by the effective-utilization
// ratio. The engine compacts at CompactionTrigger, a fraction of it that
// also leaves room for the model's response.
//
// It used to reserve a further 30% for the system prompt, tool definitions
// and repo map ("× 0.7", docs/中等模型平替优化建议.md §2.3). The count it is
// compared with is now the provider's reported prompt size, which already
// includes all of those, so the reserve counted them twice.
func EffectiveCompactionBudget(model string) int {
	window := ContextWindowForModel(model)
	ratio := UtilizationRatioForModel(model)
	budget := int(float64(window) * ratio)
	const floor = 4000 // never compact so aggressively useful history can't fit at all
	if budget < floor {
		budget = floor
	}
	return budget
}

// StaticContextBudget returns the token budget Cove allocates to
// per-turn "static" system-prompt content that isn't the running
// conversation — matched skill prompts, retrieved memories, the repo
// map, and the project file tree: 30% of the effective window (the share
// EffectiveCompactionBudget used to hold back for it), clamped to a sane
// floor/ceiling so extreme-context
// models (e.g. qwen-long's 1M-token window) don't get an unbounded
// allowance that would just get shipped to the provider unexamined.
//
// See internal/engine/context_budget.go, which spends this budget across
// priority layers (relevant > on-demand > overflow) instead of letting
// every section grow without bound and hoping the combined total happens
// to fit — the "Context分层预算器" item in
// docs/中等模型平替优化建议.md.
func StaticContextBudget(model string) int {
	window := ContextWindowForModel(model)
	ratio := UtilizationRatioForModel(model)
	budget := int(float64(window) * ratio * 0.3)
	const floor = 2000
	const ceiling = 24000
	if budget < floor {
		budget = floor
	}
	if budget > ceiling {
		budget = ceiling
	}
	return budget
}

// maxRequestOutputTokens caps the output any request asks for.
const maxRequestOutputTokens = 64000

// minRequestOutputTokens is the least output a request asks for, however
// small the window.
const minRequestOutputTokens = 4000

// CompactionSafetyMargin is the most kept free on top of the reply's
// MaxTokens when deciding where compaction triggers: token counts are partly
// estimated, and the provider's own framing takes a little room too.
const CompactionSafetyMargin = 8000

// minCompactionSafetyMargin is the least of it, however small the window.
const minCompactionSafetyMargin = 1024

// CompactionSafetyMarginFor is the margin for model's window: an eighth of
// it, between minCompactionSafetyMargin and CompactionSafetyMargin. The
// estimate's error grows with the prompt, not with a fixed amount; a flat
// 8000 took half of a 16K window, put the trigger (4288) below the fixed
// overhead of every request and turned compaction off for a window that
// still had ~5.7K tokens of room for the conversation.
func CompactionSafetyMarginFor(model string) int {
	m := ContextWindowForModel(model) / 8
	if m < minCompactionSafetyMargin {
		m = minCompactionSafetyMargin
	}
	if m > CompactionSafetyMargin {
		m = CompactionSafetyMargin
	}
	return m
}

// compactionFraction is the share of EffectiveCompactionBudget at which the
// engine compacts. 0.75 of a 200K model's 170K budget is 127.5K: history
// of 128K on a 200K window already crowds the reply out.
const compactionFraction = 0.75

// modelOutputCaps lists models whose API rejects or truncates larger
// max_tokens values. Substring match, most specific first.
//
// Opus 4 and 4.1 take at most 32000 output tokens. They were missing, so
// their 200K window gave an outputReserve of 50000 and every request came
// back 400. Bare "claude-opus-4" would also match Opus 4.6+ (no such cap),
// so Opus 4 itself is matched by its dated, "-0" alias and Vertex "@" forms.
// Sonnet 4/4.5 and Opus 4.5 take 64000: the request ceiling anyway, listed
// so the table says so rather than relying on the clamp.
var modelOutputCaps = []contextWindowPattern{
	{"claude-opus-4-1", 32000},
	{"claude-opus-4-0", 32000},
	{"claude-opus-4-2025", 32000},
	{"claude-opus-4@", 32000},
	{"claude-opus-4-5", 64000},
	{"claude-sonnet-4-5", 64000},
	{"claude-sonnet-4-0", 64000},
	{"claude-sonnet-4-2025", 64000},
	{"claude-sonnet-4@", 64000},
	{"claude-3-5-sonnet", 8192},
	{"claude-3-5-haiku", 8192},
	{"claude-3-opus", 4096},
	{"claude-3-haiku", 4096},
	{"gpt-4o", 16384},
	{"deepseek-chat", 8192},
}

// MaxOutputTokensForModel is the max_tokens a request to model asks for:
// min(64000, the model's own output cap, a quarter of its window), and never
// below 4000. A fixed 64000 asked a 64K-window model for its whole window.
func MaxOutputTokensForModel(model string) int {
	n := outputReserve(model)
	lower := strings.ToLower(model)
	for _, c := range modelOutputCaps {
		if strings.Contains(lower, c.pattern) {
			if c.window < n {
				n = c.window
			}
			break
		}
	}
	return n
}

// outputReserve is the share of model's window kept for the reply: a quarter
// of it, clamped to [4000, 64000]. It is MaxOutputTokensForModel before the
// model's own output cap, so a model with a small cap still compacts with
// the same headroom.
func outputReserve(model string) int {
	n := ContextWindowForModel(model) / 4
	if n < minRequestOutputTokens {
		n = minRequestOutputTokens
	}
	if n > maxRequestOutputTokens {
		n = maxRequestOutputTokens
	}
	return n
}

// CompactionTrigger is the context size at which the engine compacts the
// history for model: compactionFraction of EffectiveCompactionBudget, but
// never so late that the reply (outputReserve, which is at least
// MaxOutputTokensForModel) plus CompactionSafetyMarginFor no longer fits the
// window.
func CompactionTrigger(model string) int {
	trigger := int(compactionFraction * float64(EffectiveCompactionBudget(model)))
	if room := ContextWindowForModel(model) - outputReserve(model) - CompactionSafetyMarginFor(model); room < trigger {
		trigger = room
	}
	const floor = 2000 // a window this small cannot hold a useful history anyway
	if trigger < floor {
		trigger = floor
	}
	return trigger
}
