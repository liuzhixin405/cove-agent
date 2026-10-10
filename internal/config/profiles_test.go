package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadWithProfileAppliesOverrides(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "cove-profile-test")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)

	os.Setenv("COVE_CONFIG_DIR", tmpDir)
	defer os.Unsetenv("COVE_CONFIG_DIR")

	cfg := DefaultConfig()
	cfg.Model = "base-model"
	cfg.ModelFast = "base-fast"
	cfg.Provider.Name = "openai"
	cfg.Provider.APIKey = "sk-base"
	cfg.MaxBudgetUsd = 3.5
	cfg.Profiles = map[string]*Profile{
		"work": {
			Model:        "work-model",
			ModelFast:    "work-fast",
			Provider:     &ProviderConfig{Name: "deepseek", APIKey: "sk-work"},
			MaxBudgetUsd: 8,
		},
	}
	cfg.ActiveProfile = "work"

	if err := Save(cfg); err != nil {
		t.Fatalf("save config: %v", err)
	}

	loaded, err := LoadWithProfile("")
	if err != nil {
		t.Fatalf("load config: %v", err)
	}

	if loaded.Model != "work-model" {
		t.Fatalf("expected profile model override, got %q", loaded.Model)
	}
	if loaded.ModelFast != "work-fast" {
		t.Fatalf("expected profile fast model override, got %q", loaded.ModelFast)
	}
	if loaded.Provider.Name != "deepseek" {
		t.Fatalf("expected provider override, got %q", loaded.Provider.Name)
	}
	if loaded.Provider.APIKey != "sk-work" {
		t.Fatalf("expected provider API key override, got %q", loaded.Provider.APIKey)
	}
	if loaded.MaxBudgetUsd != 8 {
		t.Fatalf("expected profile max budget override, got %v", loaded.MaxBudgetUsd)
	}

	if _, err := os.Stat(filepath.Join(tmpDir, "config.json")); err != nil {
		t.Fatalf("expected config file to exist: %v", err)
	}
}

func TestProfileUnmarshalLegacyModeFallback(t *testing.T) {
	raw := []byte(`{"model":"m1","mode":"auto"}`)
	var p Profile
	if err := json.Unmarshal(raw, &p); err != nil {
		t.Fatalf("unmarshal profile: %v", err)
	}
	if p.PermissionMode != "auto" {
		t.Fatalf("expected legacy mode to map to permission_mode, got %q", p.PermissionMode)
	}
}

func TestSaveProfilesDoesNotEmitLegacyModeField(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "cove-profile-save-test")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)

	os.Setenv("COVE_CONFIG_DIR", tmpDir)
	defer os.Unsetenv("COVE_CONFIG_DIR")

	cfg := DefaultConfig()
	cfg.Profiles = map[string]*Profile{
		"work": {
			Model:          "m1",
			PermissionMode: "auto",
		},
	}
	if err := Save(cfg); err != nil {
		t.Fatalf("save config: %v", err)
	}
	b, err := os.ReadFile(filepath.Join(tmpDir, "config.json"))
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	s := string(b)
	if strings.Contains(s, `"mode"`) {
		t.Fatalf("config should not contain legacy profile mode field: %s", s)
	}
}
