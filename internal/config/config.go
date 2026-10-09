package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"

	"github.com/liuzhixin405/cove-agent/internal/api"
	"github.com/liuzhixin405/cove-agent/internal/fsatomic"
	"github.com/liuzhixin405/cove-agent/internal/log"
)

type ProviderConfig struct {
	Name          string   `json:"name"`
	APIKey        string   `json:"api_key,omitempty"`
	APIKeys       []string `json:"-"`
	BaseURL       string   `json:"base_url,omitempty"`
	ImageFilesAPI *bool    `json:"image_files_api,omitempty"`
}

func (p ProviderConfig) ImageFilesEnabled() bool {
	return p.ImageFilesAPI != nil && *p.ImageFilesAPI
}

// MarshalJSON masks the API key to prevent leakage in logs/display.
func (p ProviderConfig) MarshalJSON() ([]byte, error) {
	type alias ProviderConfig
	a := alias(p)
	if a.APIKey != "" {
		a.APIKey = maskKey(a.APIKey)
	}
	return json.Marshal(a)
}

func maskKey(key string) string {
	if len(key) <= 8 {
		return "****"
	}
	return key[:4] + "****" + key[len(key)-4:]
}

type Profile struct {
	Model          string          `json:"model,omitempty"`
	ModelFast      string          `json:"model_fast,omitempty"`
	Provider       *ProviderConfig `json:"provider,omitempty"`
	PermissionMode string          `json:"permission_mode,omitempty"`
	MaxBudgetUsd   float64         `json:"max_budget_usd,omitempty"`
	ThinkingTokens int             `json:"thinking_tokens,omitempty"`
	// Debug and Verbose are pointers so "absent" is distinguishable from
	// "false". As plain bools, applyProfile could only ever turn them ON
	// (`if prof.Debug { cfg.Debug = true }`), so a profile written specifically
	// to quieten a noisy global config — "debug": false — did nothing at all.
	Debug        *bool  `json:"debug,omitempty"`
	Verbose      *bool  `json:"verbose,omitempty"`
	SystemPrompt string `json:"system_prompt,omitempty"`
	// The per-turn limits and max_sessions, as in Config. MaxTurnMinutes is a
	// pointer because 0 ("off") is a value a profile may set.
	MaxIterations         int  `json:"max_iterations,omitempty"`
	MaxTurnMinutes        *int `json:"max_turn_minutes,omitempty"`
	SubagentMaxIterations int  `json:"subagent_max_iterations,omitempty"`
	MaxSessions           int  `json:"max_sessions,omitempty"`

	// extra holds the keys of this profile that the struct does not know (an
	// option from a newer cove, a note the user added), so Save writes them
	// back instead of dropping them.
	extra map[string]json.RawMessage
}

// UnmarshalJSON keeps backward compatibility with older configs that used
// profile "mode" instead of "permission_mode".
func (p *Profile) UnmarshalJSON(data []byte) error {
	type alias Profile
	aux := struct {
		alias
		Mode string `json:"mode,omitempty"`
	}{}
	if err := json.Unmarshal(data, &aux); err != nil {
		return err
	}
	*p = Profile(aux.alias)
	if p.PermissionMode == "" && aux.Mode != "" {
		p.PermissionMode = aux.Mode
	}
	var all map[string]json.RawMessage
	if json.Unmarshal(data, &all) == nil {
		for k, v := range all {
			// "mode" was folded into permission_mode above; writing it back
			// would resurrect the legacy field.
			if k == "mode" || profileKnownKeys[k] {
				continue
			}
			if p.extra == nil {
				p.extra = map[string]json.RawMessage{}
			}
			p.extra[k] = v
		}
	}
	return nil
}

// profileKnownKeys are the JSON names of Profile's fields.
var profileKnownKeys = func() map[string]bool {
	m := map[string]bool{}
	t := reflect.TypeOf(Profile{})
	for i := 0; i < t.NumField(); i++ {
		if name, _, _ := strings.Cut(t.Field(i).Tag.Get("json"), ","); name != "" && name != "-" {
			m[name] = true
		}
	}
	return m
}()

// rawJSON is the profile as written to disk: every field, the provider's API
// key unmasked, and the keys the struct does not know.
func (p *Profile) rawJSON() (json.RawMessage, error) {
	type alias Profile
	cp := alias(*p)
	cp.Provider = nil // marshalled below without ProviderConfig's masking
	data, err := json.Marshal(cp)
	if err != nil {
		return nil, err
	}
	m := map[string]json.RawMessage{}
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, err
	}
	if p.Provider != nil {
		if m["provider"], err = json.Marshal(rawProvider{
			Name:          p.Provider.Name,
			APIKey:        p.Provider.APIKey,
			BaseURL:       p.Provider.BaseURL,
			ImageFilesAPI: p.Provider.ImageFilesAPI,
		}); err != nil {
			return nil, err
		}
	}
	for k, v := range p.extra {
		if _, ok := m[k]; !ok {
			m[k] = v
		}
	}
	return json.Marshal(m)
}

type Config struct {
	Model          string         `json:"model"`
	ModelFast      string         `json:"model_fast,omitempty"`
	Provider       ProviderConfig `json:"provider"`
	PermissionMode string         `json:"permission_mode"`
	MaxBudgetUsd   float64        `json:"max_budget_usd"`
	ThinkingTokens int            `json:"thinking_tokens"`
	// ContextWindow is the context window of Model in tokens, for servers
	// cove cannot recognise by model name (a local llama.cpp with -c 16384).
	// It sizes compaction; 0 keeps the name-based estimate. The E2008
	// remedy suggests the value to put here.
	ContextWindow int `json:"context_window,omitempty"`
	// ModelContextWindows are windows learned per model (lower-cased name →
	// tokens): the E2008 remedy writes the window a server reported here,
	// so the next start budgets for it without hitting the wall again. It is
	// keyed by model, unlike ContextWindow, so switching profiles does not
	// apply one model's window to another.
	ModelContextWindows map[string]int             `json:"model_context_windows,omitempty"`
	Debug               bool                       `json:"debug"`
	Verbose             bool                       `json:"verbose"`
	SystemPrompt        string                     `json:"system_prompt,omitempty"`
	MCPServers          map[string]MCPServerConfig `json:"mcp_servers,omitempty"`
	Profiles            map[string]*Profile        `json:"profiles,omitempty"`
	ActiveProfile       string                     `json:"active_profile,omitempty"`
	// The former "telemetry" key is no longer read (the recorder had no
	// callers and was removed). Old files that still carry it load fine:
	// unknown keys are ignored, and Save keeps them on disk.
	// DoneVerifyCommands, if set, are shell commands (e.g. "go build ./...")
	// run before the engine accepts a model's "no more tool calls" response
	// as actually complete; see internal/engine/verify_gate.go. Off by
	// default; an empty/absent list disables the gate entirely.
	DoneVerifyCommands []string `json:"done_verify_commands,omitempty"`
	// DoneVerifyAuto, when no done_verify_commands are configured, derives a
	// verification command from the project (go.mod -> "go build ./...",
	// Cargo.toml -> "cargo check", a local TypeScript install -> tsc) and runs
	// it only on turns that changed files. nil means on.
	DoneVerifyAuto *bool `json:"done_verify_auto,omitempty"`
	// DoneVerifyTimeoutSeconds bounds each verification command; 0 (the
	// default) keeps 120 s, and 300 s for dotnet and npm commands. A command
	// that runs out of time is reported, not counted as a failure.
	DoneVerifyTimeoutSeconds int `json:"done_verify_timeout_seconds,omitempty"`
	// DoneVerifyTests, with automatic verification on, also runs the tests
	// (and go vet) of the packages/projects the turn changed: compiling says
	// nothing about the logic. nil means on.
	DoneVerifyTests *bool `json:"done_verify_tests,omitempty"`
	// DoneSelfReview has a read-only review sub-agent check the turn's diff
	// before the turn may end: "off" (the default), "on" (every turn that
	// changed files) or "auto" (only changes of SelfReviewMinLines or more).
	DoneSelfReview string `json:"done_self_review,omitempty"`
	// DoneCheck controls the one-time "is the request fully met?" prompt the
	// engine shows a model that is about to end a turn which changed files:
	// "on", "off", or "auto" (the default, also for an empty or unknown
	// value): only fast-tier models and providers other than anthropic.
	DoneCheck string `json:"done_check,omitempty"`
	// Thinking selects the model's thinking mode on providers that support it
	// ("adaptive" or "disabled"); empty keeps the model's default. Effort
	// ("low", "medium", "high", "xhigh", "max") sets reasoning depth.
	Thinking string `json:"thinking,omitempty"`
	Effort   string `json:"effort,omitempty"`
	// ShowReasoning streams a thinking model's full reasoning into the
	// conversation. Off by default: the status line shows its progress.
	ShowReasoning bool `json:"show_reasoning,omitempty"`
	// DisabledSkills are skills (built-in or otherwise) that are not loaded.
	DisabledSkills []string `json:"disabled_skills,omitempty"`
	// MemoryEmbedding, if set, opts the memory store into blending BM25
	// keyword search with real semantic similarity from a remote embeddings
	// API. Off by default; nil means pure BM25 with zero extra network calls
	// or cost, exactly like before this field existed.
	MemoryEmbedding *MemoryEmbeddingConfig `json:"memory_embedding,omitempty"`
	// ExperimentalTools registers the experimental coordination tools
	// (task/task_*, team_*, send_message, brief, sleep). Off by default:
	// every registered tool costs prompt tokens on every request.
	ExperimentalTools bool `json:"experimental_tools,omitempty"`
	// WebSearch selects the websearch backend ("tavily", "brave" or
	// "duckduckgo") and its API key. A configured provider wins over the
	// environment; its key falls back to the provider's environment variable
	// (TAVILY_API_KEY, BRAVE_API_KEY / BRAVE_SEARCH_API_KEY). Without a
	// provider the environment variables pick the backend as before.
	WebSearch *WebSearchConfig `json:"web_search,omitempty"`
	// MaxSessions is how many saved sessions are kept: older ones are deleted
	// automatically at the end of a turn (the session in use never is).
	// Unset or 0 means DefaultMaxSessions; a negative value turns pruning off.
	MaxSessions int `json:"max_sessions,omitempty"`
	// MaxIterations caps the model calls of one turn. At the cap the
	// interactive shell asks whether to go on; -p stops (--max-turns
	// overrides it there). Unset or <= 0 means DefaultMaxIterations.
	MaxIterations int `json:"max_iterations,omitempty"`
	// MaxTurnMinutes limits how long one turn runs before the shell asks
	// whether to go on (-p stops). 0 or negative turns the limit off, so the
	// key has no omitempty: an explicit 0 must survive a Save.
	MaxTurnMinutes int `json:"max_turn_minutes"`
	// SubagentMaxIterations caps each sub-agent's model calls (agent,
	// execute_plan). Unset or <= 0 means DefaultSubagentMaxIterations.
	SubagentMaxIterations int `json:"subagent_max_iterations,omitempty"`

	// loadedView is the effective config as Load returned it (see rawView).
	// Save writes only the fields that differ from it, so values that came
	// from .cove.json, the active profile or the built-in defaults stay where
	// they came from instead of being copied into ~/.cove/config.json.
	// nil (a Config not built by Load) means "write every field".
	loadedView map[string]json.RawMessage
	// turnMinutesExplicit records that config.json, .cove.json or the active
	// profile set max_turn_minutes (UnattendedTurnMinutes).
	turnMinutesExplicit bool
	// appliedProfile is the profile Load applied (active_profile or
	// --profile). Save writes a change to a field that profile sets into the
	// profile: the top level would be overridden by it again on every start.
	appliedProfile string
	// appliedProf is that profile as Load applied it, kept so Save still
	// knows which keys it set once /profile delete removed it from Profiles.
	appliedProf *Profile

	// The project's .cove.json as Load saw it: its path, the sha256 of the
	// content that was read, and the sensitive fields that were ignored
	// because that content is not trusted (see project_trust.go).
	projectConfigPath string
	projectConfigHash string
	untrustedFields   []string

	// userValues and loadedValues are the profile settings Load produced
	// without and with the project .cove.json (profile_snapshot.go), so
	// SnapshotProfile can leave the project's values out of a saved
	// profile. Both nil when the project changed none of them.
	userValues, loadedValues *profileValues
}

// UnattendedTurnMinutes is the turn time limit for -p and headless runs,
// where nobody can answer the prompt that the interactive shell shows at the
// limit: only a max_turn_minutes the user wrote applies there, the default
// does not (0 = no limit).
func (c *Config) UnattendedTurnMinutes() int {
	if c == nil || !c.turnMinutesExplicit {
		return 0
	}
	return c.MaxTurnMinutes
}

// hasKey reports whether the JSON object in data has a top-level key.
func hasKey(data []byte, key string) bool {
	var m map[string]json.RawMessage
	if json.Unmarshal(data, &m) != nil {
		return false
	}
	_, ok := m[key]
	return ok
}

// DoneCheckMode is the effective done_check setting: "on", "off" or "auto".
func (c *Config) DoneCheckMode() string {
	if c != nil {
		switch m := strings.ToLower(strings.TrimSpace(c.DoneCheck)); m {
		case "on", "off":
			return m
		}
	}
	return "auto"
}

// VerifyAutoEnabled reports whether automatic completion verification is on.
// On (the default) still runs the detected commands only in a trusted
// project directory or in auto/bypass mode (engine verifyTrusted): they
// execute the repository's build scripts.
func (c *Config) VerifyAutoEnabled() bool {
	return c.DoneVerifyAuto == nil || *c.DoneVerifyAuto
}

// VerifyTestsEnabled reports whether automatic verification also runs the
// tests of what the turn changed.
func (c *Config) VerifyTestsEnabled() bool {
	return c.VerifyAutoEnabled() && (c.DoneVerifyTests == nil || *c.DoneVerifyTests)
}

// SelfReviewMode is the effective done_self_review setting: "on", "auto" or
// "off" (the default, also for an unknown value).
func (c *Config) SelfReviewMode() string {
	if c != nil {
		switch m := strings.ToLower(strings.TrimSpace(c.DoneSelfReview)); m {
		case "on", "auto":
			return m
		}
	}
	return "off"
}

// MemoryEmbeddingConfig configures the optional remote embeddings endpoint
// used for semantic memory search. BaseURL/APIKey default to the main
// provider's values when empty, so in the common case a user who wants
// this only needs to add `"memory_embedding": {}` (or set a model name);
// no separate account or key is needed, reusing what is already configured for chat.
type MemoryEmbeddingConfig struct {
	BaseURL string `json:"base_url,omitempty"`
	APIKey  string `json:"api_key,omitempty"`
	Model   string `json:"model,omitempty"`
}

// WebSearchConfig configures the websearch tool (config key web_search).
type WebSearchConfig struct {
	Provider string `json:"provider,omitempty"`
	APIKey   string `json:"api_key,omitempty"`
}

type MCPServerConfig struct {
	Command string            `json:"command"`
	Args    []string          `json:"args,omitempty"`
	Env     map[string]string `json:"env,omitempty"`
	Type    string            `json:"type,omitempty"`
	URL     string            `json:"url,omitempty"`
}

// DefaultMaxSessions is the max_sessions default.
const DefaultMaxSessions = 200

// Defaults of the per-turn limits.
const (
	DefaultMaxIterations         = 200
	DefaultMaxTurnMinutes        = 60
	DefaultSubagentMaxIterations = 60
)

func DefaultConfig() *Config {
	return &Config{
		Model:          "claude-sonnet-4-20250514",
		PermissionMode: "default",
		MaxBudgetUsd:   10,
		ThinkingTokens: 16000,
		MaxSessions:    DefaultMaxSessions,

		MaxIterations:         DefaultMaxIterations,
		MaxTurnMinutes:        DefaultMaxTurnMinutes,
		SubagentMaxIterations: DefaultSubagentMaxIterations,
	}
}

func ConfigDir() (string, error) {
	if d := os.Getenv("COVE_CONFIG_DIR"); d != "" {
		return d, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".cove"), nil
}

func Load() (*Config, error) {
	return LoadWithProfile("")
}

func LoadWithProfile(profileName string) (*Config, error) {
	cfg := DefaultConfig()
	// DefaultConfig's model is Anthropic's. Left in place, a config that only
	// says "provider": {"name": "deepseek"} sent claude-sonnet-4 to DeepSeek and
	// every request failed; empty lets applyDefaults pick the provider's default.
	cfg.Model = ""
	var base Config // cfg before the project override
	baseSet := false
	var applied *Profile
	finish := func(err error) (*Config, error) {
		if !baseSet {
			base = *cfg // failed before the project override: nothing from it
		}
		applyDefaults(cfg)
		cfg.loadedView, _ = rawView(cfg)
		recordUserLevelValues(cfg, base, applied)
		return cfg, err
	}
	dir, err := ConfigDir()
	if err == nil {
		p := filepath.Join(dir, "config.json")
		data, err := os.ReadFile(p)
		if err == nil {
			if err := json.Unmarshal(stripBOM(data), cfg); err != nil {
				return finish(fmt.Errorf("parse config %s: %w", p, err))
			}
			cfg.turnMinutesExplicit = hasKey(stripBOM(data), "max_turn_minutes")
		}
	}
	base, baseSet = *cfg, true
	if err := loadProjectOverride(cfg); err != nil {
		return finish(err)
	}
	if profileName == "" {
		profileName = cfg.ActiveProfile
	}
	if profileName != "" {
		prof, ok := cfg.Profiles[profileName]
		if !ok {
			// Used to be ignored, so `cove --profile wrok` quietly ran on the
			// base settings. The base config is still returned and usable.
			return finish(fmt.Errorf("profile %q not found in config", profileName))
		}
		applyProfile(cfg, prof)
		applied = prof
		cfg.appliedProfile = profileName
		cfg.appliedProf = prof
	}
	return finish(nil)
}

// utf8BOM is what Windows PowerShell 5.1 (Out-File, Set-Content -Encoding
// utf8) and older Notepad put in front of a UTF-8 file. encoding/json rejects
// it, which made such a config fail to parse and every setting in it ignored.
var utf8BOM = []byte{0xEF, 0xBB, 0xBF}

func stripBOM(data []byte) []byte { return bytes.TrimPrefix(data, utf8BOM) }

// CheckFile reports whether the config file at path would be rejected by
// Load. A missing file is fine: cove runs on defaults and environment keys.
func CheckFile(path string) error {
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var probe Config
	if err := json.Unmarshal(stripBOM(data), &probe); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	return nil
}

// maxProjectConfigBytes caps a project .cove.json; a real one is a few KiB.
const maxProjectConfigBytes = 1 << 20

// errProjectConfigRefused marks a .cove.json that is not read at all: not a
// regular file, or over maxProjectConfigBytes.
var errProjectConfigRefused = errors.New("project config ignored")

// readProjectConfigFile reads the project .cove.json at p in one read, the
// bytes both parsed and hashed for the trust gate.
//
// The file comes with the repository. It used to be read with os.ReadFile,
// which follows a symlink and reads without a limit, so a committed
// `.cove.json -> /dev/zero` (or /dev/tty, a fifo) hung cove at startup or ran
// it out of memory, and the dream worker with it. Only a regular file, not a
// link, of at most maxProjectConfigBytes is read now.
func readProjectConfigFile(p string) ([]byte, error) {
	fi, err := os.Lstat(p)
	if err != nil {
		return nil, err
	}
	if !fi.Mode().IsRegular() {
		return nil, fmt.Errorf("%w: %s is not a regular file (%s)", errProjectConfigRefused, p, fi.Mode().Type())
	}
	if fi.Size() > maxProjectConfigBytes {
		return nil, fmt.Errorf("%w: %s is larger than %d bytes", errProjectConfigRefused, p, maxProjectConfigBytes)
	}
	f, err := os.Open(p)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	// The path may have been swapped for a link or a device between the
	// Lstat and the Open; what was opened must be the file that was checked.
	if opened, err := f.Stat(); err != nil || !os.SameFile(fi, opened) {
		return nil, fmt.Errorf("%w: %s changed while being read", errProjectConfigRefused, p)
	}
	data, err := io.ReadAll(io.LimitReader(f, maxProjectConfigBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxProjectConfigBytes {
		return nil, fmt.Errorf("%w: %s is larger than %d bytes", errProjectConfigRefused, p, maxProjectConfigBytes)
	}
	return data, nil
}

// loadProjectOverride merges the working directory's .cove.json into cfg.
//
// The file comes with the repository, so a clone must not be able to run code
// or redirect credentials just by being opened. It used to be applied in full:
// its mcp_servers were started at launch (`python -c ...` ran before the first
// prompt), its provider.base_url received the user's global API key on the
// first request, and its done_verify_commands ran after every turn. Fields
// like these (see the sensitive checks below) now apply only when the user
// trusted this exact content (project_trust.go); otherwise they are left out
// and recorded for UntrustedProjectConfig. Everything else applies as before.
func loadProjectOverride(cfg *Config) error {
	cwd, err := os.Getwd()
	if err != nil {
		return nil
	}
	p := filepath.Join(cwd, ".cove.json")
	data, err := readProjectConfigFile(p)
	if err != nil {
		if errors.Is(err, errProjectConfigRefused) {
			log.Warnf("%v", err)
		}
		return nil
	}
	var override Config
	if err := json.Unmarshal(stripBOM(data), &override); err != nil {
		return fmt.Errorf("parse project config %s: %w", p, err)
	}
	if abs, err := filepath.Abs(p); err == nil {
		p = abs
	}
	p = filepath.Clean(p)
	hash := contentHash(data)
	cfg.projectConfigPath, cfg.projectConfigHash = p, hash

	// trusted is looked up only when a sensitive field is present, so the
	// common .cove.json (model, effort ...) never reads the trust store. An
	// unreadable store counts as "not trusted".
	var trustChecked, trusted bool
	var ignored []string
	sensitive := func(field string) bool {
		if !trustChecked {
			trustChecked = true
			trusted, _ = isTrustedHash(p, hash)
		}
		if !trusted {
			ignored = append(ignored, field)
		}
		return trusted
	}

	if override.Model != "" {
		cfg.Model = override.Model
	}
	if override.ModelFast != "" {
		cfg.ModelFast = override.ModelFast
	}
	// "auto" and "bypass" run tools without asking; only a mode at least as
	// careful as the default needs no trust.
	if override.PermissionMode != "" && (restrictivePermissionMode(override.PermissionMode) || sensitive("permission_mode")) {
		cfg.PermissionMode = override.PermissionMode
	}
	// Raising the spend cap spends the user's money; lowering it is safe.
	if override.MaxBudgetUsd > 0 {
		raises := cfg.MaxBudgetUsd > 0 && override.MaxBudgetUsd > cfg.MaxBudgetUsd
		if !raises || sensitive("max_budget_usd") {
			cfg.MaxBudgetUsd = override.MaxBudgetUsd
		}
	}
	// The system prompt steers every tool call the model makes.
	if override.SystemPrompt != "" && sensitive("system_prompt") {
		cfg.SystemPrompt = override.SystemPrompt
	}
	// MCP servers are processes started at launch.
	if len(override.MCPServers) > 0 && sensitive("mcp_servers") {
		cfg.MCPServers = override.MCPServers
	}
	// Shell commands run automatically after a turn.
	if len(override.DoneVerifyCommands) > 0 && sensitive("done_verify_commands") {
		cfg.DoneVerifyCommands = override.DoneVerifyCommands
	}
	// Automatic verification runs the project's own build and tests (cargo
	// build scripts, a node_modules tsc, go test); turning it on needs trust,
	// turning it off does not.
	if override.DoneVerifyAuto != nil && (!*override.DoneVerifyAuto || sensitive("done_verify_auto")) {
		cfg.DoneVerifyAuto = override.DoneVerifyAuto
	}
	if override.DoneVerifyTimeoutSeconds > 0 {
		cfg.DoneVerifyTimeoutSeconds = override.DoneVerifyTimeoutSeconds
	}
	if override.DoneVerifyTests != nil && (!*override.DoneVerifyTests || sensitive("done_verify_tests")) {
		cfg.DoneVerifyTests = override.DoneVerifyTests
	}
	if override.DoneSelfReview != "" {
		cfg.DoneSelfReview = override.DoneSelfReview
	}
	if override.DoneCheck != "" {
		cfg.DoneCheck = override.DoneCheck
	}
	if override.Thinking != "" {
		cfg.Thinking = override.Thinking
	}
	if override.Effort != "" {
		cfg.Effort = override.Effort
	}
	if override.ShowReasoning {
		cfg.ShowReasoning = true
	}
	if len(override.DisabledSkills) > 0 {
		cfg.DisabledSkills = override.DisabledSkills
	}
	// Its base_url defaults to the provider's and its api_key to the user's:
	// an endpoint from the project would receive the key and the memories.
	if override.MemoryEmbedding != nil && sensitive("memory_embedding") {
		cfg.MemoryEmbedding = override.MemoryEmbedding
	}
	if override.ExperimentalTools {
		cfg.ExperimentalTools = true
	}
	// Carries an API key and decides where search queries are sent.
	if override.WebSearch != nil && sensitive("web_search") {
		cfg.WebSearch = override.WebSearch
	}
	// Provider and ThinkingTokens were silently dropped here, so a project that
	// pinned its own endpoint or thinking budget in .cove.json was ignored with
	// no message — the user's setting simply had no effect. The provider is
	// sensitive: a base_url from the project received the user's global API
	// key, and a name or key from it redirects the session's traffic.
	if override.Provider.Name != "" && sensitive("provider.name") {
		cfg.Provider.Name = override.Provider.Name
	}
	if override.Provider.APIKey != "" && sensitive("provider.api_key") {
		cfg.Provider.APIKey = override.Provider.APIKey
	}
	if override.Provider.BaseURL != "" && sensitive("provider.base_url") {
		cfg.Provider.BaseURL = override.Provider.BaseURL
	}
	if override.Provider.ImageFilesAPI != nil && sensitive("provider.image_files_api") {
		value := *override.Provider.ImageFilesAPI
		cfg.Provider.ImageFilesAPI = &value
	}
	if override.ThinkingTokens > 0 {
		cfg.ThinkingTokens = override.ThinkingTokens
	}
	// The per-turn limits and max_sessions were only read from config.json.
	if override.MaxIterations > 0 {
		cfg.MaxIterations = override.MaxIterations
	}
	if override.SubagentMaxIterations > 0 {
		cfg.SubagentMaxIterations = override.SubagentMaxIterations
	}
	// Saved sessions beyond max_sessions are deleted: a project value below
	// the user's own would delete the user's sessions.
	if override.MaxSessions != 0 {
		cur := cfg.MaxSessions
		if cur == 0 {
			cur = DefaultMaxSessions
		}
		prunesMore := override.MaxSessions > 0 && (cur < 0 || override.MaxSessions < cur)
		if !prunesMore || sensitive("max_sessions") {
			cfg.MaxSessions = override.MaxSessions
		}
	}
	// max_turn_minutes has no omitempty and 0 means "off": only the key's
	// presence tells an explicit value from an absent one.
	if hasKey(stripBOM(data), "max_turn_minutes") {
		cfg.MaxTurnMinutes = override.MaxTurnMinutes
		cfg.turnMinutesExplicit = true
	}
	cfg.untrustedFields = ignored
	return nil
}

// restrictivePermissionMode reports whether mode asks at least as often as
// the default mode.
func restrictivePermissionMode(mode string) bool {
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case "default", "plan":
		return true
	}
	return false
}

func applyProfile(cfg *Config, prof *Profile) {
	if prof == nil {
		return
	}
	if prof.Model != "" {
		cfg.Model = prof.Model
	}
	if prof.ModelFast != "" {
		cfg.ModelFast = prof.ModelFast
	}
	if prof.Provider != nil {
		cfg.Provider = *prof.Provider
	}
	if prof.PermissionMode != "" {
		cfg.PermissionMode = prof.PermissionMode
	}
	if prof.MaxBudgetUsd > 0 {
		cfg.MaxBudgetUsd = prof.MaxBudgetUsd
	}
	if prof.ThinkingTokens > 0 {
		cfg.ThinkingTokens = prof.ThinkingTokens
	}
	if prof.Debug != nil {
		cfg.Debug = *prof.Debug
	}
	if prof.Verbose != nil {
		cfg.Verbose = *prof.Verbose
	}
	if prof.SystemPrompt != "" {
		cfg.SystemPrompt = prof.SystemPrompt
	}
	if prof.MaxIterations > 0 {
		cfg.MaxIterations = prof.MaxIterations
	}
	if prof.SubagentMaxIterations > 0 {
		cfg.SubagentMaxIterations = prof.SubagentMaxIterations
	}
	if prof.MaxSessions != 0 {
		cfg.MaxSessions = prof.MaxSessions
	}
	if prof.MaxTurnMinutes != nil {
		cfg.MaxTurnMinutes = *prof.MaxTurnMinutes
		cfg.turnMinutesExplicit = true
	}
}

func applyDefaults(cfg *Config) {
	normalizeConfig(cfg)
	if cfg.Model == "" || strings.EqualFold(cfg.Model, "auto") {
		cfg.Model = DefaultModelForProvider(cfg.Provider.Name)
	}
	if cfg.ModelFast == "" || strings.EqualFold(cfg.ModelFast, "auto") {
		// No fast model configured  - reuse the main model. Routing a "simple"
		// task to the same model is a no-op, which is correct and provider-safe.
		// (Previously this hardcoded deepseek-v4-flash for every provider, which
		// broke routing whenever the active provider wasn't deepseek.)
		cfg.ModelFast = cfg.Model
	}
	if cfg.PermissionMode == "" {
		cfg.PermissionMode = "default"
	}
	if cfg.ThinkingTokens < 1024 {
		cfg.ThinkingTokens = 16000
	}
	if cfg.MaxSessions == 0 {
		cfg.MaxSessions = DefaultMaxSessions
	}
	if cfg.MaxIterations <= 0 {
		cfg.MaxIterations = DefaultMaxIterations
	}
	if cfg.SubagentMaxIterations <= 0 {
		cfg.SubagentMaxIterations = DefaultSubagentMaxIterations
	}
	// MaxTurnMinutes starts at its default (DefaultConfig) and only an
	// explicit value in a config file changes it; 0 is "off", not "unset".
	if cfg.MaxTurnMinutes < 0 {
		cfg.MaxTurnMinutes = 0
	}
}

func normalizeConfig(cfg *Config) {
	cfg.Model = strings.TrimSpace(cfg.Model)
	cfg.PermissionMode = strings.TrimSpace(cfg.PermissionMode)
	cfg.SystemPrompt = strings.TrimSpace(cfg.SystemPrompt)
	cfg.Provider.Name = strings.TrimSpace(cfg.Provider.Name)
	cfg.Provider.APIKey = strings.TrimSpace(cfg.Provider.APIKey)
	cfg.Provider.BaseURL = strings.TrimSpace(cfg.Provider.BaseURL)
	// Clear keys that look masked (contain ****) to force env-var fallback.
	// This heals config files corrupted by earlier versions that saved masked keys.
	if strings.Contains(cfg.Provider.APIKey, "****") {
		cfg.Provider.APIKey = ""
	}
}

func DefaultModelForProvider(providerName string) string {
	switch api.NormalizeProviderName(providerName) {
	case "deepseek":
		return "deepseek-v4-pro"
	case "openai", "openai-compatible":
		return "gpt-4o"
	default:
		return "claude-sonnet-4-20250514"
	}
}

func ResolveModelForProvider(model, providerName string) string {
	model = strings.TrimSpace(model)
	if model == "" || strings.EqualFold(model, "auto") {
		return DefaultModelForProvider(providerName)
	}
	return model
}

// Save writes the settings cfg changed since Load into ~/.cove/config.json.
//
// It used to serialize the whole Config over the file. Because Load merges
// .cove.json, the active profile and the defaults into cfg, one /model in a
// cloned repo copied that repo's base_url, MCP servers and permission mode
// into the global config, the active profile's values leaked into the top
// level, keys this version does not know were dropped, and two cove windows
// reverted each other's changes. Now the file is re-read and only the fields
// that differ from what Load returned are replaced (see loadedView). A file
// that no longer parses is left alone: overwriting it with defaults destroyed
// the user's whole config, API key included.
func Save(cfg *Config) error {
	dir, err := ConfigDir()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	path := filepath.Join(dir, "config.json")

	view, err := rawView(cfg)
	if err != nil {
		return err
	}
	// The applied profile was deleted this session (/profile delete of the
	// active one). Its values are still in effect but have no profile to
	// live in any more, and against loadedView they look unchanged, so
	// neither the delete nor a later `/model <same value>` wrote them: the
	// next start silently fell back to the top-level values. They go to
	// the top level now, so the file says what is running.
	if cfg.appliedProfile != "" && cfg.Profiles[cfg.appliedProfile] == nil {
		for _, k := range profileOwnedKeys(cfg.appliedProf) {
			delete(cfg.loadedView, k)
		}
		cfg.appliedProfile, cfg.appliedProf = "", nil
	}
	// A change to a field the applied profile sets goes into that profile;
	// written at the top level, applyProfile overrode it again on the next
	// start and /model or /api-key was silently lost.
	write := view
	// Recomputed whenever the profile was touched, not only when a key is
	// still owned: clearing a profile-owned field to a value the profile
	// cannot hold (budget 0) changed prof but left owned empty, so the
	// profiles blob written was the one rendered before the change, and the
	// next start applied the old value again.
	if owned, touched := syncAppliedProfile(cfg, view); touched {
		if view, err = rawView(cfg); err != nil {
			return err
		}
		write = make(map[string]json.RawMessage, len(view))
		for k, v := range view {
			write[k] = v
		}
		for _, k := range owned {
			if old, ok := cfg.loadedView[k]; ok {
				write[k] = old
			} else {
				delete(write, k)
			}
		}
	}
	onDisk := map[string]json.RawMessage{}
	data, err := os.ReadFile(path)
	switch {
	case err == nil:
		if body := stripBOM(data); len(bytes.TrimSpace(body)) > 0 {
			if err := json.Unmarshal(body, &onDisk); err != nil {
				return fmt.Errorf("%s is not valid JSON (%w); fix or delete it first, it was not overwritten", path, err)
			}
			if onDisk == nil { // the file said "null"
				onDisk = map[string]json.RawMessage{}
			}
		}
	case !errors.Is(err, fs.ErrNotExist):
		return err
	}

	mergeChanges(onDisk, cfg.loadedView, write)
	out, err := json.MarshalIndent(onDisk, "", "  ")
	if err != nil {
		return err
	}
	// Atomic replace: a crash mid-write used to leave a truncated config.json.
	// It also applies 0600 every time, where os.WriteFile only did so when it
	// created the file, so a pre-existing 0644 file holding the key stayed so.
	if err := fsatomic.WriteFile(path, out, 0o600); err != nil {
		return err
	}
	cfg.loadedView = view
	return nil
}

// syncAppliedProfile copies into the applied profile each top-level field that
// changed since Load and that the profile sets, and returns those fields' keys:
// the top level must keep its own value for them.
//
// touched reports whether the profile was changed at all; owned lists the
// keys the profile still sets afterwards (kept at their loaded top-level
// value in the file).
func syncAppliedProfile(cfg *Config, view map[string]json.RawMessage) (owned []string, touched bool) {
	if cfg.loadedView == nil || cfg.appliedProfile == "" {
		return nil, false
	}
	prof := cfg.Profiles[cfg.appliedProfile]
	if prof == nil {
		return nil, false
	}
	changed := func(k string) bool { return !bytes.Equal(cfg.loadedView[k], view[k]) }
	// A value the profile cannot hold (0 for max_budget_usd, "" for model)
	// clears the field there and goes to the top level as well, so that is
	// what the next start sees; hence sets is asked again after apply.
	own := func(k string, sets func() bool, apply func()) {
		if sets() && changed(k) {
			apply()
			touched = true
			if sets() {
				owned = append(owned, k)
			}
		}
	}
	own("model", func() bool { return prof.Model != "" }, func() { prof.Model = cfg.Model })
	own("model_fast", func() bool { return prof.ModelFast != "" }, func() { prof.ModelFast = cfg.ModelFast })
	own("provider", func() bool { return prof.Provider != nil }, func() {
		// The profile's provider replaces cfg.Provider as a whole, so every
		// provider field belongs to it, even ones the profile left empty.
		p := cfg.Provider
		p.APIKeys = nil
		prof.Provider = &p
	})
	own("permission_mode", func() bool { return prof.PermissionMode != "" }, func() { prof.PermissionMode = cfg.PermissionMode })
	own("max_budget_usd", func() bool { return prof.MaxBudgetUsd > 0 }, func() { prof.MaxBudgetUsd = cfg.MaxBudgetUsd })
	own("thinking_tokens", func() bool { return prof.ThinkingTokens > 0 }, func() { prof.ThinkingTokens = cfg.ThinkingTokens })
	own("debug", func() bool { return prof.Debug != nil }, func() { v := cfg.Debug; prof.Debug = &v })
	own("verbose", func() bool { return prof.Verbose != nil }, func() { v := cfg.Verbose; prof.Verbose = &v })
	own("system_prompt", func() bool { return prof.SystemPrompt != "" }, func() { prof.SystemPrompt = cfg.SystemPrompt })
	own("max_iterations", func() bool { return prof.MaxIterations > 0 }, func() { prof.MaxIterations = cfg.MaxIterations })
	own("max_turn_minutes", func() bool { return prof.MaxTurnMinutes != nil }, func() { v := cfg.MaxTurnMinutes; prof.MaxTurnMinutes = &v })
	own("subagent_max_iterations", func() bool { return prof.SubagentMaxIterations > 0 }, func() { prof.SubagentMaxIterations = cfg.SubagentMaxIterations })
	own("max_sessions", func() bool { return prof.MaxSessions != 0 }, func() { prof.MaxSessions = cfg.MaxSessions })
	return owned, touched
}

// mergeChanges copies into dst every top-level field of cur that differs from
// base, and removes the ones cur no longer has. Keys dst has that neither
// knows about are kept. A nil base (a Config not built by Load) writes all.
func mergeChanges(dst, base, cur map[string]json.RawMessage) {
	for k, v := range cur {
		if old, ok := base[k]; ok && bytes.Equal(old, v) {
			continue
		}
		if k == "provider" {
			// Field by field: a project's base_url must not ride along with
			// a global /api-key change.
			mergeProvider(dst, base[k], v, base == nil)
			continue
		}
		dst[k] = v
	}
	if base == nil {
		return
	}
	for k := range base {
		if _, ok := cur[k]; !ok {
			delete(dst, k)
		}
	}
}

func mergeProvider(dst map[string]json.RawMessage, base, cur json.RawMessage, writeAll bool) {
	// No baseline (Save dropped loadedView["provider"] because the applied
	// profile was deleted): every field is written, and a field the profile
	// did not set is removed from disk instead of surviving next to the
	// profile's name and key (a stale base_url sent the new key elsewhere).
	if len(base) == 0 {
		writeAll = true
	}
	var disk, was, now map[string]json.RawMessage
	_ = json.Unmarshal(dst["provider"], &disk)
	if disk == nil {
		disk = map[string]json.RawMessage{}
	}
	_ = json.Unmarshal(base, &was)
	_ = json.Unmarshal(cur, &now)
	for _, k := range []string{"name", "api_key", "base_url", "image_files_api"} {
		v, ok := now[k]
		if !writeAll && bytes.Equal(was[k], v) {
			continue
		}
		if ok {
			disk[k] = v
		} else {
			delete(disk, k)
		}
	}
	dst["provider"], _ = json.Marshal(disk)
}

// MarshalRaw is cfg as a config.json to be loaded by another cove process:
// every field under its JSON name with the API keys in full. json.Marshal(cfg)
// writes the masked key (ProviderConfig.MarshalJSON is for display), which a
// child then clears on load and runs without credentials.
func (c *Config) MarshalRaw() ([]byte, error) {
	view, err := rawView(c)
	if err != nil {
		return nil, err
	}
	return json.Marshal(view)
}

// rawView is cfg as it is written to disk: each field under its JSON name, the
// API keys in full (ProviderConfig.MarshalJSON masks them for display).
func rawView(cfg *Config) (map[string]json.RawMessage, error) {
	data, err := json.Marshal(cfg)
	if err != nil {
		return nil, err
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, err
	}

	// Re-marshal provider using rawProvider  - no masking MarshalJSON.
	providerRaw, err := json.Marshal(rawProvider{
		Name:          cfg.Provider.Name,
		APIKey:        cfg.Provider.APIKey,
		BaseURL:       cfg.Provider.BaseURL,
		ImageFilesAPI: cfg.Provider.ImageFilesAPI,
	})
	if err != nil {
		return nil, err
	}
	m["provider"] = providerRaw

	// Each profile is written in full. It used to be rebuilt by hand from a
	// field list that stopped at system_prompt, and Save replaces the whole
	// "profiles" key, so any profile change wiped max_iterations,
	// max_turn_minutes: 0 ... and unknown keys from every profile on disk.
	if len(cfg.Profiles) > 0 {
		profilesRaw := make(map[string]json.RawMessage, len(cfg.Profiles))
		for name, prof := range cfg.Profiles {
			if prof == nil {
				profilesRaw[name] = json.RawMessage("{}")
				continue
			}
			if profilesRaw[name], err = prof.rawJSON(); err != nil {
				return nil, err
			}
		}
		if m["profiles"], err = json.Marshal(profilesRaw); err != nil {
			return nil, err
		}
	}
	return m, nil
}

// rawProvider mirrors ProviderConfig fields without the masking MarshalJSON method.
// Used by Save to write the full API key to disk.
type rawProvider struct {
	Name          string `json:"name"`
	APIKey        string `json:"api_key,omitempty"`
	BaseURL       string `json:"base_url,omitempty"`
	ImageFilesAPI *bool  `json:"image_files_api,omitempty"`
}

func (c *Config) EffectiveProvider() ProviderConfig {
	pc := c.Provider
	if pc.Name == "" {
		pc.Name = "anthropic"
	}
	pc.Name = api.NormalizeProviderName(pc.Name)
	if pc.APIKey == "" {
		pc.APIKey = firstEnv(api.ProviderEnvCandidates(pc.Name)...)
	}
	if pc.BaseURL == "" {
		pc.BaseURL = os.Getenv("LLM_BASE_URL")
	}
	if pc.BaseURL == "" && api.IsOpenAICompatibleProvider(pc.Name) {
		pc.BaseURL = api.DefaultBaseURL(pc.Name)
	}
	return pc
}

func firstEnv(keys ...string) string {
	for _, k := range keys {
		if v := os.Getenv(k); v != "" {
			return v
		}
	}
	return ""
}

// profileOwnedKeys lists the config.json keys prof sets (the fields
// applyProfile copies), as the keys syncAppliedProfile owns.
func profileOwnedKeys(prof *Profile) []string {
	if prof == nil {
		return nil
	}
	var keys []string
	add := func(k string, set bool) {
		if set {
			keys = append(keys, k)
		}
	}
	add("model", prof.Model != "")
	add("model_fast", prof.ModelFast != "")
	add("provider", prof.Provider != nil)
	add("permission_mode", prof.PermissionMode != "")
	add("max_budget_usd", prof.MaxBudgetUsd > 0)
	add("thinking_tokens", prof.ThinkingTokens > 0)
	add("debug", prof.Debug != nil)
	add("verbose", prof.Verbose != nil)
	add("system_prompt", prof.SystemPrompt != "")
	add("max_iterations", prof.MaxIterations > 0)
	add("max_turn_minutes", prof.MaxTurnMinutes != nil)
	add("subagent_max_iterations", prof.SubagentMaxIterations > 0)
	add("max_sessions", prof.MaxSessions != 0)
	return keys
}
