package diagnostic

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/liuzhixin405/cove-agent/internal/config"
	"github.com/liuzhixin405/cove-agent/internal/shell"
)

// keyEnvs are all the variables EffectiveProvider may read a key from for the
// providers these tests use; they are blanked so the developer's own keys
// cannot make a check pass.
var keyEnvs = []string{
	"LLM_API_KEY", "LLM_BASE_URL", "ANTHROPIC_API_KEY", "DEEPSEEK_API_KEY", "OPENAI_API_KEY",
	"GLM_API_KEY", "ZHIPU_API_KEY",
}

// newTestChecker isolates HOME, the config dir and the working directory.
func newTestChecker(t *testing.T, cfg *config.Config) (*Checker, string) {
	t.Helper()
	root := t.TempDir()
	t.Setenv("HOME", root)
	t.Setenv("USERPROFILE", root)
	cfgDir := filepath.Join(root, "cfg")
	t.Setenv("COVE_CONFIG_DIR", cfgDir)
	for _, k := range keyEnvs {
		t.Setenv(k, "")
	}
	project := filepath.Join(root, "project")
	if err := os.MkdirAll(project, 0o755); err != nil {
		t.Fatal(err)
	}
	c := NewChecker(cfg)
	c.cwd = project
	return c, root
}

func problem(r CheckResult) bool {
	return !r.Skipped && r.Status >= SevWarning && r.Status != SevRecovered
}

// The "auto-fix" for a missing config.json wrote one with the API key
// "sk-xxxxxxxx…". It ran from QuickCheck on every start, so a user who keeps
// the key in DEEPSEEK_API_KEY got the placeholder written on first launch and,
// because a key in the file beats the environment, a 401 on every request
// from the second launch on. It also switched telemetry on.
func TestCheckConfigExistsNeverWritesAPlaceholderConfig(t *testing.T) {
	c, _ := newTestChecker(t, &config.Config{Provider: config.ProviderConfig{Name: "deepseek"}})
	t.Setenv("DEEPSEEK_API_KEY", "sk-from-env")

	res := c.checkConfigExists(t.Context())

	if problem(res) {
		t.Fatalf("missing config.json with a key in the environment reported as a problem: %+v", res.Error)
	}
	if _, err := os.Stat(filepath.Join(c.configDir, "config.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the check created config.json (err=%v)", err)
	}
	if _, err := os.Stat(filepath.Join(c.homeDir, ".cove", "config.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the check created ~/.cove/config.json (err=%v)", err)
	}
}

// Without a file and without a key, cove cannot talk to any provider; that is
// worth a warning, and the hint must not send the user to /init (which writes
// CLAUDE.md, not a config).
func TestCheckConfigExistsWarnsWhenThereIsNoConfigAndNoKey(t *testing.T) {
	c, _ := newTestChecker(t, &config.Config{Provider: config.ProviderConfig{Name: "deepseek"}})

	res := c.checkConfigExists(t.Context())

	if res.Status != SevWarning || res.Error == nil || res.Error.Def.Code != ErrConfigMissing {
		t.Fatalf("got %+v, want a warning with %s", res, ErrConfigMissing)
	}
	if strings.Contains(res.Error.Def.Recovery, "/init") {
		t.Fatalf("recovery points at /init, which does not create a config: %q", res.Error.Def.Recovery)
	}
}

// COVE_CONFIG_DIR moves the config; the check looked only at ~/.cove.
func TestCheckConfigExistsHonorsCOVE_CONFIG_DIR(t *testing.T) {
	c, _ := newTestChecker(t, &config.Config{Provider: config.ProviderConfig{Name: "deepseek"}})
	if err := os.MkdirAll(c.configDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(c.configDir, "config.json"), []byte(`{"model":"m"}`), 0o600); err != nil {
		t.Fatal(err)
	}

	if res := c.checkConfigExists(t.Context()); problem(res) {
		t.Fatalf("config in COVE_CONFIG_DIR not found: %+v", res.Error)
	}
}

// "配置格式" only checked that the in-memory config was non-nil, so a
// config.json with a syntax error (whose settings Load had just discarded)
// was reported as fine.
func TestCheckConfigValidReportsASyntaxError(t *testing.T) {
	c, _ := newTestChecker(t, &config.Config{})
	if err := os.MkdirAll(c.configDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(c.configDir, "config.json"), []byte(`{"model":"m",}`), 0o600); err != nil {
		t.Fatal(err)
	}

	res := c.checkConfigValid(t.Context())
	if res.Status < SevError || res.Error == nil || res.Error.Def.Code != ErrConfigInvalid {
		t.Fatalf("got %+v, want %s", res, ErrConfigInvalid)
	}
	if !strings.Contains(res.Error.Detail, "config.json") {
		t.Fatalf("detail %q does not say which file is broken", res.Error.Detail)
	}
}

func TestCheckConfigValidReportsABrokenProjectFile(t *testing.T) {
	c, _ := newTestChecker(t, &config.Config{})
	if err := os.WriteFile(filepath.Join(c.cwd, ".cove.json"), []byte(`{"effort": high}`), 0o644); err != nil {
		t.Fatal(err)
	}

	res := c.checkConfigValid(t.Context())
	if res.Status < SevError || !strings.Contains(res.Error.Detail, ".cove.json") {
		t.Fatalf("got %+v, want the broken .cove.json reported", res)
	}
}

// The check knew four environment variables. A GLM user with GLM_API_KEY was
// told the key was missing, and an empty provider name (which cove runs as
// anthropic) was reported as fatal.
func TestCheckAPIKeyFindsProviderSpecificEnvVars(t *testing.T) {
	c, _ := newTestChecker(t, &config.Config{Provider: config.ProviderConfig{Name: "glm"}})
	t.Setenv("GLM_API_KEY", "glm-key")

	if res := c.checkAPIKey(t.Context()); problem(res) {
		t.Fatalf("GLM_API_KEY not recognised: %+v", res.Error)
	}
}

func TestCheckAPIKeyTreatsEmptyProviderAsAnthropic(t *testing.T) {
	c, _ := newTestChecker(t, &config.Config{})
	t.Setenv("ANTHROPIC_API_KEY", "sk-ant-env")

	if res := c.checkAPIKey(t.Context()); problem(res) {
		t.Fatalf("empty provider with ANTHROPIC_API_KEY reported: %+v", res.Error)
	}
}

// DEEPSEEK_API_KEY is no use to the openai provider; it used to count.
func TestCheckAPIKeyIgnoresAnotherProvidersKey(t *testing.T) {
	c, _ := newTestChecker(t, &config.Config{Provider: config.ProviderConfig{Name: "openai"}})
	t.Setenv("DEEPSEEK_API_KEY", "sk-deepseek")

	if res := c.checkAPIKey(t.Context()); !problem(res) {
		t.Fatal("openai provider with only DEEPSEEK_API_KEY set passed the key check")
	}
}

// Configs written by the old auto-fix still carry its placeholder key.
func TestCheckAPIKeyFlagsThePlaceholderKey(t *testing.T) {
	c, _ := newTestChecker(t, &config.Config{Provider: config.ProviderConfig{Name: "deepseek", APIKey: "sk-xxxxxxxxxxxxxxxxxxxxxxxxxxxxxx"}})

	res := c.checkAPIKey(t.Context())
	if !problem(res) || res.Status < SevError {
		t.Fatalf("placeholder key passed: %+v", res)
	}
}

// An http:// endpoint (a local proxy or Ollama) got a TLS handshake anyway,
// which failed and was reported as "TLS失败".
func TestCheckNetworkAcceptsPlainHTTPEndpoint(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" || r.Header.Get("x-api-key") != "" {
			t.Errorf("the reachability probe sent credentials")
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()
	c, _ := newTestChecker(t, &config.Config{Provider: config.ProviderConfig{Name: "openai-compatible", APIKey: "sk-k", BaseURL: srv.URL + "/v1"}})

	res := c.checkNetworkReachable(t.Context())
	if res.Skipped || problem(res) {
		t.Fatalf("reachable http endpoint reported: %+v", res.Error)
	}
}

// With no base_url in the config (the normal DeepSeek setup) the check was
// skipped, even though cove does connect to the provider's default URL or
// LLM_BASE_URL.
func TestCheckNetworkUsesTheEffectiveBaseURL(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer srv.Close()
	c, _ := newTestChecker(t, &config.Config{Provider: config.ProviderConfig{Name: "openai-compatible", APIKey: "sk-k"}})
	t.Setenv("LLM_BASE_URL", srv.URL)

	res := c.checkNetworkReachable(t.Context())
	if res.Skipped {
		t.Fatal("network check skipped although cove would connect to LLM_BASE_URL")
	}
	if problem(res) {
		t.Fatalf("reachable endpoint reported: %+v", res.Error)
	}
}

func TestCheckNetworkReportsUnreachableWithoutLeakingQuery(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	url := srv.URL
	srv.Close()
	c, _ := newTestChecker(t, &config.Config{Provider: config.ProviderConfig{Name: "openai-compatible", APIKey: "sk-k", BaseURL: url + "/v1?api-key=topsecret"}})

	res := c.checkNetworkReachable(t.Context())
	if res.Status < SevError || res.Error == nil || res.Error.Def.Code != ErrAPIUnreachable {
		t.Fatalf("got %+v, want %s", res, ErrAPIUnreachable)
	}
	if strings.Contains(res.Error.Format(), "topsecret") {
		t.Fatalf("report shows the URL's query string: %s", res.Error.Format())
	}
}

// On Windows the check looked for PowerShell, but cove runs commands with the
// shell from internal/shell (Git Bash first). It passed on machines where
// cove had fallen back to PowerShell and the model's bash commands failed.
func TestCheckShellWarnsWhenWindowsHasNoGitBash(t *testing.T) {
	c, _ := newTestChecker(t, &config.Config{})
	c.goos = "windows"
	c.shell = func() shell.Shell {
		return shell.Shell{Kind: shell.PowerShell, Path: `C:\Windows\System32\WindowsPowerShell\v1.0\powershell.exe`}
	}
	c.lookPath = func(string) (string, error) { return "found", nil }

	res := c.checkShellAvailable(t.Context())
	if res.Status != SevWarning || res.Error == nil || !strings.Contains(res.Error.Detail, "powershell") {
		t.Fatalf("got %+v, want a warning naming the PowerShell fallback", res)
	}
}

// The WSL launcher (System32\bash.exe) runs commands in a Linux distro that
// may not exist; if cove ever resolves it again, the check must say so.
func TestCheckShellFlagsTheWSLLauncher(t *testing.T) {
	c, _ := newTestChecker(t, &config.Config{})
	c.goos = "windows"
	c.shell = func() shell.Shell { return shell.Shell{Kind: shell.Bash, Path: `C:\Windows\System32\bash.exe`} }
	c.lookPath = func(string) (string, error) { return "found", nil }

	if res := c.checkShellAvailable(t.Context()); res.Status < SevError {
		t.Fatalf("WSL launcher accepted: %+v", res)
	}
}

func TestCheckShellPassesWithGitBash(t *testing.T) {
	c, _ := newTestChecker(t, &config.Config{})
	c.goos = "windows"
	c.shell = func() shell.Shell { return shell.Shell{Kind: shell.Bash, Path: `C:\Program Files\Git\bin\bash.exe`} }
	c.lookPath = func(string) (string, error) { return "found", nil }

	if res := c.checkShellAvailable(t.Context()); problem(res) {
		t.Fatalf("Git Bash reported: %+v", res.Error)
	}
}

func TestCheckShellReportsAMissingShellBinary(t *testing.T) {
	c, _ := newTestChecker(t, &config.Config{})
	c.goos = "linux"
	c.shell = func() shell.Shell { return shell.Shell{Kind: shell.Bash, Path: "/bin/sh"} }
	c.lookPath = func(string) (string, error) { return "", errors.New("not found") }

	if res := c.checkShellAvailable(t.Context()); res.Status < SevError {
		t.Fatalf("missing shell accepted: %+v", res)
	}
}

// Checkpoints and git-aware tools need git; nothing told the user it was
// missing.
func TestCheckGitWarnsWhenGitIsMissing(t *testing.T) {
	c, _ := newTestChecker(t, &config.Config{})
	c.lookPath = func(name string) (string, error) {
		if name == "git" {
			return "", errors.New("not found")
		}
		return name, nil
	}

	res := c.checkGit(t.Context())
	if res.Status != SevWarning || res.Error == nil {
		t.Fatalf("missing git not reported: %+v", res)
	}
}

// A typo in permission_mode silently ran in "default"; the catalogue's hint
// also listed modes that do not exist ("ask").
func TestCheckPermissionModeReportsAnInvalidMode(t *testing.T) {
	c, _ := newTestChecker(t, &config.Config{PermissionMode: "bypas"})

	res := c.checkPermissionMode(t.Context())
	if res.Status != SevWarning || res.Error == nil || res.Error.Def.Code != ErrConfigPermMode {
		t.Fatalf("got %+v, want %s", res, ErrConfigPermMode)
	}
	rec := res.Error.Def.Recovery
	for _, m := range []string{"default", "plan", "auto", "bypass"} {
		if !strings.Contains(rec, m) {
			t.Errorf("recovery %q does not list mode %q", rec, m)
		}
	}
	if strings.Contains(rec, "ask") {
		t.Errorf("recovery %q lists the non-existent mode ask", rec)
	}
}

// A bedrock user with IAM credentials only (SigV4) has a working setup;
// startup and /diagnose reported it as a fatal missing key.
func TestCheckAPIKeyAcceptsBedrockIAMCredentials(t *testing.T) {
	c, _ := newTestChecker(t, &config.Config{Provider: config.ProviderConfig{Name: "bedrock"}})
	dir := t.TempDir()
	for k, v := range map[string]string{
		"AWS_BEARER_TOKEN_BEDROCK": "", "BEDROCK_API_KEY": "", "LLM_API_KEY": "", "AWS_SESSION_TOKEN": "", "AWS_PROFILE": "",
		"AWS_SHARED_CREDENTIALS_FILE": filepath.Join(dir, "credentials"), "AWS_CONFIG_FILE": filepath.Join(dir, "config"),
		"AWS_ACCESS_KEY_ID": "AKIDEXAMPLE", "AWS_SECRET_ACCESS_KEY": "secret",
	} {
		t.Setenv(k, v)
	}
	if res := c.checkAPIKey(t.Context()); problem(res) {
		t.Fatalf("bedrock with IAM credentials reported: %+v", res.Error)
	}
	if res := c.checkConfigExists(t.Context()); problem(res) {
		t.Fatalf("missing config.json with IAM credentials reported: %+v", res.Error)
	}
	t.Setenv("AWS_ACCESS_KEY_ID", "")
	if res := c.checkAPIKey(t.Context()); !problem(res) {
		t.Fatal("bedrock without any credentials must still be reported")
	}
}
