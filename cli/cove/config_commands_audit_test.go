package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/liuzhixin405/cove-agent/internal/config"
)

func TestProviderSwitchModel(t *testing.T) {
	cases := []struct{ oldProv, newProv, model, want string }{
		// The old provider's default follows the switch...
		{"anthropic", "deepseek", config.DefaultModelForProvider("anthropic"), config.DefaultModelForProvider("deepseek")},
		{"deepseek", "openai", config.DefaultModelForProvider("deepseek"), config.DefaultModelForProvider("openai")},
		{"anthropic", "deepseek", "", config.DefaultModelForProvider("deepseek")},
		{"anthropic", "deepseek", "auto", config.DefaultModelForProvider("deepseek")},
		// ...a model the user chose stays.
		{"openai-compatible", "glm", "glm-4.6", "glm-4.6"},
		{"deepseek", "openrouter", "deepseek-v4-flash", "deepseek-v4-flash"},
	}
	for _, c := range cases {
		if got := providerSwitchModel(c.oldProv, c.newProv, c.model); got != c.want {
			t.Errorf("providerSwitchModel(%q, %q, %q) = %q, want %q", c.oldProv, c.newProv, c.model, got, c.want)
		}
	}
}

func savedConfig(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(os.Getenv("COVE_CONFIG_DIR"), "config.json"))
	if err != nil {
		t.Fatalf("read saved config: %v", err)
	}
	return string(data)
}

// "/provider deepseek" kept claude-sonnet-4 as the model, and saved the file
// before the model was even looked at.
func TestProviderCommandMovesDefaultModelAndSavesIt(t *testing.T) {
	eng := newTestEngine(t)
	captureOut(t)
	cfg := config.DefaultConfig()
	cfg.Provider.Name = "anthropic"
	cfg.Provider.APIKey = "placeholder"
	cfg.Model = config.DefaultModelForProvider("anthropic")

	handleBuiltinConfigCommand("/provider deepseek", cfg, eng)

	want := config.DefaultModelForProvider("deepseek")
	if cfg.Model != want || eng.Model() != want {
		t.Fatalf("model = %q (engine %q), want %q", cfg.Model, eng.Model(), want)
	}
	if saved := savedConfig(t); !strings.Contains(saved, want) {
		t.Fatalf("saved config does not carry the new model:\n%s", saved)
	}
}

// config.Save refuses to overwrite a config.json it cannot parse; /mode and
// /budget discarded that error and reported success.
func TestConfigCommandsReportSaveFailure(t *testing.T) {
	for _, in := range []string{"/mode auto", "/budget save"} {
		eng := newTestEngine(t)
		dir := os.Getenv("COVE_CONFIG_DIR")
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte("{not json"), 0o600); err != nil {
			t.Fatal(err)
		}
		buf := captureOut(t)
		handleBuiltinConfigCommand(in, config.DefaultConfig(), eng)
		if !strings.Contains(buf.String(), "保存失败") {
			t.Errorf("%s: save failure not reported, output %q", in, buf.String())
		}
	}
}

func TestProviderCommandsRestoreConfigurationOnSaveFailure(t *testing.T) {
	for _, input := range []string{"/model deepseek-v4-pro", "/provider openai", "/api-key replacement", "/base-url https://example.test/v1"} {
		t.Run(input, func(t *testing.T) {
			eng := newTestEngine(t)
			captureOut(t)
			cfg := config.DefaultConfig()
			cfg.Model = "deepseek-flash"
			cfg.Provider = config.ProviderConfig{Name: "deepseek", APIKey: "old", BaseURL: "https://api.deepseek.com"}
			if err := eng.ReloadProviderConfig(providerAPIConfig(cfg.Provider), cfg.Model); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(os.Getenv("COVE_CONFIG_DIR"), "config.json")
			if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte("{not json"), 0600); err != nil {
				t.Fatal(err)
			}
			handleBuiltinConfigCommand(input, cfg, eng)
			if cfg.Model != "deepseek-flash" || eng.Model() != "deepseek-flash" || cfg.Provider.Name != "deepseek" ||
				cfg.Provider.APIKey != "old" || cfg.Provider.BaseURL != "https://api.deepseek.com" {
				t.Fatal("save failure left candidate configuration active")
			}
			if savedConfig(t) != "{not json" {
				t.Fatal("save failure overwrote the original config file")
			}
		})
	}
}
