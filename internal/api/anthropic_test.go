package api

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestAnthropicChatStreamParsesSSEDataFrames(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/messages" {
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		chunks := []string{
			"event: message_start\n",
			// Anthropic reports the prompt-side counters only in message_start;
			// message_delta carries output_tokens. Mirror that split here so the
			// test would catch a regression that drops message_start usage.
			"data: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_123\",\"usage\":{\"input_tokens\":12,\"output_tokens\":0}}}\n\n",
			"event: content_block_start\n",
			"data: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n",
			"event: content_block_delta\n",
			"data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"hello\"}}\n\n",
			"event: content_block_delta\n",
			"data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\" world\"}}\n\n",
			"event: message_delta\n",
			"data: {\"type\":\"message_delta\",\"usage\":{\"output_tokens\":3}}\n\n",
			"event: message_stop\n",
			"data: {\"type\":\"message_stop\"}\n\n",
		}
		for _, chunk := range chunks {
			if _, err := io.WriteString(w, chunk); err != nil {
				return
			}
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
		}
	}))
	defer server.Close()

	p := &anthropicProvider{
		apiKey:  "test-key",
		baseURL: server.URL + "/v1",
		client:  server.Client(),
	}

	var deltas []string
	resp, err := p.ChatStream(context.Background(), ChatRequest{
		Model:     "claude-sonnet-4-20250514",
		MaxTokens: 128,
		Messages:  []Message{{Role: "user", Content: "hi"}},
	}, func(ev StreamEvent) {
		if ev.Type == "delta" {
			deltas = append(deltas, ev.Delta)
		}
	})
	if err != nil {
		t.Fatalf("ChatStream returned error: %v", err)
	}
	if got, want := resp.Content, "hello world"; got != want {
		t.Fatalf("resp.Content = %q, want %q", got, want)
	}
	if got, want := strings.Join(deltas, ""), "hello world"; got != want {
		t.Fatalf("stream deltas = %q, want %q", got, want)
	}
	if resp.InputTokens != 12 || resp.OutputTokens != 3 {
		t.Fatalf("unexpected usage: in=%d out=%d", resp.InputTokens, resp.OutputTokens)
	}
}

func TestAnthropicChatStreamReturnsDecodeErrorForBrokenSSEPayload(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, "event: content_block_delta\n")
		fmt.Fprint(w, "data: {not-json}\n\n")
		fmt.Fprint(w, "event: message_stop\n")
		fmt.Fprint(w, "data: {\"type\":\"message_stop\"}\n\n")
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	}))
	defer server.Close()

	p := &anthropicProvider{
		apiKey:  "test-key",
		baseURL: server.URL + "/v1",
		client:  server.Client(),
	}

	_, err := p.ChatStream(context.Background(), ChatRequest{
		Model:     "claude-sonnet-4-20250514",
		MaxTokens: 128,
		Messages:  []Message{{Role: "user", Content: "hi"}},
	}, nil)
	if err == nil {
		t.Fatalf("expected decode error, got nil")
	}
	if !strings.Contains(err.Error(), "decode anthropic SSE") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestAnthropicConvertMessagesSupportsImageParts(t *testing.T) {
	p := &anthropicProvider{}
	msgs := p.convertMessages([]Message{{
		Role: "user",
		Parts: []MessagePart{
			{Type: "text", Text: "请描述这张图"},
			{Type: "image", MimeType: "image/jpeg", Data: "aGVsbG8="},
		},
	}}, true)

	if len(msgs) != 1 {
		t.Fatalf("expected 1 message, got %d", len(msgs))
	}
	if len(msgs[0].Content) != 2 {
		t.Fatalf("expected 2 content blocks, got %d", len(msgs[0].Content))
	}
	if got := msgs[0].Content[1].Type; got != "image" {
		t.Fatalf("second block type = %q, want image", got)
	}
	if got := msgs[0].Content[1].Source["media_type"]; got != "image/jpeg" {
		t.Fatalf("media_type = %#v, want image/jpeg", got)
	}
}

// A request made with thinking disabled (the wrap-up summary) must not carry
// the thinking blocks kept in the history; one made with thinking on keeps
// the last assistant turn's blocks verbatim.
func TestAnthropicConvertMessagesThinkingBlocksFollowTheRequest(t *testing.T) {
	p := &anthropicProvider{}
	block := json.RawMessage(`{"type":"thinking","thinking":"…","signature":"sig"}`)
	msgs := []Message{{Role: "assistant", Content: "x", ThinkingBlocks: []json.RawMessage{block}}}
	if out := p.convertMessages(msgs, true); len(out) != 1 || len(out[0].Content) != 2 || out[0].Content[0].Raw == nil {
		t.Fatalf("thinking on: blocks dropped: %+v", out)
	}
	if out := p.convertMessages(msgs, false); len(out) != 1 || len(out[0].Content) != 1 || out[0].Content[0].Type != "text" {
		t.Fatalf("thinking disabled: blocks still sent: %+v", out)
	}
}
