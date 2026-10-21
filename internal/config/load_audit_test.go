package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// isolate points the global config at a fresh directory and runs the test from
// a fresh project directory, so neither the user's real ~/.cove nor a
// .cove.json next to the package can leak in.
func isolate(t *testing.T) (globalDir, projectDir string) {
	t.Helper()
	root := t.TempDir()
	t.Setenv("HOME", root)
	t.Setenv("USERPROFILE", root)
	globalDir = filepath.Join(root, "global")
	projectDir = filepath.Join(root, "project")
	for _, d := range []string{globalDir, projectDir} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("COVE_CONFIG_DIR", globalDir)
	oldWD, _ := os.Getwd()
	t.Cleanup(func() { _ = os.Chdir(oldWD) })
	if err := os.Chdir(projectDir); err != nil {
		t.Fatal(err)
	}
	return globalDir, projectDir
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// The most common DeepSeek setup names only the provider. The model then has
// to be DeepSeek's default, not the Anthropic model DefaultConfig starts from:
// sending "claude-sonnet-4-20250514" to api.deepseek.com fails every request.
func TestLoadPicksProviderDefaultModelWhenModelIsOmitted(t *testing.T) {
	global, _ := isolate(t)
	writeFile(t, filepath.Join(global, "config.json"), `{"provider":{"name":"deepseek"}}`)

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Model != "deepseek-v4-pro" {
		t.Fatalf("Model = %q, want deepseek-v4-pro", cfg.Model)
	}
	if cfg.ModelFast != "deepseek-v4-pro" {
		t.Fatalf("ModelFast = %q, want the main model", cfg.ModelFast)
	}
}

// Windows PowerShell 5.1 writes UTF-8 with a byte-order mark (Out-File,
// Set-Content -Encoding utf8), and so does Notepad on older builds; the shipped
// docs/config.example.json starts with one too. encoding/json rejects the BOM,
// so such a config failed to parse and every setting in it was ignored.
func TestLoadAcceptsUTF8BOM(t *testing.T) {
	global, project := isolate(t)
	writeFile(t, filepath.Join(global, "config.json"), bomPrefix+`{"model":"deepseek-flash","provider":{"name":"deepseek"}}`)
	writeFile(t, filepath.Join(project, ".cove.json"), bomPrefix+`{"effort":"high"}`)

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Model != "deepseek-flash" || cfg.Effort != "high" {
		t.Fatalf("model=%q effort=%q: BOM-prefixed files were not applied", cfg.Model, cfg.Effort)
	}
}

// A misspelled profile name used to be ignored without a word, so
// `cove --profile wrok` silently ran with the base settings.
func TestLoadWithUnknownProfileReportsIt(t *testing.T) {
	global, _ := isolate(t)
	writeFile(t, filepath.Join(global, "config.json"), `{"model":"base","profiles":{"work":{"model":"work-model"}}}`)

	cfg, err := LoadWithProfile("wrok")
	if err == nil || !strings.Contains(err.Error(), "wrok") {
		t.Fatalf("err = %v, want an error naming the missing profile", err)
	}
	if cfg == nil || cfg.Model != "base" {
		t.Fatalf("cfg = %+v, want the base config to stay usable", cfg)
	}
}

// CheckFile is what /diagnose uses to say whether a config file would load.
func TestCheckFileReportsWhatLoadWouldReject(t *testing.T) {
	dir := t.TempDir()
	missing := filepath.Join(dir, "missing.json")
	if err := CheckFile(missing); err != nil {
		t.Fatalf("missing file: %v, want nil (cove runs without one)", err)
	}
	good := filepath.Join(dir, "good.json")
	writeFile(t, good, bomPrefix+`{"model":"m"}`)
	if err := CheckFile(good); err != nil {
		t.Fatalf("BOM-prefixed valid file: %v", err)
	}
	bad := filepath.Join(dir, "bad.json")
	writeFile(t, bad, `{"max_budget_usd":"10"}`) // a string where Load wants a number
	if err := CheckFile(bad); err == nil || !strings.Contains(err.Error(), "bad.json") {
		t.Fatalf("err = %v, want an error naming bad.json", err)
	}
}

// docs/config.example.json is what users copy to ~/.cove/config.json. It is
// saved with a BOM, so before Load stripped BOMs the copied file did not load.
func TestShippedExampleConfigLoads(t *testing.T) {
	global, _ := isolate(t)
	data, err := os.ReadFile(filepath.Join(testSourceDir(t), "..", "..", "docs", "config.example.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(global, "config.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load()
	if err != nil {
		t.Fatalf("the shipped example config does not load: %v", err)
	}
	// The example ships active_profile "bedrock" and no top-level model,
	// provider or budget: Load must resolve them from that profile rather
	// than leaving the built-in defaults.
	if cfg.Provider.Name != "bedrock" || cfg.Model != "anthropic.claude-opus-5-5" || cfg.ModelFast != "anthropic.claude-haiku-4-5" || cfg.MaxBudgetUsd != 20 {
		t.Fatalf("example settings not applied: provider=%q model=%q budget=%v",
			cfg.Provider.Name, cfg.Model, cfg.MaxBudgetUsd)
	}
}

// testSourceDir is the package directory; isolate() changes the working
// directory, so relative paths must be resolved before or independently.
func testSourceDir(t *testing.T) string {
	t.Helper()
	return sourceDir
}

var sourceDir = func() string {
	wd, _ := os.Getwd()
	return wd
}()

// bomPrefix is the UTF-8 encoding of U+FEFF, spelled as bytes so the source
// file itself does not start containing a byte-order mark.
const bomPrefix = "\xef\xbb\xbf"
