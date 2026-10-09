package api

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestDeepSeekImageFilesReuse(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		name := "disabled"
		if enabled {
			name = "enabled"
		}
		t.Run(name, func(t *testing.T) {
			var uploads atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
				if request.URL.Path == "/files" {
					uploads.Add(1)
					if request.Header.Get("Authorization") != "Bearer test-key" {
						t.Error("wrong upload authorization")
					}
					if err := request.ParseMultipartForm(1 << 20); err != nil {
						t.Errorf("multipart: %v", err)
						return
					}
					defer request.MultipartForm.RemoveAll()
					if request.FormValue("purpose") != "user_data" || request.FormValue("expires_after[anchor]") != "created_at" || request.FormValue("expires_after[seconds]") != "3600" {
						t.Error("upload omitted purpose or one-hour expiration")
					}
					file, _, err := request.FormFile("file")
					if err != nil {
						t.Errorf("upload file: %v", err)
						return
					}
					defer file.Close()
					data, err := io.ReadAll(file)
					if err != nil || string(data) != "pixels" {
						t.Errorf("uploaded bytes changed: %q, %v", data, err)
					}
					w.Header().Set("Content-Type", "application/json")
					_, _ = io.WriteString(w, `{"id":"file-api-test"}`)
					return
				}
				if request.URL.Path != "/v1/chat/completions" {
					t.Errorf("unexpected path: %q", request.URL.Path)
				}
				var body oaiReq
				if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
					t.Errorf("decode chat: %v", err)
					return
				}
				if len(body.Messages) != 1 {
					t.Errorf("messages=%d, want 1", len(body.Messages))
					return
				}
				blocks, _ := body.Messages[0].Content.([]any)
				if len(blocks) != 1 {
					t.Errorf("blocks=%d, want 1", len(blocks))
					return
				}
				block, _ := blocks[0].(map[string]any)
				if enabled {
					if block["type"] != "file" || block["file_id"] != "file-api-test" || len(block) != 2 {
						t.Errorf("expected file reference, got %+v", block)
					}
				} else {
					imageURL, _ := block["image_url"].(map[string]any)
					if block["type"] != "image_url" || imageURL["url"] != "data:image/png;base64,cGl4ZWxz" {
						t.Errorf("inline image changed: %+v", block)
					}
				}
				if body.Stream {
					w.Header().Set("Content-Type", "text/event-stream")
					_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
				} else {
					w.Header().Set("Content-Type", "application/json")
					_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"ok"},"finish_reason":"stop"}]}`)
				}
			}))
			defer server.Close()
			configJSON, err := json.Marshal(map[string]any{"Name": "deepseek", "APIKey": "test-key", "BaseURL": server.URL + "/v1", "ImageFilesAPI": enabled})
			if err != nil {
				t.Fatal(err)
			}
			var cfg ProviderConfig
			if err := json.Unmarshal(configJSON, &cfg); err != nil {
				t.Fatal(err)
			}
			provider := newOpenAICompatProvider(cfg)
			provider.client = server.Client()
			provider.streamClient = server.Client()
			request := ChatRequest{Model: "deepseek-flash", MaxTokens: 32, Messages: []Message{{Role: "user", Parts: []MessagePart{{Type: "image", MimeType: "image/png", Data: "cGl4ZWxz"}}}}}
			if _, err := provider.Chat(context.Background(), request); err != nil {
				t.Fatal(err)
			}
			if _, err := provider.ChatStream(context.Background(), request, func(StreamEvent) {}); err != nil {
				t.Fatal(err)
			}
			wantUploads := int32(0)
			if enabled {
				wantUploads = 1
			}
			if uploads.Load() != wantUploads {
				t.Fatalf("uploads=%d, want %d", uploads.Load(), wantUploads)
			}
			if request.Messages[0].Parts[0].Data != "cGl4ZWxz" {
				t.Fatal("session image data mutated")
			}
		})
	}
}

func imageFilesTestProvider(server *httptest.Server, keys ...string) *openAICompatProvider {
	provider := newOpenAICompatProvider(ProviderConfig{Name: "deepseek", APIKeys: keys, BaseURL: server.URL + "/v1", ImageFilesAPI: true})
	provider.client = server.Client()
	provider.streamClient = server.Client()
	return provider
}

func imageFilesTestRequest() ChatRequest {
	return ChatRequest{Model: "deepseek-flash", MaxTokens: 32, Messages: []Message{{Role: "user", Parts: []MessagePart{{Type: "image", MimeType: "image/png", Data: "cGl4ZWxz"}}}}}
}

func TestDeepSeekImageFilesUploadFailureKeepsInline(t *testing.T) {
	for _, scenario := range []string{"status", "invalid-id", "expired", "redirect"} {
		t.Run(scenario, func(t *testing.T) {
			var redirected atomic.Int32
			other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { redirected.Add(1) }))
			defer other.Close()
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/files" {
					switch scenario {
					case "status":
						w.WriteHeader(http.StatusServiceUnavailable)
					case "invalid-id":
						_, _ = io.WriteString(w, `{"id":"not-a-file"}`)
					case "expired":
						_, _ = io.WriteString(w, `{"id":"file-api-expired","expires_at":1}`)
					case "redirect":
						http.Redirect(w, r, other.URL, http.StatusTemporaryRedirect)
					}
					return
				}
				var body oaiReq
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Errorf("chat decode: %v", err)
					return
				}
				blocks, _ := body.Messages[0].Content.([]any)
				block, _ := blocks[0].(map[string]any)
				imageURL, _ := block["image_url"].(map[string]any)
				if block["type"] != "image_url" || imageURL["url"] != "data:image/png;base64,cGl4ZWxz" {
					t.Errorf("failed upload lost inline image: %+v", block)
				}
				_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"ok"},"finish_reason":"stop"}]}`)
			}))
			defer server.Close()
			if _, err := imageFilesTestProvider(server, "key").Chat(context.Background(), imageFilesTestRequest()); err != nil {
				t.Fatal(err)
			}
			if redirected.Load() != 0 {
				t.Fatal("upload followed a redirect")
			}
		})
	}
}

func TestDeepSeekImageFilesFollowRotatedKey(t *testing.T) {
	var uploads atomic.Int32
	var chats atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if r.URL.Path == "/files" {
			uploads.Add(1)
			_, _ = fmt.Fprintf(w, `{"id":"file-api-%s"}`, key)
			return
		}
		chats.Add(1)
		var body oaiReq
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("chat decode: %v", err)
			return
		}
		blocks, _ := body.Messages[0].Content.([]any)
		block, _ := blocks[0].(map[string]any)
		if block["file_id"] != "file-api-"+key {
			t.Errorf("file ID crossed accounts: key=%q block=%+v", key, block)
		}
		if key == "revoked" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"ok"},"finish_reason":"stop"}]}`)
	}))
	defer server.Close()
	provider := imageFilesTestProvider(server, "revoked", "working")
	if _, err := provider.Chat(context.Background(), imageFilesTestRequest()); err != nil {
		t.Fatal(err)
	}
	if uploads.Load() != 2 || chats.Load() != 2 {
		t.Fatalf("uploads=%d chats=%d, want one per key", uploads.Load(), chats.Load())
	}
}

func TestDeepSeekImageFilesConcurrentReuseAndExpiration(t *testing.T) {
	var uploads atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/files" {
			_, _ = fmt.Fprintf(w, `{"id":"file-api-%d"}`, uploads.Add(1))
			return
		}
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"ok"},"finish_reason":"stop"}]}`)
	}))
	defer server.Close()
	provider := imageFilesTestProvider(server, "key")
	var group sync.WaitGroup
	for index := 0; index < 8; index++ {
		group.Add(1)
		go func() {
			defer group.Done()
			if _, err := provider.Chat(context.Background(), imageFilesTestRequest()); err != nil {
				t.Errorf("concurrent chat: %v", err)
			}
		}()
	}
	group.Wait()
	if uploads.Load() != 1 {
		t.Fatalf("concurrent uploads=%d, want 1", uploads.Load())
	}
	for key, entry := range provider.imageFiles.entries {
		entry.Expires = time.Now().Add(-time.Second)
		provider.imageFiles.entries[key] = entry
	}
	if _, err := provider.Chat(context.Background(), imageFilesTestRequest()); err != nil {
		t.Fatal(err)
	}
	if uploads.Load() != 2 {
		t.Fatalf("uploads after expiration=%d, want 2", uploads.Load())
	}
}

func TestDeepSeekImageFilesWaitIsCancelable(t *testing.T) {
	provider := newOpenAICompatProvider(ProviderConfig{Name: "deepseek", ImageFilesAPI: true})
	provider.imageFiles.gate <- struct{}{}
	defer func() { <-provider.imageFiles.gate }()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := provider.imageFile(ctx, "key", []byte("pixels"), "image/png", imageFileKey{}); err != context.Canceled {
		t.Fatalf("upload wait error=%v, want canceled", err)
	}
}

func TestDeepSeekImageFilesInvalidRequestDoesNotUpload(t *testing.T) {
	fastRetry(t)
	var uploads atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		uploads.Add(1)
		_, _ = io.WriteString(w, `{"id":"file-api-test"}`)
	}))
	defer server.Close()
	request := imageFilesTestRequest()
	request.Messages = append(request.Messages, Message{Role: "assistant", ToolCalls: []ToolCall{{ID: "call", Name: "read", Input: map[string]any{}, Extra: json.RawMessage("not-json")}}})
	provider := imageFilesTestProvider(server, "key")
	if _, err := provider.ChatStream(context.Background(), request, func(StreamEvent) {}); err == nil {
		t.Fatal("invalid metadata was accepted")
	}
	if uploads.Load() != 0 {
		t.Fatal("invalid chat request uploaded an image before validation")
	}
}
