package main

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/liuzhixin405/cove-agent/internal/api"
	"github.com/liuzhixin405/cove-agent/internal/config"
)

func TestProviderHelpLineListsExpandedProviders(t *testing.T) {
	line := providerHelpLine()
	checks := []string{
		"/provider <\u540d\u79f0>",
		"anthropic",
		"deepseek",
		"openai-compatible",
		"glm",
		"openrouter",
		"mistral",
	}
	for _, want := range checks {
		if !strings.Contains(line, want) {
			t.Fatalf("provider help line missing %q: %s", want, line)
		}
	}
}

func TestProviderEnvHelpLineListsExpandedEnvVars(t *testing.T) {
	line := providerEnvHelpLine()
	checks := []string{
		"LLM_API_KEY",
		"ANTHROPIC_API_KEY",
		"DEEPSEEK_API_KEY",
		"OPENAI_API_KEY",
		"GLM_API_KEY",
		"OPENROUTER_API_KEY",
		"SILICONFLOW_API_KEY",
		"LLM_BASE_URL",
	}
	for _, want := range checks {
		if !strings.Contains(line, want) {
			t.Fatalf("provider env help line missing %q: %s", want, line)
		}
	}
}

func TestMissingAPIKeyMessageIsShortAndPointsAtSetup(t *testing.T) {
	msg := missingAPIKeyMessage("deepseek")
	if n := strings.Count(msg, "\n"); n > 3 {
		t.Errorf("message has %d line breaks, want at most 3:\n%s", n, msg)
	}
	checks := []string{
		"未配置 API key",
		"deepseek",
		"/setup",
		"DEEPSEEK_API_KEY",
		"config.json",
	}
	for _, want := range checks {
		if !strings.Contains(msg, want) {
			t.Fatalf("message missing %q\nfull message:\n%s", want, msg)
		}
	}
}

type stubProviderReloader struct {
	calls []providerReloadCall
	err   error
}

type stubStreamingRunner struct {
	result string
	err    error
}

type providerReloadCall struct {
	provider string
	model    string
	baseURL  string
	apiKey   string
}

func (s *stubProviderReloader) ReloadProvider(provider, model, baseURL, apiKey string) error {
	s.calls = append(s.calls, providerReloadCall{
		provider: provider,
		model:    model,
		baseURL:  baseURL,
		apiKey:   apiKey,
	})
	return s.err
}

func (s stubStreamingRunner) RunWithStream(ctx context.Context, input string, onDelta func(delta string)) (string, error) {
	if s.err != nil {
		return "", s.err
	}
	if onDelta != nil {
		onDelta(s.result)
	}
	return s.result, nil
}

func TestApplyProviderConfigChangeReloadsProviderForAPIKey(t *testing.T) {
	reloader := &stubProviderReloader{}
	cfg := &config.Config{Model: "claude-sonnet-4-20250514"}

	if err := applyProviderConfigChange(cfg, reloader, func() error {
		cfg.Provider.APIKey = "sk-test"
		return nil
	}); err != nil {
		t.Fatalf("applyProviderConfigChange returned error: %v", err)
	}

	if len(reloader.calls) != 1 {
		t.Fatalf("expected 1 reload call, got %d", len(reloader.calls))
	}
	call := reloader.calls[0]
	if call.provider != "anthropic" {
		t.Fatalf("expected anthropic provider, got %q", call.provider)
	}
	if call.apiKey != "sk-test" {
		t.Fatalf("expected api key to be reloaded, got %q", call.apiKey)
	}
}

type filesAPIConfigReloader struct {
	stubProviderReloader
	configs []api.ProviderConfig
}

func (reloader *filesAPIConfigReloader) ReloadProviderConfig(cfg api.ProviderConfig, model string) error {
	reloader.configs = append(reloader.configs, cfg)
	return nil
}

func TestApplyProviderConfigChangeReloadsImageFilesAPI(t *testing.T) {
	reloader := &filesAPIConfigReloader{}
	cfg := &config.Config{Model: "deepseek-flash", Provider: config.ProviderConfig{Name: "deepseek", APIKey: "key"}}
	for index, enabled := range []bool{true, false} {
		if err := applyProviderConfigChange(cfg, reloader, func() error {
			value := enabled
			cfg.Provider.ImageFilesAPI = &value
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		if len(reloader.configs) != index+1 || reloader.configs[index].ImageFilesAPI != enabled {
			t.Fatalf("Files API toggle not reloaded: %+v", reloader.configs)
		}
	}
	if len(reloader.calls) != 0 {
		t.Fatal("full configuration was reduced to legacy reload fields")
	}
}

func TestProviderAPIConfigPreservesFields(t *testing.T) {
	enabled, disabled := true, false
	for _, flag := range []*bool{nil, &enabled, &disabled} {
		source := config.ProviderConfig{
			Name: "deepseek", APIKey: "primary-key", APIKeys: []string{"primary-key", "backup-key"},
			BaseURL: "https://example.invalid/v1", ImageFilesAPI: flag,
		}
		got := providerAPIConfig(source)
		if got.Name != source.Name || got.APIKey != source.APIKey || got.BaseURL != source.BaseURL ||
			!slices.Equal(got.APIKeys, source.APIKeys) || got.ImageFilesAPI != source.ImageFilesEnabled() {
			t.Fatal("provider configuration fields were lost or changed")
		}
	}
}

func TestApplyProviderConfigChangeReturnsReloadError(t *testing.T) {
	wantErr := errors.New("reload failed")
	reloader := &stubProviderReloader{err: wantErr}
	cfg := &config.Config{Model: "claude-sonnet-4-20250514"}

	err := applyProviderConfigChange(cfg, reloader, func() error {
		cfg.Provider.APIKey = "sk-test"
		return nil
	})
	if !errors.Is(err, wantErr) {
		t.Fatalf("expected reload error %v, got %v", wantErr, err)
	}
}

func TestApplyProviderConfigChangeRestoresOnFailure(t *testing.T) {
	wantErr := errors.New("reload failed")
	enabled := false
	cfg := &config.Config{Model: "deepseek-flash", Provider: config.ProviderConfig{
		Name: "deepseek", APIKey: "old", APIKeys: []string{"old"}, ImageFilesAPI: &enabled,
	}}
	err := applyProviderConfigChange(cfg, &stubProviderReloader{err: wantErr}, func() error {
		cfg.Model = "deepseek-v4-pro"
		cfg.Provider.APIKey = "new"
		cfg.Provider.APIKeys[0] = "new"
		*cfg.Provider.ImageFilesAPI = true
		return nil
	})
	if !errors.Is(err, wantErr) || cfg.Model != "deepseek-flash" || cfg.Provider.APIKey != "old" ||
		cfg.Provider.APIKeys[0] != "old" || cfg.Provider.ImageFilesEnabled() {
		t.Fatal("failed reload changed live configuration")
	}
}

func TestApplyProviderConfigChangeReloadsAPIKeys(t *testing.T) {
	reloader := &filesAPIConfigReloader{}
	cfg := &config.Config{Model: "deepseek-flash", Provider: config.ProviderConfig{
		Name: "deepseek", APIKey: "primary", APIKeys: []string{"backup"},
	}}
	if err := applyProviderConfigChange(cfg, reloader, func() error {
		cfg.Provider.APIKeys[0] = "replacement"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(reloader.configs) != 1 || reloader.configs[0].APIKeys[0] != "replacement" {
		t.Fatal("key list change did not reload the provider")
	}
}

func TestApplyProviderConfigChangePersistenceFailureRestoresProvider(t *testing.T) {
	wantErr := errors.New("save failed")
	reloader := &filesAPIConfigReloader{}
	cfg := &config.Config{Model: "deepseek-flash", Provider: config.ProviderConfig{Name: "deepseek", APIKey: "old"}}
	err := applyProviderConfigChange(cfg, reloader, func() error {
		cfg.Provider.APIKey = "new"
		return nil
	}, func() error {
		if len(reloader.configs) != 1 || reloader.configs[0].APIKey != "new" {
			t.Fatal("save ran before provider reload")
		}
		return wantErr
	})
	if !errors.Is(err, wantErr) || cfg.Provider.APIKey != "old" || len(reloader.configs) != 2 || reloader.configs[1].APIKey != "old" {
		t.Fatal("save failure did not restore configuration and provider")
	}
}

func TestApplyProviderConfigChangeReloadFailureDoesNotSave(t *testing.T) {
	wantErr := errors.New("reload failed")
	cfg := &config.Config{Model: "deepseek-flash", Provider: config.ProviderConfig{Name: "deepseek", APIKey: "old"}}
	saved := false
	err := applyProviderConfigChange(cfg, &stubProviderReloader{err: wantErr}, func() error {
		cfg.Provider.APIKey = "new"
		return nil
	}, func() error { saved = true; return nil })
	if !errors.Is(err, wantErr) || saved || cfg.Provider.APIKey != "old" {
		t.Fatal("failed reload persisted candidate configuration")
	}
}

func TestRunChatInteractionReturnsVisibleError(t *testing.T) {
	if testing.Short() {
		t.Skip("slow (>3s): skipped under -short")
	}
	runner := stubStreamingRunner{err: errors.New("api: connection refused")}
	out, err := runChatInteraction(context.Background(), runner, "hello")
	if err == nil {
		t.Fatalf("expected error, got nil")
	}
	if !strings.Contains(out, "请求失败：api: connection refused") {
		t.Fatalf("expected visible request failure, got %q", out)
	}
}

func TestRunChatInteractionStreamsAndTerminatesCleanly(t *testing.T) {
	runner := stubStreamingRunner{result: "MOCK_REPLY: hello"}
	out, err := runChatInteraction(context.Background(), runner, "hello")
	if err != nil {
		t.Fatalf("expected nil error, got %v", err)
	}
	if !strings.Contains(out, "MOCK_REPLY: hello") {
		t.Fatalf("expected streamed reply, got %q", out)
	}
	if !strings.HasSuffix(out, "\r\n\r\n") {
		t.Fatalf("expected trailing line break, got %q", out)
	}
}

func TestRunChatInteractionAppendsMissingFinalSuffix(t *testing.T) {
	runner := stubStreamingRunner{result: "hello world"}
	out, err := runChatInteraction(context.Background(), runner, "hello")
	if err != nil {
		t.Fatalf("expected nil error, got %v", err)
	}
	if !strings.Contains(out, "hello world") {
		t.Fatalf("expected final reply content to contain 'hello world', got %q", out)
	}
	if !strings.HasSuffix(out, "\r\n\r\n") {
		t.Fatalf("expected trailing line break, got %q", out)
	}
}

func TestApplyProviderConfigChangeTrimsModelAndProviderValues(t *testing.T) {
	reloader := &stubProviderReloader{}
	cfg := &config.Config{}

	err := applyProviderConfigChange(cfg, reloader, func() error {
		cfg.Model = "  deepseek-v4-pro  "
		cfg.Provider.Name = "  deepseek  "
		cfg.Provider.APIKey = "  sk-test  "
		cfg.Provider.BaseURL = "  https://api.deepseek.com  "
		return nil
	})
	if err != nil {
		t.Fatalf("applyProviderConfigChange returned error: %v", err)
	}
	if got, want := cfg.Model, "deepseek-v4-pro"; got != want {
		t.Fatalf("cfg.Model = %q, want %q", got, want)
	}
	if got, want := cfg.Provider.Name, "deepseek"; got != want {
		t.Fatalf("cfg.Provider.Name = %q, want %q", got, want)
	}
	if got, want := cfg.Provider.APIKey, "sk-test"; got != want {
		t.Fatalf("cfg.Provider.APIKey = %q, want %q", got, want)
	}
	if got, want := cfg.Provider.BaseURL, "https://api.deepseek.com"; got != want {
		t.Fatalf("cfg.Provider.BaseURL = %q, want %q", got, want)
	}
	if len(reloader.calls) != 1 {
		t.Fatalf("expected one reload call, got %d", len(reloader.calls))
	}
	call := reloader.calls[0]
	if got, want := call.model, "deepseek-v4-pro"; got != want {
		t.Fatalf("reload model = %q, want %q", got, want)
	}
	if got, want := call.provider, "deepseek"; got != want {
		t.Fatalf("reload provider = %q, want %q", got, want)
	}
	if got, want := call.apiKey, "sk-test"; got != want {
		t.Fatalf("reload api key = %q, want %q", got, want)
	}
	if got, want := call.baseURL, "https://api.deepseek.com"; got != want {
		t.Fatalf("reload baseURL = %q, want %q", got, want)
	}
}

func TestApplyProviderConfigChangeResolvesAutoModelForDeepSeek(t *testing.T) {
	reloader := &stubProviderReloader{}
	cfg := &config.Config{}

	err := applyProviderConfigChange(cfg, reloader, func() error {
		cfg.Model = "auto"
		cfg.Provider.Name = "deepseek"
		return nil
	})
	if err != nil {
		t.Fatalf("applyProviderConfigChange returned error: %v", err)
	}
	if got, want := cfg.Model, "deepseek-v4-pro"; got != want {
		t.Fatalf("cfg.Model = %q, want %q", got, want)
	}
	if len(reloader.calls) != 1 {
		t.Fatalf("expected one reload call, got %d", len(reloader.calls))
	}
	if got, want := reloader.calls[0].model, "deepseek-v4-pro"; got != want {
		t.Fatalf("reload model = %q, want %q", got, want)
	}
}

func TestKnownProviderValidation(t *testing.T) {
	if !api.IsKnownProvider("deepseek") {
		t.Fatalf("expected deepseek to be a known provider")
	}
	if !api.IsKnownProvider("openrouter") {
		t.Fatalf("expected openrouter to be a known provider")
	}
	if api.IsKnownProvider("invalid-provider") {
		t.Fatalf("expected invalid-provider to be rejected")
	}
}

func TestCompleteSuggestsToolArgumentNames(t *testing.T) {
	entries := []cmdEntry{{Name: "bash", Type: "tool", Args: []string{"command", "description", "timeout"}}}

	got := complete("bash co", entries, nil)
	if len(got) != 1 || got[0] != "bash command=" {
		t.Fatalf("complete tool arg = %#v, want bash command=", got)
	}

	got = complete("bash command=ls d", entries, nil)
	if len(got) != 1 || got[0] != "bash command=ls description=" {
		t.Fatalf("complete second tool arg = %#v, want description", got)
	}
}

func TestCompleteKeepsSlashCompletionToSlashCommands(t *testing.T) {
	entries := []cmdEntry{
		{Name: "/help", Type: "builtin"},
		{Name: "bash", Type: "tool", Args: []string{"command"}},
	}

	got := complete("/", entries, nil)
	if len(got) != 1 || got[0] != "/help" {
		t.Fatalf("slash completion = %#v, want only /help", got)
	}
}

func TestCompleteConfigValueHints(t *testing.T) {
	entries := []cmdEntry{{Name: "/mode", Type: "config", ArgHints: map[string][]string{"": {"default", "plan", "auto", "bypass"}}}}

	got := complete("/mode a", entries, nil)
	if len(got) != 1 || got[0] != "/mode auto" {
		t.Fatalf("mode completion = %#v, want /mode auto", got)
	}
}

func TestToolArgNamesRequiredFirst(t *testing.T) {
	raw := json.RawMessage(`{
		"type":"object",
		"properties":{"optional":{"type":"string"},"command":{"type":"string"},"timeout":{"type":"integer"}},
		"required":["command"]
	}`)

	got := toolArgNames(raw)
	want := []string{"command", "optional", "timeout"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("toolArgNames = %#v, want %#v", got, want)
	}
}
