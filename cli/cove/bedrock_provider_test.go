package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/liuzhixin405/cove-agent/internal/config"
)

func TestRunNeedsAPIKeyAcceptsBedrockIAMCredentials(t *testing.T) {
	for _, k := range []string{"AWS_BEARER_TOKEN_BEDROCK", "LLM_API_KEY", "BEDROCK_API_KEY", "AWS_SESSION_TOKEN", "AWS_PROFILE"} {
		t.Setenv(k, "")
	}
	dir := t.TempDir()
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", filepath.Join(dir, "credentials"))
	t.Setenv("AWS_CONFIG_FILE", filepath.Join(dir, "config"))
	t.Setenv("AWS_ACCESS_KEY_ID", "")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "")

	if !runNeedsAPIKey(config.ProviderConfig{Name: "bedrock"}) {
		t.Fatal("bedrock without any credentials needs setup")
	}
	if !runNeedsAPIKey(config.ProviderConfig{Name: "deepseek"}) || runNeedsAPIKey(config.ProviderConfig{Name: "deepseek", APIKey: "k"}) {
		t.Fatal("key-based providers unchanged")
	}
	t.Setenv("AWS_ACCESS_KEY_ID", "AKIDEXAMPLE")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "secret")
	if runNeedsAPIKey(config.ProviderConfig{Name: "bedrock"}) {
		t.Fatal("bedrock with IAM credentials must not be sent to setup")
	}
	if !runNeedsAPIKey(config.ProviderConfig{Name: "anthropic"}) {
		t.Fatal("AWS credentials do not satisfy the anthropic provider")
	}
	note, ok := setupCredentialsWithoutKey("bedrock")
	if !ok || !strings.Contains(note, "env") {
		t.Fatalf("setupCredentialsWithoutKey(bedrock) = %q,%v", note, ok)
	}
	if _, ok := setupCredentialsWithoutKey("anthropic"); ok {
		t.Fatal("only bedrock may proceed without a key")
	}
	if got := awsCredentialsSource(config.ProviderConfig{Name: "bedrock"}); got != "env" {
		t.Fatalf("awsCredentialsSource = %q, want env", got)
	}
	if got := awsCredentialsSource(config.ProviderConfig{Name: "bedrock", APIKey: "tok"}); got != "" {
		t.Fatalf("bearer configured: awsCredentialsSource = %q, want empty", got)
	}
}

func TestMissingAPIKeyMessageForBedrockListsBothWays(t *testing.T) {
	msg := missingAPIKeyMessage("bedrock")
	if !strings.Contains(msg, "AWS_BEARER_TOKEN_BEDROCK") || !strings.Contains(msg, "AWS_ACCESS_KEY_ID") {
		t.Fatalf("message = %q", msg)
	}
	if strings.Contains(missingAPIKeyMessage("deepseek"), "AWS_ACCESS_KEY_ID") {
		t.Fatal("other providers must not mention AWS")
	}
}

func TestSetupProvidersEndWithBedrockSoNumbersStayStable(t *testing.T) {
	last := setupProviders[len(setupProviders)-1]
	if last.Name != "bedrock" {
		t.Fatalf("last setup provider = %q, want bedrock (existing tests type the index of openai-compatible)", last.Name)
	}
	if setupProviders[9].Name != "openai-compatible" {
		t.Fatalf("setupProviders[9] = %q, the wizard tests type 10 for it", setupProviders[9].Name)
	}
}

func TestPreferredVisionModelForBedrock(t *testing.T) {
	if got := preferredVisionModelForProvider("bedrock", "anthropic.claude-sonnet-5"); got != "anthropic.claude-sonnet-5" {
		t.Fatalf("vision-capable model should be kept, got %q", got)
	}
	if got, want := preferredVisionModelForProvider("bedrock", "not-a-vision-model"), "anthropic.claude-sonnet-5"; got != want {
		t.Fatalf("preferredVisionModelForProvider(bedrock) = %q, want %q", got, want)
	}
}

func TestProviderHelpMentionsBedrock(t *testing.T) {
	if !strings.Contains(providerEnvHelpLine(), "AWS_BEARER_TOKEN_BEDROCK") {
		t.Fatalf("env help line lacks AWS_BEARER_TOKEN_BEDROCK: %s", providerEnvHelpLine())
	}
	if !strings.Contains(providerHelpLine(), "bedrock") {
		t.Fatalf("provider help line lacks bedrock: %s", providerHelpLine())
	}
}

// HasCredentials is a bool; the message must still say what was wrong
// (half-set environment, unsupported SSO profile) for bedrock.
func TestMissingAPIKeyMessageForBedrockCarriesResolveDetail(t *testing.T) {
	dir := t.TempDir()
	for k, v := range map[string]string{
		"AWS_BEARER_TOKEN_BEDROCK": "", "BEDROCK_API_KEY": "", "LLM_API_KEY": "", "AWS_SESSION_TOKEN": "", "AWS_PROFILE": "",
		"AWS_SHARED_CREDENTIALS_FILE": filepath.Join(dir, "credentials"), "AWS_CONFIG_FILE": filepath.Join(dir, "config"),
		"AWS_ACCESS_KEY_ID": "AKIDEXAMPLE", "AWS_SECRET_ACCESS_KEY": "",
	} {
		t.Setenv(k, v)
	}
	if msg := missingAPIKeyMessage("bedrock"); !strings.Contains(msg, "AWS_SECRET_ACCESS_KEY is missing") {
		t.Fatalf("half-set environment not explained:\n%s", msg)
	}
	t.Setenv("AWS_ACCESS_KEY_ID", "")
	if err := os.WriteFile(filepath.Join(dir, "config"), []byte("[default]\nsso_start_url = https://x.awsapps.com/start\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if msg := missingAPIKeyMessage("bedrock"); !strings.Contains(msg, "aws configure export-credentials") {
		t.Fatalf("SSO profile not explained:\n%s", msg)
	}
}
