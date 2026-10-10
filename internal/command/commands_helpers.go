package command

import (
	"encoding/json"
	"fmt"

	"github.com/liuzhixin405/cove-agent/internal/api"
	"github.com/liuzhixin405/cove-agent/internal/config"
	"github.com/liuzhixin405/cove-agent/internal/permission"
)

func humanBytes(n int) string {
	switch {
	case n >= 1024*1024:
		return fmt.Sprintf("%.1fMB", float64(n)/(1024*1024))
	case n >= 1024:
		return fmt.Sprintf("%.1fKB", float64(n)/1024)
	default:
		return fmt.Sprintf("%dB", n)
	}
}

func configValue(cfg *config.Config, key string) string {
	switch key {
	case "model":
		return cfg.Model
	case "provider":
		return cfg.Provider.Name
	case "api_key", "api-key":
		if cfg.Provider.APIKey == "" {
			return ""
		}
		return "[REDACTED]"
	case "base_url", "base-url":
		return cfg.Provider.BaseURL
	case "mode", "permission_mode", "permission-mode":
		return cfg.PermissionMode
	case "budget", "max_budget_usd", "max-budget-usd":
		return fmt.Sprintf("%.2f", cfg.MaxBudgetUsd)
	case "system", "system_prompt", "system-prompt":
		return cfg.SystemPrompt
	default:
		return ""
	}
}

func applyConfigValue(cfg *config.Config, key, value string) error {
	switch key {
	case "model":
		cfg.Model = value
	case "provider":
		// Saved unchecked, a typo only surfaced as a failure on the next start.
		if !api.IsKnownProvider(value) {
			return fmt.Errorf("unknown provider: %s", value)
		}
		cfg.Provider.Name = value
	case "api_key", "api-key":
		cfg.Provider.APIKey = value
	case "base_url", "base-url":
		cfg.Provider.BaseURL = value
	case "mode", "permission_mode", "permission-mode":
		if !permission.ValidMode(permission.Mode(value)) {
			return fmt.Errorf("invalid permission mode: %s", value)
		}
		cfg.PermissionMode = value
	case "budget", "max_budget_usd", "max-budget-usd":
		var v float64
		if _, err := fmt.Sscanf(value, "%f", &v); err != nil {
			return fmt.Errorf("invalid budget: %w", err)
		}
		cfg.MaxBudgetUsd = v
	case "system", "system_prompt", "system-prompt":
		cfg.SystemPrompt = value
	default:
		return fmt.Errorf("unknown config key: %s", key)
	}
	return nil
}

func renderConfig(cfg *config.Config) string {
	pc := cfg.EffectiveProvider()
	data, _ := json.MarshalIndent(map[string]any{
		"model":           cfg.Model,
		"provider":        pc.Name,
		"base_url":        pc.BaseURL,
		"permission_mode": cfg.PermissionMode,
		"max_budget_usd":  cfg.MaxBudgetUsd,
		"debug":           cfg.Debug,
		"api_key_set":     pc.APIKey != "",
		"system_prompt":   cfg.SystemPrompt,
		"mcp_servers":     len(cfg.MCPServers),
	}, "", "  ")
	return string(data)
}
