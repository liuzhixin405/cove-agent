package api

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestOpenAICompatChatCarriesReasoningContentIntoResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{
			"model":"deepseek-v4-pro",
			"choices":[{
				"index":0,
				"message":{
					"role":"assistant",
					"content":"",
					"reasoning_content":"need tool first",
					"tool_calls":[{
						"id":"call_1",
						"type":"function",
						"function":{"name":"read_file","arguments":"{\"path\":\"README.md\"}"}
					}]
				}
			}],
			"usage":{"prompt_tokens":10,"completion_tokens":3}
		}`)
	}))
	defer server.Close()

	p := &openAICompatProvider{
		apiKey:  "test-key",
		baseURL: server.URL + "/v1",
		client:  server.Client(),
	}

	resp, err := p.Chat(context.Background(), ChatRequest{
		Model:     "deepseek-v4-pro",
		MaxTokens: 128,
		Messages:  []Message{{Role: "user", Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("Chat returned error: %v", err)
	}
	if got, want := resp.ReasoningContent, "need tool first"; got != want {
		t.Fatalf("ReasoningContent = %q, want %q", got, want)
	}
	if len(resp.ToolCalls) != 1 {
		t.Fatalf("expected 1 tool call, got %d", len(resp.ToolCalls))
	}
}

func TestOpenAICompatConvertMessagesStripsReasoningContent(t *testing.T) {
	p := &openAICompatProvider{}
	msgs := p.convertMessages([]Message{{
		Role:             "assistant",
		Content:          "",
		ReasoningContent: "internal reasoning with tool calls",
		ToolCalls: []ToolCall{{
			ID:    "call_1",
			Name:  "read_file",
			Input: map[string]any{"path": "README.md"},
		}},
	}, {
		Role:       "tool",
		ToolCallID: "call_1",
		Content:    "# cove",
	}, {
		Role:             "assistant",
		Content:          "final reply",
		ReasoningContent: "final internal reasoning without tool calls",
	}})

	if len(msgs) != 3 {
		t.Fatalf("expected 3 messages, got %d", len(msgs))
	}
	// DeepSeek guidelines:
	// - Message 0 (has tool calls): reasoning_content MUST be retained.
	if got := msgs[0].ReasoningContent; got != "internal reasoning with tool calls" {
		t.Fatalf("assistant with tool calls: reasoning_content = %q, want 'internal reasoning with tool calls'", got)
	}
	// Message 1 (tool output): has tool_call_id
	if got := msgs[1].ToolCallID; got != "call_1" {
		t.Fatalf("tool message tool_call_id = %q, want call_1", got)
	}
	// Message 2 (no tool calls): reasoning_content MUST be stripped to avoid bloat/errors.
	if got := msgs[2].ReasoningContent; got != "" {
		t.Fatalf("assistant without tool calls: reasoning_content = %q, want empty (stripped)", got)
	}
}

func TestOpenAICompatChatParsesDeepSeekCacheUsage(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{
			"model":"deepseek-v4-pro",
			"choices":[{"index":0,"message":{"role":"assistant","content":"ok"}}],
			"usage":{
				"prompt_tokens":100,
				"completion_tokens":20,
				"prompt_cache_hit_tokens":60,
				"prompt_cache_miss_tokens":40,
				"completion_tokens_details":{"reasoning_tokens":7}
			}
		}`)
	}))
	defer server.Close()

	p := &openAICompatProvider{apiKey: "test-key", baseURL: server.URL, client: server.Client()}
	resp, err := p.Chat(context.Background(), ChatRequest{Model: "deepseek-v4-pro", MaxTokens: 128, Messages: []Message{{Role: "user", Content: "hi"}}})
	if err != nil {
		t.Fatalf("Chat returned error: %v", err)
	}
	if resp.PromptCacheHitTokens != 60 || resp.PromptCacheMissTokens != 40 {
		t.Fatalf("unexpected cache usage: hit=%d miss=%d", resp.PromptCacheHitTokens, resp.PromptCacheMissTokens)
	}
	if resp.ReasoningTokens != 7 {
		t.Fatalf("ReasoningTokens = %d, want 7", resp.ReasoningTokens)
	}
}

func TestOpenAICompatChatStreamCapturesReasoningContentAndToolCalls(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		chunks := []string{
			"data: {\"choices\":[{\"index\":0,\"delta\":{\"reasoning_content\":\"need tool\"}}],\"usage\":null}\n\n",
			"data: {\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_1\",\"function\":{\"name\":\"read_file\",\"arguments\":\"{\"}}]}}],\"usage\":null}\n\n",
			"data: {\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"arguments\":\"\\\"path\\\":\\\"README.md\\\"}\"}}]}}],\"usage\":null}\n\n",
			"data: {\"choices\":[],\"usage\":{\"prompt_tokens\":11,\"completion_tokens\":7,\"prompt_cache_hit_tokens\":5,\"prompt_cache_miss_tokens\":6,\"completion_tokens_details\":{\"reasoning_tokens\":3}}}\n\n",
			"data: [DONE]\n\n",
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

	p := &openAICompatProvider{
		apiKey:  "test-key",
		baseURL: server.URL + "/v1",
		client:  server.Client(),
	}

	var deltas []string
	resp, err := p.ChatStream(context.Background(), ChatRequest{
		Model:     "deepseek-v4-pro",
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
	if got, want := resp.ReasoningContent, "need tool"; got != want {
		t.Fatalf("ReasoningContent = %q, want %q", got, want)
	}
	if len(resp.ToolCalls) != 1 {
		t.Fatalf("expected 1 tool call, got %d", len(resp.ToolCalls))
	}
	if got, want := resp.ToolCalls[0].Input["path"], any("README.md"); got != want {
		t.Fatalf("tool input path = %#v, want %#v", got, want)
	}
	if got, want := resp.InputTokens, 11; got != want {
		t.Fatalf("InputTokens = %d, want %d", got, want)
	}
	if got, want := resp.OutputTokens, 7; got != want {
		t.Fatalf("OutputTokens = %d, want %d", got, want)
	}
	if got, want := resp.PromptCacheHitTokens, 5; got != want {
		t.Fatalf("PromptCacheHitTokens = %d, want %d", got, want)
	}
	if got, want := resp.PromptCacheMissTokens, 6; got != want {
		t.Fatalf("PromptCacheMissTokens = %d, want %d", got, want)
	}
	if got, want := resp.ReasoningTokens, 3; got != want {
		t.Fatalf("ReasoningTokens = %d, want %d", got, want)
	}
	if got := strings.Join(deltas, ""); got != "" {
		t.Fatalf("expected no content deltas, got %q", got)
	}
}

func TestOpenAICompatChatStreamRequestsUsageInStreamOptions(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer r.Body.Close()
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		streamOptions, ok := body["stream_options"].(map[string]any)
		if !ok {
			t.Fatalf("stream_options missing: %#v", body)
		}
		if got, ok := streamOptions["include_usage"].(bool); !ok || !got {
			t.Fatalf("include_usage = %#v, want true", streamOptions["include_usage"])
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":1,\"completion_tokens\":1}}\n\n")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer server.Close()

	p := &openAICompatProvider{apiKey: "test-key", baseURL: server.URL + "/v1", client: server.Client()}
	resp, err := p.ChatStream(context.Background(), ChatRequest{Model: "deepseek-v4-pro", MaxTokens: 32, Messages: []Message{{Role: "user", Content: "hi"}}}, nil)
	if err != nil {
		t.Fatalf("ChatStream returned error: %v", err)
	}
	if resp.InputTokens != 1 || resp.OutputTokens != 1 {
		t.Fatalf("unexpected usage: in=%d out=%d", resp.InputTokens, resp.OutputTokens)
	}
}

func TestOpenAICompatConvertMessagesSupportsImageParts(t *testing.T) {
	p := &openAICompatProvider{}
	msgs := p.convertMessages([]Message{{
		Role: "user",
		Parts: []MessagePart{
			{Type: "text", Text: "请看这张图"},
			{Type: "image", MimeType: "image/png", Data: "aGVsbG8="},
		},
	}})

	if len(msgs) != 1 {
		t.Fatalf("expected 1 message, got %d", len(msgs))
	}
	arr, ok := msgs[0].Content.([]map[string]any)
	if !ok {
		t.Fatalf("expected content array, got %T", msgs[0].Content)
	}
	if len(arr) != 2 {
		t.Fatalf("expected 2 content blocks, got %d", len(arr))
	}
	if arr[0]["type"] != "text" {
		t.Fatalf("first block type = %#v, want text", arr[0]["type"])
	}
	if arr[1]["type"] != "image_url" {
		t.Fatalf("second block type = %#v, want image_url", arr[1]["type"])
	}
}

func TestOpenAICompatToolImagesFollowToolResults(t *testing.T) {
	provider := &openAICompatProvider{}
	messages := provider.convertMessages([]Message{
		{Role: "user", Content: "inspect screenshots"},
		{Role: "assistant", ToolCalls: []ToolCall{{ID: "read-1", Name: "read", Input: map[string]any{}}, {ID: "read-2", Name: "read", Input: map[string]any{}}}},
		{Role: "tool", ToolCallID: "read-1", Content: "Image: first.png"},
		{Role: "tool", ToolCallID: "read-2", Content: "Image: second.png"},
		{Role: "user", Synthetic: true, Content: "Tool image results", Parts: []MessagePart{{Type: "image", MimeType: "image/png", Data: "Zmlyc3Q="}, {Type: "image", MimeType: "image/png", Data: "c2Vjb25k"}}},
	})
	raw, err := json.Marshal(messages)
	if err != nil {
		t.Fatal(err)
	}
	var wire []oaiMsg
	if err := json.Unmarshal(raw, &wire); err != nil {
		t.Fatal(err)
	}
	if len(wire) != 5 || wire[2].Role != "tool" || wire[2].ToolCallID != "read-1" || wire[3].Role != "tool" || wire[3].ToolCallID != "read-2" || wire[4].Role != "user" {
		t.Fatalf("tool-image order changed: %s", raw)
	}
	blocks, _ := wire[4].Content.([]any)
	if len(blocks) != 3 {
		t.Fatalf("image blocks=%d, want text and two images", len(blocks))
	}
	for index, expected := range []string{"data:image/png;base64,Zmlyc3Q=", "data:image/png;base64,c2Vjb25k"} {
		block, _ := blocks[index+1].(map[string]any)
		imageURL, _ := block["image_url"].(map[string]any)
		if block["type"] != "image_url" || imageURL["url"] != expected {
			t.Fatalf("image %d changed: %+v", index, block)
		}
	}
}

func TestDeepSeekFlashSendsNativeImage(t *testing.T) {
	for _, stream := range []bool{false, true} {
		name := "chat"
		if stream {
			name = "stream"
		}
		t.Run(name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var body oaiReq
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Errorf("decode request: %v", err)
				}
				if body.Model != "deepseek-flash" || len(body.Messages) != 1 {
					t.Errorf("unexpected request: %+v", body)
					return
				}
				blocks, _ := body.Messages[0].Content.([]any)
				if body.Messages[0].Role != "user" || len(blocks) != 2 {
					t.Errorf("expected user text and image: %+v", body.Messages[0])
					return
				}
				block, _ := blocks[1].(map[string]any)
				imageURL, _ := block["image_url"].(map[string]any)
				if block["type"] != "image_url" || imageURL["url"] != "data:image/png;base64,cGl4ZWxz" {
					t.Errorf("image payload changed: %+v", block)
				}
				if stream {
					w.Header().Set("Content-Type", "text/event-stream")
					_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
				} else {
					w.Header().Set("Content-Type", "application/json")
					_, _ = io.WriteString(w, `{"model":"deepseek-flash","choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`)
				}
			}))
			defer server.Close()
			provider := &openAICompatProvider{apiKey: "test-key", baseURL: server.URL, client: server.Client()}
			request := ChatRequest{Model: "deepseek-flash", MaxTokens: 32, Messages: []Message{{Role: "user", Content: "see image", Parts: []MessagePart{{Type: "image", MimeType: "image/png", Data: "cGl4ZWxz"}}}}}
			var response *ChatResponse
			var err error
			if stream {
				response, err = provider.ChatStream(context.Background(), request, func(StreamEvent) {})
			} else {
				response, err = provider.Chat(context.Background(), request)
			}
			if err != nil || response == nil || response.Content != "ok" {
				t.Fatalf("response=%+v err=%v", response, err)
			}
		})
	}
}

func TestOpenAICompatChatDowngradesImageForNonVisionModel(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer r.Body.Close()
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		messages, ok := body["messages"].([]any)
		if !ok || len(messages) == 0 {
			t.Fatalf("messages missing in request body: %#v", body)
		}
		m0, _ := messages[0].(map[string]any)
		content, _ := m0["content"].([]any)
		if len(content) == 0 {
			t.Fatalf("content missing in first message: %#v", m0)
		}
		for _, blockAny := range content {
			block, _ := blockAny.(map[string]any)
			if block["type"] == "image_url" {
				t.Fatalf("non-vision model request should not contain image_url block: %#v", block)
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"model":"deepseek-reasoner","choices":[{"index":0,"message":{"role":"assistant","content":"ok"}}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`)
	}))
	defer server.Close()

	p := &openAICompatProvider{apiKey: "test-key", baseURL: server.URL, client: server.Client()}
	_, err := p.Chat(context.Background(), ChatRequest{
		Model: "deepseek-reasoner",
		Messages: []Message{{
			Role:  "user",
			Parts: []MessagePart{{Type: "image", MimeType: "image/png", Data: "aGVsbG8=", FileName: "x.png"}},
		}},
		MaxTokens: 32,
	})
	if err != nil {
		t.Fatalf("Chat returned error: %v", err)
	}
}

func TestOpenAICompatChatReportsImageUnsupportedError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body oaiReq
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode request: %v", err)
		}
		foundImage := false
		for _, message := range body.Messages {
			blocks, _ := message.Content.([]any)
			for _, rawBlock := range blocks {
				block, _ := rawBlock.(map[string]any)
				foundImage = foundImage || block["type"] == "image_url"
			}
		}
		if body.Model != "deepseek-flash" || !foundImage {
			t.Errorf("expected Flash image request, got %+v", body)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"error":{"message":"Failed to deserialize: unknown variant `+"`image_url`"+`, expected `+"`text`"+`"}}`)
	}))
	defer server.Close()

	p := &openAICompatProvider{apiKey: "test-key", baseURL: server.URL, client: server.Client()}
	_, err := p.Chat(context.Background(), ChatRequest{
		Model:     "deepseek-flash",
		MaxTokens: 32,
		Messages: []Message{{
			Role:  "user",
			Parts: []MessagePart{{Type: "image", MimeType: "image/png", Data: "aGVsbG8=", FileName: "x.png"}},
		}},
	})
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "不支持图片输入") {
		t.Fatalf("error = %q, want friendly unsupported-image hint", err.Error())
	}
}

func TestOpenAICompatReasonerStripsTools(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var reqBody map[string]any
		bodyBytes, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(bodyBytes, &reqBody)

		if _, exists := reqBody["tools"]; exists {
			t.Errorf("reasoner model should not receive 'tools' in body")
		}
		if _, exists := reqBody["tool_choice"]; exists {
			t.Errorf("reasoner model should not receive 'tool_choice' in body")
		}

		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"model":"deepseek-reasoner","choices":[{"index":0,"message":{"role":"assistant","content":"understood"}}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`)
	}))
	defer server.Close()

	p := &openAICompatProvider{apiKey: "test-key", baseURL: server.URL, client: server.Client()}
	resp, err := p.Chat(context.Background(), ChatRequest{
		Model: "deepseek-reasoner",
		Messages: []Message{{
			Role:    "user",
			Content: "hello",
		}},
		Tools: []ToolDef{{
			Name:        "test_tool",
			Description: "does nothing",
		}},
		MaxTokens: 32,
	})
	if err != nil {
		t.Fatalf("Chat returned error: %v", err)
	}
	if resp.Content != "understood" {
		t.Fatalf("unexpected content: %q", resp.Content)
	}
}
