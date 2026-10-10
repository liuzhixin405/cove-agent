package config

import (
	"path/filepath"
	"testing"
)

// /profile save built the profile from the live config, so the values a
// (trusted) project .cove.json set — its provider base_url and api_key, its
// system prompt, permission mode, model and budget — were copied into the
// global profiles, and `/profile switch` in any other project then sent the
// user's traffic to that project's endpoint. The profile records only what
// the user's own config says, plus what the user changed in the session.
func TestSnapshotProfileLeavesOutProjectValues(t *testing.T) {
	global, project := isolate(t)
	writeFile(t, filepath.Join(global, "config.json"),
		`{"model":"user-model","provider":{"name":"deepseek","api_key":"sk-global"},"system_prompt":"mine","max_budget_usd":5,"thinking_tokens":20000}`)
	projectPath := filepath.Join(project, ".cove.json")
	writeFile(t, projectPath, `{
		"model":"project-model",
		"model_fast":"project-fast",
		"provider":{"base_url":"https://proxy.example/v1","api_key":"sk-project"},
		"permission_mode":"bypass",
		"system_prompt":"project rules",
		"max_budget_usd":500,
		"thinking_tokens":30000
	}`)
	if err := TrustProjectConfig(projectPath); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Provider.BaseURL != "https://proxy.example/v1" || cfg.SystemPrompt != "project rules" {
		t.Fatalf("trusted project config not applied: %+v %q", cfg.Provider, cfg.SystemPrompt)
	}

	p := cfg.SnapshotProfile()
	if p.Provider == nil || p.Provider.Name != "deepseek" || p.Provider.APIKey != "sk-global" || p.Provider.BaseURL != "" {
		t.Fatalf("profile provider = %+v, want the user's", p.Provider)
	}
	if p.Model != "user-model" || p.ModelFast != "user-model" || p.PermissionMode != "default" ||
		p.SystemPrompt != "mine" || p.MaxBudgetUsd != 5 {
		t.Fatalf("project values saved into the profile: %+v", p)
	}

	// What the user changed during the session is theirs and is saved.
	cfg.Model = "picked-in-session"
	cfg.Provider.BaseURL = "https://mine.example"
	cfg.MaxBudgetUsd = 7
	p = cfg.SnapshotProfile()
	if p.Model != "picked-in-session" || p.Provider.BaseURL != "https://mine.example" || p.MaxBudgetUsd != 7 {
		t.Fatalf("session changes lost: %+v %+v", p, p.Provider)
	}
}

// Without a project override the profile is the live config, as before.
func TestSnapshotProfileWithoutProjectConfig(t *testing.T) {
	global, _ := isolate(t)
	writeFile(t, filepath.Join(global, "config.json"),
		`{"model":"m","provider":{"name":"openai","api_key":"k","base_url":"https://b"},"debug":true}`)
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	p := cfg.SnapshotProfile()
	if p.Model != "m" || p.Provider.BaseURL != "https://b" || p.Debug == nil || !*p.Debug || p.Verbose == nil || *p.Verbose {
		t.Fatalf("profile = %+v", p)
	}
	cfg.Debug = false
	if !*p.Debug {
		t.Fatal("the saved profile aliases the live config")
	}
}
