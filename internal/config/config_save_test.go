package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSavePreservesAPIKey(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "cove-config-test")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)

	os.Setenv("COVE_CONFIG_DIR", tmpDir)
	defer os.Unsetenv("COVE_CONFIG_DIR")

	cfg := DefaultConfig()
	cfg.Model = "deepseek-v4-pro"
	cfg.Provider.Name = "deepseek"
	cfg.Provider.APIKey = "sk-234...cdef"
	cfg.Provider.BaseURL = "https://api.deepseek.com/v1"
	cfg.MaxBudgetUsd = 5.0
	cfg.ThinkingTokens = 1024
	cfg.Debug = true
	cfg.SystemPrompt = "You are a helpful assistant."

	if err := Save(cfg); err != nil {
		t.Fatal(err)
	}

	// Read raw file — verify key is NOT masked
	raw, err := os.ReadFile(filepath.Join(tmpDir, "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	rawStr := string(raw)

	if strings.Contains(rawStr, "****") {
		t.Errorf("raw config.json contains masked key!\n%s", rawStr)
	}
	if !strings.Contains(rawStr, "sk-234...cdef") {
		t.Errorf("full API key not found in raw config!\n%s", rawStr)
	}

	// Load and verify roundtrip
	loaded, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Provider.APIKey != "sk-234...cdef" {
		t.Errorf("APIKey corrupted after roundtrip: got %q", loaded.Provider.APIKey)
	}
	if loaded.MaxBudgetUsd != 5.0 {
		t.Errorf("MaxBudgetUsd corrupted: %f", loaded.MaxBudgetUsd)
	}
	if !loaded.Debug {
		t.Error("Debug flag lost")
	}

	// Verify display masking still works
	display, _ := json.Marshal(loaded.Provider)
	dispStr := string(display)
	if !strings.Contains(dispStr, "****") {
		t.Errorf("display should mask key, got: %s", dispStr)
	}
	if strings.Contains(dispStr, "sk-234...cdef") {
		t.Errorf("display leaked full key: %s", dispStr)
	}
}

func TestDisplayMasking(t *testing.T) {
	pc := ProviderConfig{
		Name:   "deepseek",
		APIKey: "sk-1234567890abcdef",
	}
	data, err := json.Marshal(pc)
	if err != nil {
		t.Fatal(err)
	}
	disp := string(data)
	if !strings.Contains(disp, "****") {
		t.Errorf("display should contain masked portion, got: %s", disp)
	}
	if strings.Contains(disp, "sk-1234567890abcdef") {
		t.Errorf("display leaked full key: %s", disp)
	}
}

func TestImageFilesAPIConfigRoundTrip(t *testing.T) {
	t.Setenv("COVE_CONFIG_DIR", t.TempDir())
	cfg := DefaultConfig()
	if err := json.Unmarshal([]byte(`{"name":"deepseek","image_files_api":true}`), &cfg.Provider); err != nil {
		t.Fatal(err)
	}
	if err := Save(cfg); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(os.Getenv("COVE_CONFIG_DIR"), "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	var stored struct {
		Provider struct {
			ImageFilesAPI *bool `json:"image_files_api"`
		} `json:"provider"`
	}
	if err := json.Unmarshal(raw, &stored); err != nil {
		t.Fatal(err)
	}
	if stored.Provider.ImageFilesAPI == nil || !*stored.Provider.ImageFilesAPI {
		t.Fatal("Files API opt-in lost on save")
	}
	profileJSON, err := json.Marshal(cfg.SnapshotProfile())
	if err != nil {
		t.Fatal(err)
	}
	stored.Provider.ImageFilesAPI = nil
	if err := json.Unmarshal(profileJSON, &stored); err != nil {
		t.Fatal(err)
	}
	if stored.Provider.ImageFilesAPI == nil || !*stored.Provider.ImageFilesAPI {
		t.Fatal("Files API opt-in lost in profile snapshot")
	}
}

func TestNormalizeClearsMaskedKey(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Provider.APIKey = "sk-a****cdef"

	normalizeConfig(cfg)

	if cfg.Provider.APIKey != "" {
		t.Errorf("masked key should be cleared, got: %q", cfg.Provider.APIKey)
	}
}

func TestNormalizeKeepsValidKey(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Provider.APIKey = "sk-234...cdef"

	normalizeConfig(cfg)

	if cfg.Provider.APIKey != "sk-234...cdef" {
		t.Errorf("valid key should be preserved, got: %q", cfg.Provider.APIKey)
	}
}

func TestConfigNoAPIKeysInJSON(t *testing.T) {
	// APIKeys field should never appear in JSON serialization
	pc := ProviderConfig{
		Name:    "deepseek",
		APIKey:  "sk-test-key",
		APIKeys: []string{"key1", "key2"},
	}
	data, err := json.Marshal(pc)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "api_keys") {
		t.Errorf("api_keys should not appear in JSON: %s", string(data))
	}
}
