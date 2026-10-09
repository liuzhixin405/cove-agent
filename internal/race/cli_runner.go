package race

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
)

type CLIRunner struct {
	Executable      func() (string, error)
	PrefixArgs      []string
	ConfigDirectory string
	// Profile is the profile the parent process runs with (its --profile);
	// empty means the config file's active_profile. The candidate's private
	// race-budget profile is built on top of it so provider, api_key and model
	// keep applying to the child.
	Profile string
}

func (r CLIRunner) Run(ctx context.Context, request Request) Execution {
	fail := func(err error) Execution {
		return Execution{Status: "start_error", ExitCode: -1, Error: err.Error(), CostSource: "unverified"}
	}
	locate := r.Executable
	if locate == nil {
		locate = os.Executable
	}
	executable, err := locate()
	if err != nil {
		return fail(err)
	}
	if request.BudgetUSD <= 0 {
		return fail(errors.New("CLI runner requires a positive budget"))
	}
	directory, err := os.MkdirTemp("", "cove-race-config-")
	if err != nil {
		return fail(err)
	}
	defer os.RemoveAll(directory)
	settings := map[string]any{}
	if r.ConfigDirectory != "" {
		data, err := os.ReadFile(filepath.Join(r.ConfigDirectory, "config.json"))
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return fail(err)
		}
		if err == nil {
			data = bytes.TrimPrefix(data, []byte{0xef, 0xbb, 0xbf})
			if err := json.Unmarshal(data, &settings); err != nil {
				return fail(err)
			}
			if settings == nil {
				return fail(errors.New("config must be a JSON object"))
			}
		}
	}
	settings["max_budget_usd"] = request.BudgetUSD
	// Start from the profile the parent runs with: replacing the whole
	// "profiles" map used to drop that profile's provider/api_key/model, so
	// both candidates ran against the base provider (often with no key).
	budgetProfile := map[string]any{}
	baseName := r.Profile
	if baseName == "" {
		baseName, _ = settings["active_profile"].(string)
	}
	if profiles, ok := settings["profiles"].(map[string]any); ok && baseName != "" {
		if base, ok := profiles[baseName].(map[string]any); ok {
			for key, value := range base {
				budgetProfile[key] = value
			}
		}
	}
	budgetProfile["max_budget_usd"] = request.BudgetUSD
	budgetProfile["permission_mode"] = "auto"
	budgetProfile["max_iterations"] = 12
	settings["active_profile"] = "race-budget"
	settings["profiles"] = map[string]any{"race-budget": budgetProfile}
	delete(settings, "mcp_servers")
	delete(settings, "memory_embedding")
	data, err := json.Marshal(settings)
	if err != nil {
		return fail(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "config.json"), data, 0600); err != nil {
		return fail(err)
	}
	argv := append([]string{executable}, r.PrefixArgs...)
	argv = append(argv, "-p", request.Prompt, "--no-tui", "--profile", "race-budget", "--max-turns", "12")
	return Execute(ctx, request.Directory, []string{"COVE_CONFIG_DIR=" + directory}, argv)
}
