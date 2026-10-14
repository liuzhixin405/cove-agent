package api

import (
	"net/http"
	"slices"
	"testing"
)

// A key pool whose keys are all cooling down hands out "". That must not
// switch a bearer-configured bedrock provider to SigV4: with IAM
// credentials present the request would go out as a different AWS
// identity, and without them a retryable 429 would become a hard error.
func TestBedrockKeyPoolEmptyKeyNeverFallsBackToSigV4(t *testing.T) {
	iamEnv(t)
	p := NewProvider(ProviderConfig{Name: "bedrock", APIKeys: []string{"a", "b"}, BaseURL: "https://bedrock-mantle.us-east-1.api.aws/anthropic"}).(*anthropicProvider)
	req, _ := http.NewRequest("POST", "https://bedrock-mantle.us-east-1.api.aws/anthropic/v1/messages", nil)
	if err := p.endpoint(p.client).auth(req, []byte("{}"), ""); err != nil {
		t.Fatalf("auth with an exhausted pool: %v", err)
	}
	if v := req.Header.Get("Authorization"); v != "" {
		t.Fatalf("Authorization = %q, a pool-configured provider must never SigV4-sign", v)
	}
}

// LLM_API_KEY is a generic fallback that people keep set for another
// provider; for bedrock it would silently shadow IAM credentials.
func TestBedrockEnvCandidatesExcludeGenericLLMKey(t *testing.T) {
	if slices.Contains(ProviderEnvCandidates("bedrock"), "LLM_API_KEY") {
		t.Fatalf("ProviderEnvCandidates(bedrock) = %v, must not include LLM_API_KEY", ProviderEnvCandidates("bedrock"))
	}
	if !slices.Contains(ProviderEnvCandidates("deepseek"), "LLM_API_KEY") {
		t.Fatal("other providers keep the LLM_API_KEY fallback")
	}
}
