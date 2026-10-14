package api

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestBedrockRegionFallsBackToProfileRegion(t *testing.T) {
	t.Setenv("AWS_REGION", "")
	t.Setenv("AWS_DEFAULT_REGION", "")
	t.Setenv("AWS_PROFILE", "")
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", filepath.Join(t.TempDir(), "none"))
	cfgPath := filepath.Join(t.TempDir(), "config")
	t.Setenv("AWS_CONFIG_FILE", cfgPath)
	if got := BedrockRegion(); got != "us-east-1" {
		t.Fatalf("no profile: %q", got)
	}
	if err := os.WriteFile(cfgPath, []byte("[default]\nregion = ap-northeast-1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := BedrockRegion(); got != "ap-northeast-1" {
		t.Fatalf("profile region ignored: %q", got)
	}
	t.Setenv("AWS_DEFAULT_REGION", "eu-west-1")
	if got := BedrockRegion(); got != "eu-west-1" {
		t.Fatalf("environment must win over the profile: %q", got)
	}
}

func TestBedrockRegionFromHost(t *testing.T) {
	cases := map[string]string{
		"bedrock-mantle.us-west-2.api.aws":      "us-west-2",
		"Bedrock-Mantle.EU-Central-1.api.aws":   "eu-central-1",
		"bedrock-mantle.us-west-2.api.aws:8443": "",
		"127.0.0.1:34567":                       "",
		"proxy.example.com":                     "",
		"":                                      "",
	}
	for host, want := range cases {
		got, ok := bedrockRegionFromHost(host)
		if got != want || ok != (want != "") {
			t.Errorf("bedrockRegionFromHost(%q) = %q,%v want %q", host, got, ok, want)
		}
	}
}

// Claude in Amazon Bedrock serves the Messages API at
// https://bedrock-mantle.{region}.api.aws/anthropic/v1/messages with the
// first-party request shape and SSE, so cove reaches it through the
// anthropic provider with a different URL, auth and name. These tests pin
// the differences; everything else is covered by the anthropic tests.

func TestBedrockAliasesNormalizeToBedrock(t *testing.T) {
	for _, in := range []string{"bedrock", "Bedrock", "aws", "amazon-bedrock", "aws-bedrock"} {
		if got := NormalizeProviderName(in); got != "bedrock" {
			t.Errorf("NormalizeProviderName(%q) = %q, want bedrock", in, got)
		}
	}
}

func TestBedrockIsKnownAndNotOpenAICompatible(t *testing.T) {
	if !IsKnownProvider("bedrock") {
		t.Fatal("bedrock should be a known provider")
	}
	if IsOpenAICompatibleProvider("bedrock") {
		t.Fatal("bedrock speaks the Anthropic Messages API, not OpenAI chat completions")
	}
	if !IsOpenAICompatibleProvider("deepseek") || IsOpenAICompatibleProvider("anthropic") {
		t.Fatal("existing provider families changed")
	}
}

func TestBedrockEnvCandidatesStartWithBearerToken(t *testing.T) {
	envs := ProviderEnvCandidates("bedrock")
	if len(envs) == 0 || envs[0] != "AWS_BEARER_TOKEN_BEDROCK" {
		t.Fatalf("ProviderEnvCandidates(bedrock) = %v, want AWS_BEARER_TOKEN_BEDROCK first", envs)
	}
}

func TestBedrockBaseURLFollowsRegion(t *testing.T) {
	if got, want := BedrockBaseURL("eu-west-1"), "https://bedrock-mantle.eu-west-1.api.aws/anthropic"; got != want {
		t.Fatalf("BedrockBaseURL = %q, want %q", got, want)
	}
	t.Setenv("AWS_REGION", "")
	t.Setenv("AWS_DEFAULT_REGION", "")
	t.Setenv("AWS_PROFILE", "")
	t.Setenv("AWS_CONFIG_FILE", filepath.Join(t.TempDir(), "config")) // a real ~/.aws/config region would win
	if got, want := DefaultBaseURL("bedrock"), "https://bedrock-mantle.us-east-1.api.aws/anthropic"; got != want {
		t.Fatalf("DefaultBaseURL(bedrock) without region = %q, want %q", got, want)
	}
	t.Setenv("AWS_DEFAULT_REGION", "ap-southeast-2")
	if got, want := DefaultBaseURL("bedrock"), "https://bedrock-mantle.ap-southeast-2.api.aws/anthropic"; got != want {
		t.Fatalf("DefaultBaseURL(bedrock) with AWS_DEFAULT_REGION = %q, want %q", got, want)
	}
	t.Setenv("AWS_REGION", " ap-northeast-1 ")
	if got, want := DefaultBaseURL("bedrock"), "https://bedrock-mantle.ap-northeast-1.api.aws/anthropic"; got != want {
		t.Fatalf("DefaultBaseURL(bedrock) with AWS_REGION = %q, want %q (AWS_REGION wins, trimmed)", got, want)
	}
}

func TestNewProviderBedrockIsAnthropicFamilyNamedBedrock(t *testing.T) {
	t.Setenv("AWS_REGION", "us-west-2")
	p := NewProvider(ProviderConfig{Name: "aws", APIKey: "k"})
	ap, ok := p.(*anthropicProvider)
	if !ok {
		t.Fatalf("bedrock should reuse anthropicProvider, got %T", p)
	}
	if p.Name() != "bedrock" || p.DisplayName() != "bedrock" {
		t.Fatalf("Name()=%q DisplayName()=%q, want bedrock", p.Name(), p.DisplayName())
	}
	if got, want := ap.baseURL, "https://bedrock-mantle.us-west-2.api.aws/anthropic/v1"; got != want {
		t.Fatalf("baseURL = %q, want %q", got, want)
	}
	c := CapabilitiesOf(p)
	if !c.CacheBreakpoints || !c.ToolsWithToolHistory || c.Family != "anthropic" {
		t.Fatalf("bedrock capabilities = %+v, want the anthropic set", c)
	}
	// The plain anthropic provider keeps its name.
	if got := NewProvider(ProviderConfig{Name: "anthropic", APIKey: "k"}).Name(); got != "anthropic" {
		t.Fatalf("anthropic Name() = %q", got)
	}
}

func TestBedrockValidateNamesTheBearerTokenEnv(t *testing.T) {
	noCredsEnv(t)
	err := NewProvider(ProviderConfig{Name: "bedrock"}).Validate()
	if err == nil || !strings.Contains(err.Error(), "AWS_BEARER_TOKEN_BEDROCK") {
		t.Fatalf("Validate() = %v, want an error naming AWS_BEARER_TOKEN_BEDROCK", err)
	}
	if err := NewProvider(ProviderConfig{Name: "anthropic"}).Validate(); err == nil || !strings.Contains(err.Error(), "ANTHROPIC_API_KEY") {
		t.Fatalf("anthropic Validate() = %v, want ANTHROPIC_API_KEY hint", err)
	}
}

// captureServer records the most recent request (path and headers) and
// answers with a minimal non-streaming Messages response.
func captureServer(t *testing.T) (*httptest.Server, *http.Request) {
	t.Helper()
	got := &http.Request{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*got = *r.Clone(context.Background())
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`)
	}))
	t.Cleanup(server.Close)
	return server, got
}

func TestBedrockChatSendsBearerTokenAsAPIKeyWithoutBetaHeader(t *testing.T) {
	server, got := captureServer(t)
	p := NewProvider(ProviderConfig{Name: "bedrock", APIKey: "bedrock-token", BaseURL: server.URL + "/anthropic"}).(*anthropicProvider)
	p.client = server.Client()
	if _, err := p.Chat(context.Background(), ChatRequest{Model: "anthropic.claude-sonnet-5", MaxTokens: 8, Messages: []Message{{Role: "user", Content: "hi"}}}); err != nil {
		t.Fatalf("Chat error: %v", err)
	}
	if got.URL.Path != "/anthropic/v1/messages" {
		t.Fatalf("path = %q, want /anthropic/v1/messages", got.URL.Path)
	}
	if got.Header.Get("x-api-key") != "bedrock-token" {
		t.Fatalf("x-api-key = %q, want the bearer token", got.Header.Get("x-api-key"))
	}
	if got.Header.Get("anthropic-version") != "2023-06-01" {
		t.Fatalf("anthropic-version = %q", got.Header.Get("anthropic-version"))
	}
	if v := got.Header.Get("anthropic-beta"); v != "" {
		t.Fatalf("anthropic-beta = %q, want none on Bedrock", v)
	}
	if v := got.Header.Get("Authorization"); v != "" {
		t.Fatalf("Authorization = %q, want none (Bedrock takes the token in x-api-key)", v)
	}
}

func TestAnthropicChatStillSendsBetaHeader(t *testing.T) {
	server, got := captureServer(t)
	p := NewProvider(ProviderConfig{Name: "anthropic", APIKey: "k", BaseURL: server.URL}).(*anthropicProvider)
	p.client = server.Client()
	if _, err := p.Chat(context.Background(), ChatRequest{Model: "claude-opus-5", MaxTokens: 8, Messages: []Message{{Role: "user", Content: "hi"}}}); err != nil {
		t.Fatalf("Chat error: %v", err)
	}
	if !strings.Contains(got.Header.Get("anthropic-beta"), "prompt-caching") {
		t.Fatalf("anthropic-beta = %q, want the first-party beta list", got.Header.Get("anthropic-beta"))
	}
}

func TestBedrockChatStreamUsesStandardSSEWithoutBetaHeader(t *testing.T) {
	var gotBeta, gotKey, gotPath string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotBeta, gotKey = r.URL.Path, r.Header.Get("anthropic-beta"), r.Header.Get("x-api-key")
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		for _, chunk := range []string{
			"event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_1\",\"usage\":{\"input_tokens\":5,\"output_tokens\":0}}}\n\n",
			"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n",
			"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"hello\"}}\n\n",
			"event: message_delta\ndata: {\"type\":\"message_delta\",\"usage\":{\"output_tokens\":1}}\n\n",
			"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n",
		} {
			_, _ = io.WriteString(w, chunk)
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
		}
	}))
	defer server.Close()

	p := NewProvider(ProviderConfig{Name: "bedrock", APIKey: "bedrock-token", BaseURL: server.URL + "/anthropic"}).(*anthropicProvider)
	p.streamClient = server.Client()
	resp, err := p.ChatStream(context.Background(), ChatRequest{Model: "anthropic.claude-sonnet-5", MaxTokens: 8, Messages: []Message{{Role: "user", Content: "hi"}}}, nil)
	if err != nil {
		t.Fatalf("ChatStream error: %v", err)
	}
	if resp.Content != "hello" || resp.InputTokens != 5 || resp.OutputTokens != 1 {
		t.Fatalf("resp = %+v", resp)
	}
	if gotPath != "/anthropic/v1/messages" || gotKey != "bedrock-token" || gotBeta != "" {
		t.Fatalf("path=%q x-api-key=%q anthropic-beta=%q", gotPath, gotKey, gotBeta)
	}
}

// Bedrock model IDs carry an "anthropic." prefix; the substring matching
// used for context windows and output caps must still recognize them.
func TestBedrockModelIDsResolveContextAndOutputCaps(t *testing.T) {
	if got := ContextWindowForModel("anthropic.claude-sonnet-5"); got != 1000000 {
		t.Fatalf("ContextWindowForModel(anthropic.claude-sonnet-5) = %d, want 1000000", got)
	}
	if got := ContextWindowForModel("anthropic.claude-haiku-4-5"); got != 200000 {
		t.Fatalf("ContextWindowForModel(anthropic.claude-haiku-4-5) = %d, want 200000", got)
	}
	if got := MaxOutputTokensForModel("anthropic.claude-opus-4-1-20250805-v1:0"); got != 32000 {
		t.Fatalf("MaxOutputTokensForModel(bedrock opus 4.1) = %d, want 32000", got)
	}
	if !IsVisionCapableModel("anthropic.claude-sonnet-5") {
		t.Fatal("prefixed Claude IDs are vision capable")
	}
}
