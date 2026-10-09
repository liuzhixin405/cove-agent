package api

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/liuzhixin405/cove-agent/internal/api/adapter"
	"github.com/liuzhixin405/cove-agent/internal/log"
	"github.com/liuzhixin405/cove-agent/internal/textutil"
)

type anthropicProvider struct {
	apiKey       string
	keyPool      *KeyPool
	baseURL      string
	client       *http.Client
	streamClient *http.Client
}

func newAnthropicProvider(cfg ProviderConfig) *anthropicProvider {
	if cfg.BaseURL == "" {
		cfg.BaseURL = "https://api.anthropic.com/v1"
	}
	transport := defaultHTTPTransport()
	var pool *KeyPool
	if len(cfg.APIKeys) > 1 {
		pool = NewKeyPool(cfg.APIKeys)
	} else if len(cfg.APIKeys) == 1 {
		cfg.APIKey = cfg.APIKeys[0]
	}
	return &anthropicProvider{
		apiKey:  cfg.APIKey,
		keyPool: pool,
		baseURL: normalizeAnthropicBaseURL(cfg.BaseURL),
		// A request that times out is not retried (see doChat): the model
		// may have generated for the whole timeout.
		client: &http.Client{
			Timeout:   anthropicChatTimeout,
			Transport: transport,
		},
		streamClient: &http.Client{
			Transport: transport,
		},
	}
}

// anthropicChatTimeout bounds one non-streaming request, like the streaming
// path's watchdog allows a long generation to finish.
const anthropicChatTimeout = 300 * time.Second

// isClientTimeout reports whether err is a request timing out (http.Client's
// Timeout, a deadline). Such a request may have had the model generate for
// the whole timeout, so it is not retried; other transport failures (a
// refused or reset connection) are.
func isClientTimeout(err error) bool {
	var ue *url.Error
	return errors.As(err, &ue) && ue.Timeout()
}

func (p *anthropicProvider) activeKey() string {
	if p.keyPool != nil {
		return p.keyPool.Get()
	}
	return p.apiKey
}

func (p *anthropicProvider) Name() string { return "anthropic" }
func (p *anthropicProvider) Capabilities() Capabilities {
	return Capabilities{CacheBreakpoints: true, ToolsWithToolHistory: true, Family: "anthropic"}
}
func (p *anthropicProvider) DisplayName() string { return "anthropic" }

// endpoint is the messages endpoint through client (p.client, or
// p.streamClient for a stream).
func (p *anthropicProvider) endpoint(client *http.Client) endpoint {
	return endpoint{url: p.baseURL + "/messages", client: client, key: p.activeKey, pool: p.keyPool,
		auth: func(h http.Header, key string) {
			h.Set("x-api-key", key)
			h.Set("anthropic-version", "2023-06-01")
			h.Set("anthropic-beta", "token-efficient-tools-2025-11-18,prompt-caching-2024-07-31")
		}}
}
func (p *anthropicProvider) Validate() error {
	if p.apiKey == "" && p.keyPool.size() == 0 {
		return fmt.Errorf("API key required (set ANTHROPIC_API_KEY)")
	}
	return nil
}

type anthropicContentBlock struct {
	Type         string            `json:"type"`
	Text         string            `json:"text,omitempty"`
	Source       map[string]any    `json:"source,omitempty"`
	ID           string            `json:"id,omitempty"`
	Name         string            `json:"name,omitempty"`
	Input        map[string]any    `json:"input,omitempty"`
	ToolUseID    string            `json:"tool_use_id,omitempty"`
	Content      any               `json:"content,omitempty"`
	IsError      *bool             `json:"is_error,omitempty"`
	CacheControl map[string]string `json:"cache_control,omitempty"`
	// Raw holds the exact JSON of a thinking / redacted_thinking block. Those
	// blocks carry a signature over the reasoning and must go back to the API
	// byte-for-byte, so they are never re-encoded from parsed fields.
	Raw json.RawMessage `json:"-"`
}

func isThinkingType(t string) bool { return t == "thinking" || t == "redacted_thinking" }

func (b *anthropicContentBlock) UnmarshalJSON(data []byte) error {
	type alias anthropicContentBlock
	var a alias
	if err := json.Unmarshal(data, &a); err != nil {
		return err
	}
	*b = anthropicContentBlock(a)
	if isThinkingType(b.Type) {
		b.Raw = append(json.RawMessage(nil), data...)
	}
	return nil
}

func (b anthropicContentBlock) MarshalJSON() ([]byte, error) {
	if len(b.Raw) > 0 {
		return b.Raw, nil
	}
	type alias anthropicContentBlock
	if b.Type == "tool_use" {
		// input is required on tool_use even when the tool takes no
		// arguments; omitempty would drop an empty map and get a 400.
		input := b.Input
		if input == nil {
			input = map[string]any{}
		}
		return json.Marshal(struct {
			alias
			Input map[string]any `json:"input"`
		}{alias(b), input})
	}
	return json.Marshal(alias(b))
}

type anthropicThinking struct {
	Type    string `json:"type"`
	Display string `json:"display,omitempty"`
}

type anthropicOutputConfig struct {
	Effort string `json:"effort,omitempty"`
}

type anthropicMsg struct {
	Role    string                  `json:"role"`
	Content []anthropicContentBlock `json:"content"`
}

type anthropicReq struct {
	Model     string           `json:"model"`
	MaxTokens int              `json:"max_tokens"`
	Messages  []anthropicMsg   `json:"messages"`
	System    string           `json:"system,omitempty"`
	Tools     []map[string]any `json:"tools,omitempty"`
	Stream    bool             `json:"stream"`

	Thinking     *anthropicThinking     `json:"thinking,omitempty"`
	OutputConfig *anthropicOutputConfig `json:"output_config,omitempty"`
}

// buildRequest assembles the wire request shared by Chat and ChatStream.
//
// req.System is appended to the system prompt rather than sent as a leading
// user message: anything placed before the history is part of every cached
// prefix, so volatile text there invalidates the whole conversation cache.
func (p *anthropicProvider) buildRequest(req ChatRequest, stream bool) anthropicReq {
	system := req.SystemBase
	if req.System != "" {
		if system != "" {
			system += "\n\n"
		}
		system += req.System
	}
	body := anthropicReq{
		Model:     req.Model,
		MaxTokens: req.MaxTokens,
		System:    system,
		Messages:  p.convertMessages(req.Messages, req.Thinking != "disabled"),
		Tools:     p.convertTools(req.Tools),
		Stream:    stream,
	}
	switch req.Thinking {
	case "adaptive":
		// summarized: the reasoning is surfaced to the user like other
		// providers' reasoning_content instead of arriving as empty blocks.
		body.Thinking = &anthropicThinking{Type: "adaptive", Display: "summarized"}
	case "disabled":
		body.Thinking = &anthropicThinking{Type: "disabled"}
	}
	if req.Effort != "" {
		body.OutputConfig = &anthropicOutputConfig{Effort: req.Effort}
	}
	return body
}

type anthropicUsage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
	// Prompt-caching counters. Anthropic reports cache activity separately from
	// input_tokens: cache_read_input_tokens are billed at a large discount and
	// cache_creation_input_tokens at a premium, and neither is included in
	// input_tokens. Ignoring them under-reports cost and lets MaxBudgetUsd drift.
	CacheReadInputTokens     int `json:"cache_read_input_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
}

// cacheHitTokens returns tokens served from the prompt cache (discounted).
func (u anthropicUsage) cacheHitTokens() int { return u.CacheReadInputTokens }

// cacheMissTokens returns non-cached prompt tokens, i.e. plain input plus the
// tokens written into the cache this request.
func (u anthropicUsage) cacheMissTokens() int {
	return u.InputTokens + u.CacheCreationInputTokens
}

// totalInputTokens returns every prompt token the request was billed for.
func (u anthropicUsage) totalInputTokens() int {
	return u.InputTokens + u.CacheReadInputTokens + u.CacheCreationInputTokens
}

// merge folds a later usage report into an earlier one. Anthropic splits usage
// across `message_start` (prompt-side counters, final) and `message_delta`
// (output_tokens, cumulative), so neither event alone is complete: taking only
// message_delta leaves InputTokens at 0 and loses all cost on the input side.
func (u *anthropicUsage) merge(next anthropicUsage) {
	if next.InputTokens > 0 {
		u.InputTokens = next.InputTokens
	}
	if next.OutputTokens > 0 {
		u.OutputTokens = next.OutputTokens
	}
	if next.CacheReadInputTokens > 0 {
		u.CacheReadInputTokens = next.CacheReadInputTokens
	}
	if next.CacheCreationInputTokens > 0 {
		u.CacheCreationInputTokens = next.CacheCreationInputTokens
	}
}

type anthropicResp struct {
	ID         string                  `json:"id"`
	Content    []anthropicContentBlock `json:"content"`
	Model      string                  `json:"model"`
	StopReason string                  `json:"stop_reason"`
	Usage      anthropicUsage          `json:"usage"`
}

func (p *anthropicProvider) Chat(ctx context.Context, req ChatRequest) (*ChatResponse, error) {
	body := p.buildRequest(req, false)
	return retryWithBackoff(ctx, defaultRetry, func() (*ChatResponse, error) {
		return p.doChat(ctx, body)
	})
}

func (p *anthropicProvider) doChat(ctx context.Context, body anthropicReq) (*ChatResponse, error) {
	data, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}
	httpResp, err := p.endpoint(p.client).post(ctx, data)
	if err != nil {
		return nil, err
	}
	raw, status := httpResp.Body, httpResp.Status
	if status != http.StatusOK {
		return nil, &StatusError{Status: status, Msg: truncate(string(raw), 500)}
	}

	var ar anthropicResp
	if err := json.Unmarshal(raw, &ar); err != nil {
		return nil, fmt.Errorf("decode: %w", err)
	}

	return &ChatResponse{
		Content:                p.extractContent(ar.Content),
		ToolCalls:              p.extractToolCalls(ar.Content),
		ThinkingBlocks:         extractThinkingBlocks(ar.Content),
		Model:                  ar.Model,
		InputTokens:            ar.Usage.totalInputTokens(),
		OutputTokens:           ar.Usage.OutputTokens,
		PromptCacheHitTokens:   ar.Usage.cacheHitTokens(),
		PromptCacheMissTokens:  ar.Usage.cacheMissTokens(),
		PromptCacheWriteTokens: ar.Usage.CacheCreationInputTokens,
		StopReason:             ar.StopReason,
		RateLimitHeaders:       httpResp.Header,
	}, nil
}

func (p *anthropicProvider) extractContent(blocks []anthropicContentBlock) string {
	var texts []string
	for _, b := range blocks {
		if b.Type == "text" {
			texts = append(texts, b.Text)
		}
	}
	return strings.Join(texts, "\n")
}

func extractThinkingBlocks(blocks []anthropicContentBlock) []json.RawMessage {
	var out []json.RawMessage
	for _, b := range blocks {
		if len(b.Raw) > 0 {
			out = append(out, b.Raw)
		}
	}
	return out
}

func (p *anthropicProvider) extractToolCalls(blocks []anthropicContentBlock) []ToolCall {
	var calls []ToolCall
	for _, b := range blocks {
		if b.Type == "tool_use" {
			calls = append(calls, ToolCall{ID: b.ID, Name: b.Name, Input: b.Input})
		}
	}
	return calls
}

// convertMessages builds the request's message list. withThinking is false
// for a request made with thinking disabled (the wrap-up summary): thinking
// blocks kept in the history are then left out, since they are only required
// (and only accepted) when thinking is on.
func (p *anthropicProvider) convertMessages(in []Message, withThinking bool) []anthropicMsg {
	var out []anthropicMsg
	// appendTurn folds consecutive same-role messages into one turn. The API
	// has only user and assistant roles: every tool result of one assistant
	// turn, plus any engine text that follows them, must arrive as a single
	// user message with the tool_result blocks first.
	appendTurn := func(role string, blocks []anthropicContentBlock, cache string) {
		if len(blocks) == 0 {
			return
		}
		if cache != "" {
			for i := len(blocks) - 1; i >= 0; i-- {
				if len(blocks[i].Raw) == 0 { // thinking blocks cannot carry cache_control
					blocks[i].CacheControl = map[string]string{"type": cache}
					break
				}
			}
		}
		if n := len(out); n > 0 && out[n-1].Role == role {
			out[n-1].Content = append(out[n-1].Content, blocks...)
			return
		}
		out = append(out, anthropicMsg{Role: role, Content: blocks})
	}
	for _, m := range in {
		switch m.Role {
		case "tool":
			block := anthropicContentBlock{Type: "tool_result", ToolUseID: m.ToolCallID, Content: m.Content}
			if isToolErrorContent(m.Content) {
				isErr := true
				block.IsError = &isErr
			}
			appendTurn("user", []anthropicContentBlock{block}, m.CacheControl)
		case "assistant":
			var blocks []anthropicContentBlock
			// Thinking first, verbatim, then text, then tool_use: the order
			// the model produced them in.
			if withThinking {
				for _, raw := range m.ThinkingBlocks {
					blocks = append(blocks, anthropicContentBlock{Raw: raw})
				}
			}
			if strings.TrimSpace(m.Content) != "" {
				blocks = append(blocks, anthropicContentBlock{Type: "text", Text: m.Content})
			}
			for _, tc := range m.ToolCalls {
				blocks = append(blocks, anthropicContentBlock{Type: "tool_use", ID: tc.ID, Name: tc.Name, Input: tc.Input})
			}
			appendTurn("assistant", blocks, m.CacheControl)
		default:
			appendTurn("user", convertAnthropicUserContent(m), m.CacheControl)
		}
	}
	return out
}

// isToolErrorContent recognises the engine's convention for failed tool
// results so they can be flagged with is_error.
func isToolErrorContent(content string) bool {
	return strings.HasPrefix(content, "Error:") || strings.HasPrefix(content, "Error (") ||
		strings.HasPrefix(content, "BLOCKED")
}

func convertAnthropicUserContent(m Message) []anthropicContentBlock {
	blocks := make([]anthropicContentBlock, 0, len(m.Parts)+1)
	if strings.TrimSpace(m.Content) != "" {
		blocks = append(blocks, anthropicContentBlock{Type: "text", Text: m.Content})
	}
	for _, part := range m.Parts {
		switch part.Type {
		case "image":
			if part.Data == "" {
				continue
			}
			mediaType := part.MimeType
			if mediaType == "" {
				mediaType = "image/png"
			}
			blocks = append(blocks, anthropicContentBlock{
				Type: "image",
				Source: map[string]any{
					"type":       "base64",
					"media_type": mediaType,
					"data":       part.Data,
				},
			})
		case "text", "file":
			if part.Text == "" {
				continue
			}
			blocks = append(blocks, anthropicContentBlock{Type: "text", Text: part.Text})
		}
	}
	return blocks
}

func (p *anthropicProvider) convertTools(tools []ToolDef) []map[string]any {
	var out []map[string]any
	for _, t := range tools {
		schema := t.InputSchema
		if schema == nil {
			schema = map[string]any{"type": "object", "properties": map[string]any{}}
		}
		out = append(out, map[string]any{
			"name": t.Name, "description": t.Description, "input_schema": schema,
		})
	}
	return out
}

// truncate clips s to at most n bytes without splitting a rune. n is a byte
// budget here: callers use it to bound error-response bodies, which are opaque
// and may be any encoding.
func truncate(s string, n int) string {
	return textutil.ClipBytes(s, n, "...")
}

type anthropicStreamBlock struct {
	Type         string                 `json:"type"`
	Index        int                    `json:"index"`
	Delta        *anthropicDelta        `json:"delta,omitempty"`
	ContentBlock *anthropicContentBlock `json:"content_block,omitempty"`
	Usage        *anthropicUsage        `json:"usage,omitempty"`
	// message_start nests the initial usage snapshot under "message".
	Message *anthropicStreamMessage `json:"message,omitempty"`
	// Error is set on an "error" event: a failure after the 200 header,
	// typically overloaded_error.
	Error *oaiStreamError `json:"error,omitempty"`
}

type anthropicStreamMessage struct {
	Usage *anthropicUsage `json:"usage,omitempty"`
}

type anthropicDelta struct {
	Type        string `json:"type"`
	Text        string `json:"text,omitempty"`
	PartialJSON string `json:"partial_json,omitempty"`
	StopReason  string `json:"stop_reason,omitempty"`
	Thinking    string `json:"thinking,omitempty"`
	Signature   string `json:"signature,omitempty"`
}

func (p *anthropicProvider) ChatStream(ctx context.Context, req ChatRequest, handler StreamHandler) (*ChatResponse, error) {
	body := p.buildRequest(req, true)

	data, _ := json.Marshal(body)

	sc := p.streamClient
	if sc == nil {
		sc = p.client
	}

	// Idle watchdog: abort the stream if no data arrives for streamIdleTimeout,
	// preventing the UI from hanging forever on a silently dropped connection.
	streamCtx, markProgress, stopWatchdog := newStreamWatchdog(ctx)
	defer stopWatchdog()

	httpResp, err := p.endpoint(sc).openStream(streamCtx, data)
	if err != nil {
		return nil, err
	}
	defer func() { _ = httpResp.Body.Close() }()
	markProgress() // headers arrived: the idle clock starts now
	if httpResp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(httpResp.Body, 4096))
		return nil, &StatusError{Status: httpResp.StatusCode, Msg: truncate(string(body), 500)}
	}

	reader := bufio.NewReader(httpResp.Body)
	var streamAcc adapter.StreamAccumulator
	type accumTC struct {
		ID      string
		Name    string
		JSONBuf strings.Builder
	}
	tcAccum := make(map[int]*accumTC)
	// Thinking blocks by content index. A thinking block streams its text and
	// signature as deltas; a redacted_thinking block arrives whole at start.
	type accumThinking struct {
		raw       json.RawMessage // complete block (redacted_thinking)
		text      strings.Builder
		signature strings.Builder
	}
	thinkAccum := make(map[int]*accumThinking)
	var usage anthropicUsage
	var stopReason string
	sawStop := false
	textBlocks := 0
	// outputFinal: output_tokens came from message_delta. message_start's
	// count is a placeholder (typically 1), not what was generated.
	outputFinal := false
	// partial is the return for a failure once the body has started: the
	// provider bills what it generated, so the error carries that usage for
	// the meter (withPartialUsage).
	partial := func(err error) (*ChatResponse, error) {
		generated := streamAcc.Content()
		for _, acc := range tcAccum {
			generated += acc.JSONBuf.String()
		}
		for _, acc := range thinkAccum {
			generated += acc.text.String()
		}
		out := 0
		if outputFinal {
			out = usage.OutputTokens
		}
		return nil, withPartialUsage(err, &ChatResponse{
			Model:                  req.Model,
			InputTokens:            usage.totalInputTokens(),
			OutputTokens:           out,
			PromptCacheHitTokens:   usage.cacheHitTokens(),
			PromptCacheMissTokens:  usage.cacheMissTokens(),
			PromptCacheWriteTokens: usage.CacheCreationInputTokens,
		}, generated)
	}

	for {
		line, err := readSSELine(reader)
		if err != nil && !errors.Is(err, io.EOF) {
			// Distinguish an idle-watchdog abort from a genuine read error.
			if streamCtx.Err() != nil && ctx.Err() == nil {
				return partial(streamStalledError())
			}
			return partial(fmt.Errorf("read anthropic SSE: %w", err))
		}
		if ctx.Err() != nil {
			return partial(ctx.Err())
		}
		markProgress() // reset the idle watchdog on every received line
		if payload, ok := sseDataPayload(line); ok {
			if payload != "" && payload != "[DONE]" {
				var ev anthropicStreamBlock
				if err := json.Unmarshal([]byte(payload), &ev); err != nil {
					return partial(fmt.Errorf("decode anthropic SSE: %w", err))
				}

				switch ev.Type {
				case "error":
					// Typed with the status the error type stands for, so
					// a streamed overloaded_error fails over and cools down
					// exactly like an HTTP 529 (see streamError).
					return partial(streamError(ev.Error))
				case "message_stop":
					sawStop = true
				case "message_start":
					// Prompt-side counters arrive here and nowhere else.
					if ev.Message != nil && ev.Message.Usage != nil {
						usage.merge(*ev.Message.Usage)
					}
					if ev.Usage != nil {
						usage.merge(*ev.Usage)
					}
				case "content_block_start":
					// Text blocks are joined with a newline, as Chat's
					// extractContent does: streamed "before"[tool]"after"
					// used to read "beforeafter" while the same reply
					// fetched without streaming read "before\nafter".
					if ev.ContentBlock != nil && ev.ContentBlock.Type == "text" {
						if textBlocks > 0 {
							streamAcc.AddDelta("\n")
							if handler != nil {
								handler(StreamEvent{Type: "delta", Delta: "\n"})
							}
						}
						textBlocks++
					}
					if ev.ContentBlock != nil && ev.ContentBlock.Type == "tool_use" {
						acc := &accumTC{ID: ev.ContentBlock.ID, Name: ev.ContentBlock.Name}
						tcAccum[ev.Index] = acc
					}
					if ev.ContentBlock != nil && isThinkingType(ev.ContentBlock.Type) {
						acc := &accumThinking{}
						if ev.ContentBlock.Type == "redacted_thinking" {
							acc.raw = ev.ContentBlock.Raw
						}
						thinkAccum[ev.Index] = acc
					}
				case "content_block_delta":
					if ev.Delta != nil {
						if ev.Delta.Type == "text_delta" && ev.Delta.Text != "" {
							streamAcc.AddDelta(ev.Delta.Text)
							if handler != nil {
								handler(StreamEvent{Type: "delta", Delta: ev.Delta.Text})
							}
						}
						if ev.Delta.Type == "input_json_delta" && ev.Delta.PartialJSON != "" {
							if acc, ok := tcAccum[ev.Index]; ok {
								acc.JSONBuf.WriteString(ev.Delta.PartialJSON)
							}
						}
						if acc, ok := thinkAccum[ev.Index]; ok {
							switch ev.Delta.Type {
							case "thinking_delta":
								acc.text.WriteString(ev.Delta.Thinking)
								if handler != nil && ev.Delta.Thinking != "" {
									handler(StreamEvent{Type: "reasoning", Reasoning: ev.Delta.Thinking})
								}
							case "signature_delta":
								acc.signature.WriteString(ev.Delta.Signature)
							}
						}
					}
				case "message_delta":
					if ev.Usage != nil {
						usage.merge(*ev.Usage)
						outputFinal = true
					}
					if ev.Delta != nil && ev.Delta.StopReason != "" {
						stopReason = ev.Delta.StopReason
					}
				}
			}
		}
		// message_stop is Anthropic's terminal event, like OpenAI's [DONE]:
		// the reply is complete whatever the connection does afterwards. The
		// loop used to keep reading, so a server that held the connection
		// open after message_stop tripped the idle watchdog and a finished
		// answer came back as "stream stalled" (retried, then failed over).
		if sawStop || errors.Is(err, io.EOF) {
			break
		}
	}
	if streamCtx.Err() != nil && ctx.Err() == nil && !sawStop {
		return partial(streamStalledError())
	}
	// No message_stop and no stop_reason: the connection closed in the middle
	// of the answer, which used to be reported as a complete end_turn.
	if !sawStop && stopReason == "" {
		return partial(fmt.Errorf("stream ended before the response completed (unexpected EOF: no message_stop)"))
	}

	indices := make([]int, 0, len(tcAccum))
	for idx := range tcAccum {
		indices = append(indices, idx)
	}
	sort.Ints(indices)
	for _, idx := range indices {
		acc := tcAccum[idx]
		rawJSON := acc.JSONBuf.String()
		if rawJSON == "" {
			if stopReason == "max_tokens" {
				// The call was cut off before its arguments arrived.
				// log, not os.Stderr: this is library code, and a direct terminal
				// write from here corrupts a front end that manages its own frame.
				log.Warnf("tool %s: empty input (response truncated, stop=%s)", acc.Name, stopReason)
				continue
			}
			// A tool that takes no arguments streams no input_json_delta at
			// all; that is a complete call with an empty input.
			streamAcc.AddToolCall(adapter.ToolCall{ID: acc.ID, Name: acc.Name, Input: map[string]any{}})
			continue
		}
		input, ok := RepairToolArguments(rawJSON)
		if !ok {
			// Incomplete/malformed JSON even after best-effort repair (tool_repair.go).
			log.Warnf("tool %s: failed to parse input JSON even after repair (stop=%s, raw: %s)", acc.Name, stopReason, truncate(rawJSON, 200))
			streamAcc.AddToolCall(adapter.ToolCall{
				ID:         acc.ID,
				Name:       acc.Name,
				Input:      toolArgsParseError(rawJSON, stopReason == "max_tokens"),
				ParseError: true,
			})
			continue
		}
		streamAcc.AddToolCall(adapter.ToolCall{ID: acc.ID, Name: acc.Name, Input: input})
	}
	toolCalls := toAPIToolCalls(streamAcc.ToolCalls())

	thinkIdx := make([]int, 0, len(thinkAccum))
	for idx := range thinkAccum {
		thinkIdx = append(thinkIdx, idx)
	}
	sort.Ints(thinkIdx)
	var thinkingBlocks []json.RawMessage
	for _, idx := range thinkIdx {
		acc := thinkAccum[idx]
		if len(acc.raw) > 0 {
			thinkingBlocks = append(thinkingBlocks, acc.raw)
			continue
		}
		raw, err := json.Marshal(map[string]string{
			"type": "thinking", "thinking": acc.text.String(), "signature": acc.signature.String(),
		})
		if err == nil {
			thinkingBlocks = append(thinkingBlocks, raw)
		}
	}

	if stopReason == "" {
		stopReason = "end_turn"
	}

	return &ChatResponse{
		Content:                streamAcc.Content(),
		ToolCalls:              toolCalls,
		ThinkingBlocks:         thinkingBlocks,
		Model:                  req.Model,
		InputTokens:            usage.totalInputTokens(),
		OutputTokens:           usage.OutputTokens,
		PromptCacheHitTokens:   usage.cacheHitTokens(),
		PromptCacheMissTokens:  usage.cacheMissTokens(),
		PromptCacheWriteTokens: usage.CacheCreationInputTokens,
		StopReason:             stopReason,
		RateLimitHeaders:       httpResp.Header,
	}, nil
}
