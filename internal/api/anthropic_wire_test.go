package api

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

// captureAnthropicBody sends req through the non-streaming Chat path against a
// stub server and returns the decoded JSON body the provider put on the wire.
// Asserting on the wire body (not on convertMessages' Go structs) is the point:
// the API contract is the JSON, and several bugs here were only visible there.
func captureAnthropicBody(t *testing.T, req ChatRequest) map[string]any {
	t.Helper()
	var body map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(raw, &body); err != nil {
			t.Errorf("request body is not JSON: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`)
	}))
	defer server.Close()

	p := &anthropicProvider{apiKey: "k", baseURL: server.URL + "/v1", client: server.Client()}
	if req.Model == "" {
		req.Model = "claude-opus-5"
	}
	if req.MaxTokens == 0 {
		req.MaxTokens = 64
	}
	if _, err := p.Chat(context.Background(), req); err != nil {
		t.Fatalf("Chat returned error: %v", err)
	}
	return body
}

func wireMessages(t *testing.T, body map[string]any) []map[string]any {
	t.Helper()
	raw, _ := body["messages"].([]any)
	out := make([]map[string]any, 0, len(raw))
	for _, m := range raw {
		out = append(out, m.(map[string]any))
	}
	return out
}

func wireBlocks(m map[string]any) []map[string]any {
	raw, _ := m["content"].([]any)
	out := make([]map[string]any, 0, len(raw))
	for _, b := range raw {
		out = append(out, b.(map[string]any))
	}
	return out
}

// The Messages API only knows the roles "user" and "assistant": tool results
// travel as tool_result blocks inside a user turn, and every result for one
// assistant turn belongs in a single user message. Sending role "tool" is a
// 400, which broke every tool-using conversation on the native provider.
func TestAnthropicWireToolResultsBecomeOneUserTurn(t *testing.T) {
	body := captureAnthropicBody(t, ChatRequest{Messages: []Message{
		{Role: "user", Content: "go"},
		{Role: "assistant", Content: "reading", ToolCalls: []ToolCall{
			{ID: "t1", Name: "read", Input: map[string]any{"path": "a.go"}},
			{ID: "t2", Name: "todoread", Input: map[string]any{}},
		}},
		{Role: "tool", ToolCallID: "t1", Name: "read", Content: "package a"},
		{Role: "tool", ToolCallID: "t2", Name: "todoread", Content: "Error: boom"},
		{Role: "user", Content: "[system: note]", Synthetic: true},
	}})

	msgs := wireMessages(t, body)
	var roles []string
	for _, m := range msgs {
		roles = append(roles, m["role"].(string))
	}
	if want := []string{"user", "assistant", "user"}; !reflect.DeepEqual(roles, want) {
		t.Fatalf("roles = %v, want %v", roles, want)
	}

	asst := wireBlocks(msgs[1])
	if len(asst) != 3 || asst[0]["type"] != "text" || asst[1]["type"] != "tool_use" || asst[2]["type"] != "tool_use" {
		t.Fatalf("assistant blocks = %v, want text then two tool_use", asst)
	}
	input, present := asst[2]["input"]
	if !present {
		t.Fatalf("tool_use without arguments dropped its required input field: %v", asst[2])
	}
	if m, ok := input.(map[string]any); !ok || len(m) != 0 {
		t.Fatalf("tool_use input = %#v, want {}", input)
	}

	results := wireBlocks(msgs[2])
	if len(results) != 3 {
		t.Fatalf("final user turn has %d blocks, want 2 tool_result + 1 text: %v", len(results), results)
	}
	if results[0]["type"] != "tool_result" || results[0]["tool_use_id"] != "t1" {
		t.Fatalf("block 0 = %v, want tool_result for t1", results[0])
	}
	if _, hasErr := results[0]["is_error"]; hasErr {
		t.Fatalf("successful result marked as error: %v", results[0])
	}
	if results[1]["tool_use_id"] != "t2" || results[1]["is_error"] != true {
		t.Fatalf("block 1 = %v, want tool_result for t2 with is_error=true", results[1])
	}
	if results[2]["type"] != "text" || results[2]["text"] != "[system: note]" {
		t.Fatalf("block 2 = %v, want the trailing text", results[2])
	}
}

func TestAnthropicWireToolImagesFollowToolResults(t *testing.T) {
	body := captureAnthropicBody(t, ChatRequest{Model: "deepseek-flash", Messages: []Message{
		{Role: "user", Content: "inspect screenshots"},
		{Role: "assistant", ToolCalls: []ToolCall{{ID: "read-1", Name: "read", Input: map[string]any{}}, {ID: "read-2", Name: "read", Input: map[string]any{}}}},
		{Role: "tool", ToolCallID: "read-1", Content: "Image: first.png"},
		{Role: "tool", ToolCallID: "read-2", Content: "Image: second.png"},
		{Role: "user", Synthetic: true, Content: "Tool image results", Parts: []MessagePart{{Type: "image", MimeType: "image/png", Data: "Zmlyc3Q="}, {Type: "image", MimeType: "image/png", Data: "c2Vjb25k"}}},
	}})
	messages := wireMessages(t, body)
	if len(messages) != 3 || messages[2]["role"] != "user" {
		t.Fatalf("unexpected message roles: %+v", messages)
	}
	blocks := wireBlocks(messages[2])
	if len(blocks) != 5 || blocks[0]["type"] != "tool_result" || blocks[0]["tool_use_id"] != "read-1" || blocks[1]["type"] != "tool_result" || blocks[1]["tool_use_id"] != "read-2" {
		t.Fatalf("tool results not before images: %+v", blocks)
	}
	for index, expected := range []string{"Zmlyc3Q=", "c2Vjb25k"} {
		block := blocks[index+3]
		source, _ := block["source"].(map[string]any)
		if block["type"] != "image" || source["type"] != "base64" || source["media_type"] != "image/png" || source["data"] != expected {
			t.Fatalf("image %d changed: %+v", index, block)
		}
	}
}

// Empty text blocks are rejected by the API; a message with nothing to say
// must not produce one.
func TestAnthropicWireNeverSendsEmptyTextBlocks(t *testing.T) {
	body := captureAnthropicBody(t, ChatRequest{Messages: []Message{
		{Role: "user", Content: "hi"},
		{Role: "assistant", Content: ""},
		{Role: "user", Content: "again"},
	}})
	for _, m := range wireMessages(t, body) {
		for _, b := range wireBlocks(m) {
			txt, _ := b["text"].(string)
			if b["type"] == "text" && strings.TrimSpace(txt) == "" {
				t.Fatalf("empty text block sent: %v", m)
			}
		}
		if len(wireBlocks(m)) == 0 {
			t.Fatalf("message with no content blocks sent: %v", m)
		}
	}
}

func TestAnthropicWireSendsThinkingAndEffortWhenConfigured(t *testing.T) {
	body := captureAnthropicBody(t, ChatRequest{
		Messages: []Message{{Role: "user", Content: "hi"}},
		Thinking: "adaptive",
		Effort:   "high",
	})
	thinking, _ := body["thinking"].(map[string]any)
	if thinking["type"] != "adaptive" || thinking["display"] != "summarized" {
		t.Fatalf("thinking = %v, want adaptive + summarized display", body["thinking"])
	}
	oc, _ := body["output_config"].(map[string]any)
	if oc["effort"] != "high" {
		t.Fatalf("output_config = %v, want effort high", body["output_config"])
	}

	disabled := captureAnthropicBody(t, ChatRequest{
		Messages: []Message{{Role: "user", Content: "hi"}},
		Thinking: "disabled",
	})
	if th, _ := disabled["thinking"].(map[string]any); th["type"] != "disabled" {
		t.Fatalf("thinking = %v, want disabled", disabled["thinking"])
	}
}

func TestAnthropicWireOmitsThinkingAndEffortByDefault(t *testing.T) {
	body := captureAnthropicBody(t, ChatRequest{Messages: []Message{{Role: "user", Content: "hi"}}})
	if _, ok := body["thinking"]; ok {
		t.Fatalf("thinking sent without being configured: %v", body["thinking"])
	}
	if _, ok := body["output_config"]; ok {
		t.Fatalf("output_config sent without being configured: %v", body["output_config"])
	}
}

// Extra system guidance belongs to the system prompt. Prepending it as
// messages[0] put volatile text in front of the whole history, so every change
// to it invalidated the cached conversation prefix.
func TestAnthropicWireKeepsExtraSystemOutOfMessages(t *testing.T) {
	body := captureAnthropicBody(t, ChatRequest{
		SystemBase: "base",
		System:     "guide",
		Messages:   []Message{{Role: "user", Content: "hi"}},
	})
	msgs := wireMessages(t, body)
	if len(msgs) != 1 || wireBlocks(msgs[0])[0]["text"] != "hi" {
		t.Fatalf("messages = %v, want only the real user turn", msgs)
	}
	sys, _ := body["system"].(string)
	if !strings.HasPrefix(sys, "base") || !strings.Contains(sys, "guide") {
		t.Fatalf("system = %q, want base followed by guide", sys)
	}
}

// Thinking blocks must be passed back complete and unmodified, ahead of the
// text and tool_use blocks of the same assistant turn.
func TestAnthropicWireEchoesThinkingBlocksFirstAndVerbatim(t *testing.T) {
	thinking := json.RawMessage(`{"type":"thinking","thinking":"plan","signature":"sig-1"}`)
	redacted := json.RawMessage(`{"type":"redacted_thinking","data":"opaque"}`)
	body := captureAnthropicBody(t, ChatRequest{Messages: []Message{
		{Role: "user", Content: "go"},
		{Role: "assistant", Content: "calling", ThinkingBlocks: []json.RawMessage{thinking, redacted},
			ToolCalls: []ToolCall{{ID: "t1", Name: "read", Input: map[string]any{"path": "a"}}}},
		{Role: "tool", ToolCallID: "t1", Content: "ok"},
	}})

	asst := wireBlocks(wireMessages(t, body)[1])
	var types []string
	for _, b := range asst {
		types = append(types, b["type"].(string))
	}
	if want := []string{"thinking", "redacted_thinking", "text", "tool_use"}; !reflect.DeepEqual(types, want) {
		t.Fatalf("assistant block order = %v, want %v", types, want)
	}
	if asst[0]["thinking"] != "plan" || asst[0]["signature"] != "sig-1" {
		t.Fatalf("thinking block altered: %v", asst[0])
	}
	if asst[1]["data"] != "opaque" {
		t.Fatalf("redacted_thinking block altered: %v", asst[1])
	}
}

func TestAnthropicChatCapturesThinkingBlocksVerbatim(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"model":"claude-opus-5","stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1},"content":[
			{"type":"thinking","thinking":"t","signature":"s"},
			{"type":"redacted_thinking","data":"xyz"},
			{"type":"text","text":"hi"}]}`)
	}))
	defer server.Close()
	p := &anthropicProvider{apiKey: "k", baseURL: server.URL + "/v1", client: server.Client()}

	resp, err := p.Chat(context.Background(), ChatRequest{Model: "claude-opus-5", MaxTokens: 64,
		Messages: []Message{{Role: "user", Content: "q"}}})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if resp.Content != "hi" {
		t.Fatalf("Content = %q, want hi", resp.Content)
	}
	want := []map[string]any{
		{"type": "thinking", "thinking": "t", "signature": "s"},
		{"type": "redacted_thinking", "data": "xyz"},
	}
	if got := decodeBlocks(t, resp.ThinkingBlocks); !reflect.DeepEqual(got, want) {
		t.Fatalf("ThinkingBlocks = %v, want %v", got, want)
	}
}

func TestAnthropicStreamCapturesThinkingBlocks(t *testing.T) {
	server := sseServer(t, []string{
		`{"type":"message_start","message":{"usage":{"input_tokens":5}}}`,
		`{"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":"","signature":""}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"step one"}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":"sig-abc"}}`,
		`{"type":"content_block_stop","index":0}`,
		`{"type":"content_block_start","index":1,"content_block":{"type":"text","text":""}}`,
		`{"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"answer"}}`,
		`{"type":"content_block_start","index":2,"content_block":{"type":"tool_use","id":"t1","name":"read","input":{}}}`,
		`{"type":"content_block_delta","index":2,"delta":{"type":"input_json_delta","partial_json":"{\"path\":\"x\"}"}}`,
		`{"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":9}}`,
	})
	defer server.Close()
	p := &anthropicProvider{apiKey: "k", baseURL: server.URL + "/v1", client: server.Client()}

	var reasoning strings.Builder
	resp, err := p.ChatStream(context.Background(), ChatRequest{Model: "claude-opus-5", MaxTokens: 64,
		Messages: []Message{{Role: "user", Content: "q"}}}, func(ev StreamEvent) {
		if ev.Type == "reasoning" {
			reasoning.WriteString(ev.Reasoning)
		}
	})
	if err != nil {
		t.Fatalf("ChatStream: %v", err)
	}
	want := []map[string]any{{"type": "thinking", "thinking": "step one", "signature": "sig-abc"}}
	if got := decodeBlocks(t, resp.ThinkingBlocks); !reflect.DeepEqual(got, want) {
		t.Fatalf("ThinkingBlocks = %v, want %v", got, want)
	}
	if reasoning.String() != "step one" {
		t.Fatalf("reasoning stream = %q, want %q", reasoning.String(), "step one")
	}
	if resp.Content != "answer" || len(resp.ToolCalls) != 1 || resp.ToolCalls[0].Input["path"] != "x" {
		t.Fatalf("content=%q toolCalls=%v", resp.Content, resp.ToolCalls)
	}
}

// A tool that takes no arguments streams no input_json_delta at all. That is a
// complete call, not a truncated one, and must not be dropped.
func TestAnthropicStreamKeepsToolCallWithoutArguments(t *testing.T) {
	server := sseServer(t, []string{
		`{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"t9","name":"todoread","input":{}}}`,
		`{"type":"content_block_stop","index":0}`,
		`{"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":3}}`,
	})
	defer server.Close()
	p := &anthropicProvider{apiKey: "k", baseURL: server.URL + "/v1", client: server.Client()}

	resp, err := p.ChatStream(context.Background(), ChatRequest{Model: "claude-opus-5", MaxTokens: 64,
		Messages: []Message{{Role: "user", Content: "q"}}}, nil)
	if err != nil {
		t.Fatalf("ChatStream: %v", err)
	}
	if len(resp.ToolCalls) != 1 || resp.ToolCalls[0].ID != "t9" {
		t.Fatalf("ToolCalls = %v, want the argument-less call t9", resp.ToolCalls)
	}
	if resp.ToolCalls[0].Input == nil {
		t.Fatalf("argument-less call has nil Input; want an empty map")
	}
}

func sseServer(t *testing.T, events []string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		for _, ev := range events {
			_, _ = io.WriteString(w, "data: "+ev+"\n\n")
		}
		_, _ = io.WriteString(w, "data: {\"type\":\"message_stop\"}\n\n")
	}))
}

func decodeBlocks(t *testing.T, raw []json.RawMessage) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, r := range raw {
		var m map[string]any
		if err := json.Unmarshal(r, &m); err != nil {
			t.Fatalf("thinking block is not valid JSON: %s", r)
		}
		out = append(out, m)
	}
	return out
}
