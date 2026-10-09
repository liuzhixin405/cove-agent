package api

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

type ToolCall struct {
	ID    string         `json:"id"`
	Name  string         `json:"name"`
	Input map[string]any `json:"input"`
	// ParseError is set when the provider returned tool-call arguments that
	// could not be parsed as JSON even after best-effort repair (see
	// tool_repair.go). When true, Input contains a single "_cove_parse_error"
	// key with a human-readable diagnostic instead of the model's real
	// arguments. Engine.executeTool checks this flag and returns a tool-result
	// error asking the model to resend the call with valid JSON, instead of
	// dispatching garbage input to the real tool.
	ParseError bool `json:"parse_error,omitempty"`
	// Extra is provider data that must travel back with the call unchanged:
	// Gemini's OpenAI-compatible API puts the thought signature of a
	// Gemini 3 tool call in "extra_content", and rejects the next request
	// (400 "missing a thought_signature") without it. Persisted with the
	// session so a resumed conversation keeps working.
	Extra json.RawMessage `json:"extra,omitempty"`
}

type MessagePart struct {
	Type     string `json:"type"` // text | image | file
	Text     string `json:"text,omitempty"`
	MimeType string `json:"mime_type,omitempty"`
	Data     string `json:"data,omitempty"` // base64 payload for image/file parts
	FileName string `json:"file_name,omitempty"`
}

type Message struct {
	Role             string        `json:"role"`
	Content          string        `json:"content,omitempty"`
	Parts            []MessagePart `json:"parts,omitempty"`
	ReasoningContent string        `json:"reasoning_content,omitempty"`
	ToolCalls        []ToolCall    `json:"tool_calls,omitempty"`
	ToolCallID       string        `json:"tool_call_id,omitempty"`
	Name             string        `json:"name,omitempty"`
	CacheControl     string        `json:"cache_control,omitempty"` // Anthropic prompt caching
	Synthetic        bool          `json:"synthetic,omitempty"`     // engine-injected (not from real user)
	// ThinkingBlocks are the provider's opaque reasoning blocks for an
	// assistant turn (Anthropic thinking / redacted_thinking), kept verbatim
	// because they must be passed back byte-for-byte on later requests.
	ThinkingBlocks []json.RawMessage `json:"thinking_blocks,omitempty"`
}

type ToolDef struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"input_schema"`
}

type ChatRequest struct {
	Model      string
	Messages   []Message
	System     string // additional system content appended after base
	SystemBase string // base system prompt (Anthropic: system field; OpenAI: first system message)
	Tools      []ToolDef
	MaxTokens  int
	// Thinking selects the provider's thinking mode ("adaptive", "disabled");
	// empty leaves the model default. Effort ("low".."max") controls depth.
	// Providers without such controls ignore both.
	Thinking string
	Effort   string
}

type ChatResponse struct {
	Content               string
	ReasoningContent      string
	ToolCalls             []ToolCall
	Model                 string
	InputTokens           int
	OutputTokens          int
	PromptCacheHitTokens  int
	PromptCacheMissTokens int
	// PromptCacheWriteTokens is the share of PromptCacheMissTokens written
	// into the prompt cache (Anthropic cache_creation_input_tokens), billed
	// at a premium over plain input.
	PromptCacheWriteTokens int
	ReasoningTokens        int
	StopReason             string
	RateLimitHeaders       http.Header // raw rate limit headers from response
	// ThinkingBlocks mirrors Message.ThinkingBlocks for the returned turn.
	ThinkingBlocks []json.RawMessage
}

type StreamEvent struct {
	Type      string    `json:"type"`
	Delta     string    `json:"delta,omitempty"`
	Reasoning string    `json:"reasoning,omitempty"`
	ToolCall  *ToolCall `json:"tool_call,omitempty"`
}

type StreamHandler func(event StreamEvent)

type Provider interface {
	Name() string
	DisplayName() string
	Chat(ctx context.Context, req ChatRequest) (*ChatResponse, error)
	ChatStream(ctx context.Context, req ChatRequest, handler StreamHandler) (*ChatResponse, error)
	Validate() error
}

// Capabilities is what a provider's API needs or offers that the engine has
// to act on. The engine used to compare Name() with "anthropic" for each of
// these, which broke silently for a wrapper, an alias, or a second provider
// with the same trait.
type Capabilities struct {
	// CacheBreakpoints: the API caches a prompt prefix only where the
	// request marks it (Anthropic cache_control), so the engine marks it.
	CacheBreakpoints bool
	// ToolsWithToolHistory: a history holding tool calls is rejected unless
	// the request defines tools, even when none may be called.
	ToolsWithToolHistory bool
	// Family names the model family ("anthropic", "openai-compatible") for
	// policies that depend on it, such as the done check's default.
	Family string
}

// CapabilitiesOf is p's Capabilities: those it declares
// (interface{ Capabilities() Capabilities }), else none of them.
func CapabilitiesOf(p Provider) Capabilities {
	if c, ok := p.(interface{ Capabilities() Capabilities }); ok {
		return c.Capabilities()
	}
	return Capabilities{Family: "openai-compatible"}
}

type ProviderConfig struct {
	Name          string
	APIKey        string
	APIKeys       []string
	BaseURL       string
	ImageFilesAPI bool
}

func NewProvider(cfg ProviderConfig) Provider {
	cfg.Name = NormalizeProviderName(cfg.Name)
	if IsOpenAICompatibleProvider(cfg.Name) {
		return newOpenAICompatProvider(cfg)
	}
	return newAnthropicProvider(cfg)
}

func DetectProvider(model string, cfg ProviderConfig) Provider {
	if cfg.Name != "" {
		return NewProvider(cfg)
	}
	if containsAny(model, "deepseek") {
		cfg.Name = "deepseek"
		return newOpenAICompatProvider(cfg)
	}
	if containsAny(model, "gpt-", "o1-", "o3-", "o4-") {
		cfg.Name = "openai"
		return newOpenAICompatProvider(cfg)
	}
	cfg.Name = "anthropic"
	return newAnthropicProvider(cfg)
}

func containsAny(s string, substrs ...string) bool {
	for _, sub := range substrs {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}

// sharedTransport is a single process-wide HTTP transport reused by all
// providers. http.Transport is safe for concurrent use and maintains its own
// connection pool, so sharing one instance avoids duplicate pools when multiple
// providers (or provider switches within a session) are created.
var (
	sharedTransportOnce sync.Once
	sharedTransport     *http.Transport
)

func defaultHTTPTransport() *http.Transport {
	sharedTransportOnce.Do(func() {
		sharedTransport = &http.Transport{
			// A hand-built Transport has no proxy unless told; without this
			// HTTPS_PROXY / HTTP_PROXY / NO_PROXY were silently ignored.
			Proxy:               http.ProxyFromEnvironment,
			TLSHandshakeTimeout: 10 * time.Second,
			DialContext: (&net.Dialer{
				Timeout:   10 * time.Second,
				KeepAlive: 60 * time.Second,
			}).DialContext,
			TLSClientConfig:       &tls.Config{MinVersion: tls.VersionTLS12},
			MaxIdleConns:          50,
			MaxIdleConnsPerHost:   20,
			MaxConnsPerHost:       30,
			IdleConnTimeout:       120 * time.Second,
			ResponseHeaderTimeout: 180 * time.Second,
			DisableCompression:    false,
			ForceAttemptHTTP2:     true,
		}
	})
	return sharedTransport
}

// streamIdleTimeout is the maximum time a streaming response may go without
// producing any new data before it is considered stalled. The streaming HTTP
// clients intentionally have no overall Timeout (so long answers aren't cut
// off), so this watchdog is what prevents the UI from hanging forever on
// "思考中..." when a connection silently drops or the server stops sending.
// A var so tests can shorten it.
var streamIdleTimeout = 180 * time.Second

// ErrStreamStalled is what a streamed request returns when the idle watchdog
// ended it: the body stopped producing data for streamIdleTimeout. It is not a
// context.Canceled, which the engine reports as the user's own cancel.
var ErrStreamStalled = errors.New("stream stalled")

// streamStalledError is the error for a stream the watchdog aborted.
func streamStalledError() error {
	return fmt.Errorf("%w: no data received for %s", ErrStreamStalled, streamIdleTimeout)
}

// newStreamWatchdog derives a context that is cancelled if markProgress is not
// called within streamIdleTimeout. Build the streaming HTTP request with the
// returned context so that a stall aborts the blocking body read. Call
// markProgress once the response headers have arrived and on every received
// chunk, and defer stop to release resources.
//
// The clock starts at the first markProgress, not here. It used to start
// before the request was sent, so waiting for the headers (a local server's
// prefill, which may take minutes) and the connect retries' backoff (a 429's
// Retry-After) counted as idle, and the stream ended in a bare
// context.Canceled. The connection phase has its own limits: the
// transport's ResponseHeaderTimeout and the retry schedule.
func newStreamWatchdog(parent context.Context) (ctx context.Context, markProgress func(), stop func()) {
	ctx, cancel := context.WithCancel(parent)
	// Read once, on the caller's goroutine: the loop below may outlive the
	// request, and a test that shortened the variable restores it meanwhile.
	idle := streamIdleTimeout
	timer := time.NewTimer(idle)
	timer.Stop() // armed by the first markProgress
	progress := make(chan struct{}, 1)
	done := make(chan struct{})
	go func() {
		defer timer.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-done:
				return
			case <-timer.C:
				cancel() // stalled: abort the in-flight read
				return
			case <-progress:
				if !timer.Stop() {
					select {
					case <-timer.C:
					default:
					}
				}
				timer.Reset(idle)
			}
		}
	}()
	markProgress = func() {
		select {
		case progress <- struct{}{}:
		default:
		}
	}
	var stopOnce sync.Once
	stop = func() {
		stopOnce.Do(func() {
			close(done)
			cancel()
		})
	}
	return ctx, markProgress, stop
}

type AgentRunResult struct {
	Output  string
	Cost    float64
	Steps   int
	Success bool
	Error   string
	// ExitReason is why the sub-agent stopped (delegate.Exit*: completed,
	// max_iterations, interrupted, error, loop); empty if the runner does
	// not report it.
	ExitReason string
	// Truncated reports that Output is a partial result.
	Truncated bool
}

type AgentRunner interface {
	Run(ctx context.Context, name string, task string) (*AgentRunResult, error)
	Register(name, description, prompt string)
}

type retryConfig struct {
	MaxRetries int
	BaseDelay  time.Duration
}

var defaultRetry = retryConfig{MaxRetries: 3, BaseDelay: 1 * time.Second}
