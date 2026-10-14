package api

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

// iamEnv gives the test static IAM credentials in the environment and no
// bearer token, with ~/.aws pointed at an empty temp dir.
func iamEnv(t *testing.T) {
	t.Helper()
	t.Setenv("AWS_BEARER_TOKEN_BEDROCK", "")
	t.Setenv("LLM_API_KEY", "")
	t.Setenv("BEDROCK_API_KEY", "")
	t.Setenv("AWS_ACCESS_KEY_ID", "AKIDEXAMPLE")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "secret")
	t.Setenv("AWS_SESSION_TOKEN", "")
	t.Setenv("AWS_PROFILE", "")
	t.Setenv("AWS_REGION", "eu-west-1")
	dir := t.TempDir()
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", filepath.Join(dir, "credentials"))
	t.Setenv("AWS_CONFIG_FILE", filepath.Join(dir, "config"))
}

func noCredsEnv(t *testing.T) {
	t.Helper()
	iamEnv(t)
	t.Setenv("AWS_ACCESS_KEY_ID", "")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "")
}

func TestHasCredentials(t *testing.T) {
	noCredsEnv(t)
	if HasCredentials(ProviderConfig{Name: "bedrock"}) {
		t.Fatal("no key and no AWS credentials must be false")
	}
	if !HasCredentials(ProviderConfig{Name: "bedrock", APIKey: "tok"}) || !HasCredentials(ProviderConfig{Name: "deepseek", APIKey: "k"}) {
		t.Fatal("an api key is credentials for any provider")
	}
	if HasCredentials(ProviderConfig{Name: "anthropic"}) {
		t.Fatal("anthropic without a key has no credentials")
	}
	iamEnv(t)
	if !HasCredentials(ProviderConfig{Name: "aws"}) {
		t.Fatal("bedrock with IAM credentials must be true")
	}
	if HasCredentials(ProviderConfig{Name: "anthropic"}) {
		t.Fatal("AWS credentials do not apply to the anthropic provider")
	}
}

func TestBedrockValidateAcceptsIAMCredentialsAndListsBothOtherwise(t *testing.T) {
	iamEnv(t)
	if err := NewProvider(ProviderConfig{Name: "bedrock"}).Validate(); err != nil {
		t.Fatalf("Validate with IAM credentials: %v", err)
	}
	noCredsEnv(t)
	err := NewProvider(ProviderConfig{Name: "bedrock"}).Validate()
	if err == nil || !strings.Contains(err.Error(), "AWS_BEARER_TOKEN_BEDROCK") || !strings.Contains(err.Error(), "AWS_ACCESS_KEY_ID") {
		t.Fatalf("Validate without credentials = %v, want both options named", err)
	}
}

func TestBedrockChatSignsWithSigV4WhenNoKey(t *testing.T) {
	iamEnv(t)
	server, got := captureServer(t)
	p := NewProvider(ProviderConfig{Name: "bedrock", BaseURL: server.URL + "/anthropic"}).(*anthropicProvider)
	p.client = server.Client()
	if _, err := p.Chat(context.Background(), ChatRequest{Model: "anthropic.claude-sonnet-5", MaxTokens: 8, Messages: []Message{{Role: "user", Content: "hi"}}}); err != nil {
		t.Fatalf("Chat error: %v", err)
	}
	authz := got.Header.Get("Authorization")
	// The test server host is 127.0.0.1:port, so the region comes from AWS_REGION.
	if !strings.HasPrefix(authz, "AWS4-HMAC-SHA256 Credential=AKIDEXAMPLE/") || !strings.Contains(authz, "/eu-west-1/bedrock-mantle/aws4_request, SignedHeaders=content-type;host;x-amz-content-sha256;x-amz-date, Signature=") {
		t.Fatalf("Authorization = %q", authz)
	}
	if got.Header.Get("x-api-key") != "" {
		t.Fatalf("x-api-key = %q, want none under SigV4", got.Header.Get("x-api-key"))
	}
	if got.Header.Get("x-amz-date") == "" || got.Header.Get("x-amz-content-sha256") == "" {
		t.Fatal("x-amz-date / x-amz-content-sha256 missing")
	}
	if got.Header.Get("anthropic-version") != "2023-06-01" || got.Header.Get("anthropic-beta") != "" {
		t.Fatalf("anthropic headers: version=%q beta=%q", got.Header.Get("anthropic-version"), got.Header.Get("anthropic-beta"))
	}
}

func TestBedrockBearerWinsOverIAMCredentials(t *testing.T) {
	iamEnv(t)
	server, got := captureServer(t)
	p := NewProvider(ProviderConfig{Name: "bedrock", APIKey: "bedrock-token", BaseURL: server.URL + "/anthropic"}).(*anthropicProvider)
	p.client = server.Client()
	if _, err := p.Chat(context.Background(), ChatRequest{Model: "anthropic.claude-sonnet-5", MaxTokens: 8, Messages: []Message{{Role: "user", Content: "hi"}}}); err != nil {
		t.Fatalf("Chat error: %v", err)
	}
	if got.Header.Get("x-api-key") != "bedrock-token" || got.Header.Get("Authorization") != "" {
		t.Fatalf("x-api-key=%q Authorization=%q, want bearer only", got.Header.Get("x-api-key"), got.Header.Get("Authorization"))
	}
}

func TestBedrockStreamIsSignedAndRetriesResign(t *testing.T) {
	iamEnv(t)
	var authz []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authz = append(authz, r.Header.Get("Authorization"))
		if len(authz) == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"m\",\"usage\":{\"input_tokens\":1,\"output_tokens\":0}}}\n\n")
		_, _ = io.WriteString(w, "event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n")
		_, _ = io.WriteString(w, "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"ok\"}}\n\n")
		_, _ = io.WriteString(w, "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
	}))
	defer server.Close()
	p := NewProvider(ProviderConfig{Name: "bedrock", BaseURL: server.URL + "/anthropic"}).(*anthropicProvider)
	p.streamClient = server.Client()
	resp, err := p.ChatStream(context.Background(), ChatRequest{Model: "anthropic.claude-sonnet-5", MaxTokens: 8, Messages: []Message{{Role: "user", Content: "hi"}}}, nil)
	if err != nil || resp.Content != "ok" {
		t.Fatalf("ChatStream = %+v, %v", resp, err)
	}
	if len(authz) != 2 || !strings.HasPrefix(authz[0], "AWS4-HMAC-SHA256 ") || !strings.HasPrefix(authz[1], "AWS4-HMAC-SHA256 ") {
		t.Fatalf("every attempt must carry a signature: %q", authz)
	}
}

func TestBedrockWithoutAnyCredentialsFailsBeforeSending(t *testing.T) {
	noCredsEnv(t)
	server, got := captureServer(t)
	p := NewProvider(ProviderConfig{Name: "bedrock", BaseURL: server.URL + "/anthropic"}).(*anthropicProvider)
	p.client = server.Client()
	_, err := p.Chat(context.Background(), ChatRequest{Model: "anthropic.claude-sonnet-5", MaxTokens: 8, Messages: []Message{{Role: "user", Content: "hi"}}})
	if err == nil || !strings.Contains(err.Error(), "AWS_ACCESS_KEY_ID") {
		t.Fatalf("err = %v, want the credentials message", err)
	}
	if got.URL != nil {
		t.Fatal("no request may reach the server without credentials")
	}
}

func TestBedrockForbiddenCarriesTroubleshootingHint(t *testing.T) {
	iamEnv(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, `{"message":"The security token included in the request is invalid."}`)
	}))
	defer server.Close()
	p := NewProvider(ProviderConfig{Name: "bedrock", BaseURL: server.URL + "/anthropic"}).(*anthropicProvider)
	p.client = server.Client()
	_, err := p.Chat(context.Background(), ChatRequest{Model: "anthropic.claude-sonnet-5", MaxTokens: 8, Messages: []Message{{Role: "user", Content: "hi"}}})
	if err == nil || !strings.Contains(err.Error(), "security token") || !strings.Contains(err.Error(), "AWS_REGION") {
		t.Fatalf("err = %v, want server message plus the Bedrock hint", err)
	}
	// The plain anthropic provider does not get the Bedrock hint.
	a := NewProvider(ProviderConfig{Name: "anthropic", APIKey: "k", BaseURL: server.URL}).(*anthropicProvider)
	a.client = server.Client()
	_, err = a.Chat(context.Background(), ChatRequest{Model: "claude-opus-5", MaxTokens: 8, Messages: []Message{{Role: "user", Content: "hi"}}})
	if err == nil || strings.Contains(err.Error(), "AWS_REGION") {
		t.Fatalf("anthropic 403 = %v, must not carry the Bedrock hint", err)
	}
}
