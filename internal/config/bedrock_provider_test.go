package config

import "testing"

func TestEffectiveProviderBedrockReadsBearerTokenAndRegion(t *testing.T) {
	t.Setenv("LLM_API_KEY", "")
	t.Setenv("LLM_BASE_URL", "")
	t.Setenv("AWS_BEARER_TOKEN_BEDROCK", "bedrock-token")
	t.Setenv("AWS_REGION", "ap-northeast-1")
	t.Setenv("AWS_DEFAULT_REGION", "")

	pc := (&Config{Provider: ProviderConfig{Name: "bedrock"}}).EffectiveProvider()
	if pc.Name != "bedrock" {
		t.Fatalf("Name = %q", pc.Name)
	}
	if pc.APIKey != "bedrock-token" {
		t.Fatalf("APIKey = %q, want AWS_BEARER_TOKEN_BEDROCK", pc.APIKey)
	}
	if want := "https://bedrock-mantle.ap-northeast-1.api.aws/anthropic"; pc.BaseURL != want {
		t.Fatalf("BaseURL = %q, want %q", pc.BaseURL, want)
	}
}

func TestEffectiveProviderBedrockKeepsConfiguredBaseURL(t *testing.T) {
	t.Setenv("LLM_BASE_URL", "")
	t.Setenv("AWS_REGION", "us-east-1")
	pc := (&Config{Provider: ProviderConfig{Name: "bedrock", APIKey: "k", BaseURL: "https://proxy.example.com/anthropic"}}).EffectiveProvider()
	if pc.BaseURL != "https://proxy.example.com/anthropic" {
		t.Fatalf("BaseURL = %q, want the configured proxy", pc.BaseURL)
	}
}

func TestDefaultModelForBedrockIsPrefixedSonnet5(t *testing.T) {
	if got, want := DefaultModelForProvider("bedrock"), "anthropic.claude-sonnet-5"; got != want {
		t.Fatalf("DefaultModelForProvider(bedrock) = %q, want %q", got, want)
	}
	if got, want := ResolveModelForProvider("auto", "aws"), "anthropic.claude-sonnet-5"; got != want {
		t.Fatalf("ResolveModelForProvider(auto, aws) = %q, want %q", got, want)
	}
}

func TestEffectiveProviderBedrockIgnoresGenericLLMKey(t *testing.T) {
	t.Setenv("LLM_BASE_URL", "")
	t.Setenv("AWS_BEARER_TOKEN_BEDROCK", "")
	t.Setenv("BEDROCK_API_KEY", "")
	t.Setenv("LLM_API_KEY", "key-for-another-provider")
	pc := (&Config{Provider: ProviderConfig{Name: "bedrock"}}).EffectiveProvider()
	if pc.APIKey != "" {
		t.Fatalf("APIKey = %q, LLM_API_KEY must not shadow IAM credentials", pc.APIKey)
	}
}

func TestHasProviderCredentialsCountsBedrockIAM(t *testing.T) {
	dir := t.TempDir()
	for k, v := range map[string]string{
		"AWS_BEARER_TOKEN_BEDROCK": "", "BEDROCK_API_KEY": "", "LLM_API_KEY": "", "AWS_SESSION_TOKEN": "", "AWS_PROFILE": "",
		"AWS_SHARED_CREDENTIALS_FILE": dir + "/credentials", "AWS_CONFIG_FILE": dir + "/config",
		"AWS_ACCESS_KEY_ID": "AKIDEXAMPLE", "AWS_SECRET_ACCESS_KEY": "secret",
	} {
		t.Setenv(k, v)
	}
	if !HasProviderCredentials(ProviderConfig{Name: "bedrock"}) {
		t.Fatal("bedrock with IAM credentials has credentials")
	}
	if HasProviderCredentials(ProviderConfig{Name: "anthropic"}) {
		t.Fatal("IAM credentials do not count for anthropic")
	}
}
