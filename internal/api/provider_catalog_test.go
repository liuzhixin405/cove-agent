package api

import "testing"

func TestDeepSeekVisionCapability(t *testing.T) {
	for model, want := range map[string]bool{
		"deepseek-flash":               true,
		"deepseek-v4-flash":            true,
		"deepseek-v4-flash-vision-exp": true,
		"DeepSeek/DeepSeek-Flash":      true,
		"deepseek-v4-pro":              false,
		"deepseek-chat":                false,
		"deepseek-reasoner":            false,
		"deepseek-v5":                  false,
	} {
		if got := IsVisionCapableModel(model); got != want {
			t.Errorf("IsVisionCapableModel(%q) = %v, want %v", model, got, want)
		}
	}
}

func TestNormalizeProviderNameAliases(t *testing.T) {
	cases := map[string]string{
		"anthropic":  "anthropic",
		"deepseek":   "deepseek",
		"openai":     "openai",
		"zhipu":      "glm",
		"moonshot":   "kimi",
		"dashscope":  "qwen",
		"ark":        "doubao",
		"grok":       "xai",
		"openrouter": "openrouter",
	}

	for in, want := range cases {
		if got := NormalizeProviderName(in); got != want {
			t.Fatalf("NormalizeProviderName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestKnownOpenAICompatibleProvidersUseExpectedBaseURL(t *testing.T) {
	cases := map[string]string{
		"deepseek":    "https://api.deepseek.com/v1",
		"openai":      "https://api.openai.com/v1",
		"glm":         "https://open.bigmodel.cn/api/paas/v4",
		"qwen":        "https://dashscope.aliyuncs.com/compatible-mode/v1",
		"kimi":        "https://api.moonshot.cn/v1",
		"doubao":      "https://ark.cn-beijing.volces.com/api/v3",
		"openrouter":  "https://openrouter.ai/api/v1",
		"siliconflow": "https://api.siliconflow.cn/v1",
		"groq":        "https://api.groq.com/openai/v1",
		"together":    "https://api.together.xyz/v1",
		"fireworks":   "https://api.fireworks.ai/inference/v1",
		"xai":         "https://api.x.ai/v1",
	}

	for provider, wantBaseURL := range cases {
		p := NewProvider(ProviderConfig{Name: provider, APIKey: "test-key"})
		op, ok := p.(*openAICompatProvider)
		if !ok {
			t.Fatalf("provider %q should use openAICompatProvider, got %T", provider, p)
		}
		if op.baseURL != wantBaseURL {
			t.Fatalf("provider %q baseURL = %q, want %q", provider, op.baseURL, wantBaseURL)
		}
	}
}

func TestCustomProviderUsesOpenAICompatWithCustomBaseURL(t *testing.T) {
	p := NewProvider(ProviderConfig{
		Name:    "mistral",
		APIKey:  "test-key",
		BaseURL: "https://api.mistral.ai/v1",
	})
	op, ok := p.(*openAICompatProvider)
	if !ok {
		t.Fatalf("custom provider should use openAICompatProvider, got %T", p)
	}
	if op.baseURL != "https://api.mistral.ai/v1" {
		t.Fatalf("custom provider baseURL = %q", op.baseURL)
	}
}

func TestProviderDisplayNamePreservesConfiguredProvider(t *testing.T) {
	p := NewProvider(ProviderConfig{Name: "deepseek", APIKey: "test-key"})
	if got, want := p.DisplayName(), "deepseek"; got != want {
		t.Fatalf("DisplayName() = %q, want %q", got, want)
	}
}
