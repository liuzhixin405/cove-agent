package diagnostic

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/liuzhixin405/cove-agent/internal/api"
	"github.com/liuzhixin405/cove-agent/internal/config"
	"github.com/liuzhixin405/cove-agent/internal/permission"
	"github.com/liuzhixin405/cove-agent/internal/session"
	"github.com/liuzhixin405/cove-agent/internal/shell"
)

// CheckResult represents one diagnostic check's outcome.
type CheckResult struct {
	Name    string     // Check name (short, English identifier)
	Title   string     // User-facing title (Chinese)
	Status  Severity   // SevInfo=pass, SevWarning/SevError/SevFatal=problem found
	Error   *DiagError // Structured error if problem found
	Skipped bool       // Check was skipped (e.g. not applicable on this OS)
	Detail  string     // Informational lines shown under the title (may be multi-line)
}

// Report aggregates all diagnostic results.
type Report struct {
	Results   []CheckResult
	AutoFixed int
	Duration  time.Duration
}

// HasProblems returns true if any check found issues.
func (r *Report) HasProblems() bool {
	for _, res := range r.Results {
		if res.Status >= SevWarning && (res.Error == nil || !res.Error.Fixed) {
			return true
		}
	}
	return false
}

// Summary returns a one-line status string.
func (r *Report) Summary() string {
	total := len(r.Results)
	passed := 0
	warnings := 0
	errors := 0
	fixed := 0
	for _, res := range r.Results {
		if res.Skipped {
			continue
		}
		switch {
		case res.Error != nil && res.Error.Fixed:
			fixed++
		case res.Status <= SevInfo:
			passed++
		case res.Status == SevWarning:
			warnings++
		default:
			errors++
		}
	}
	parts := []string{fmt.Sprintf("共 %d 项检查", total)}
	if passed > 0 {
		parts = append(parts, fmt.Sprintf("\x1b[32m%d 通过\x1b[0m", passed))
	}
	if fixed > 0 {
		parts = append(parts, fmt.Sprintf("\x1b[32m%d 已修复\x1b[0m", fixed))
	}
	if warnings > 0 {
		parts = append(parts, fmt.Sprintf("\x1b[33m%d 警告\x1b[0m", warnings))
	}
	if errors > 0 {
		parts = append(parts, fmt.Sprintf("\x1b[31m%d 错误\x1b[0m", errors))
	}
	return strings.Join(parts, ", ")
}

// Format returns the full diagnostic report as a terminal-ready string.
func (r *Report) Format() string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "\x1b[1m🔍 系统诊断报告\x1b[0m (耗时 %s)\n", r.Duration.Round(time.Millisecond))
	fmt.Fprintf(&sb, "   %s\n\n", r.Summary())

	for _, res := range r.Results {
		formatResult(&sb, res)
	}

	if r.AutoFixed > 0 {
		sb.WriteString("\n\x1b[32m✓ 所有修复已立即生效，无需重启\x1b[0m\n")
	}
	return sb.String()
}

// formatResult writes one check's line (icon and title), its detail lines and
// its error, if any; skipped checks write nothing.
func formatResult(sb *strings.Builder, res CheckResult) {
	if res.Skipped {
		return
	}
	icon := "\x1b[32m✓\x1b[0m"
	if res.Error != nil && res.Error.Fixed {
		icon = "\x1b[32m🔧\x1b[0m"
	} else if res.Status == SevWarning {
		icon = "\x1b[33m⚠\x1b[0m"
	} else if res.Status >= SevError {
		icon = "\x1b[31m✗\x1b[0m"
	}
	fmt.Fprintf(sb, " %s %s", icon, res.Title)
	if res.Detail != "" {
		for _, line := range strings.Split(res.Detail, "\n") {
			fmt.Fprintf(sb, "\n   \x1b[2m%s\x1b[0m", line)
		}
	}
	if res.Error != nil {
		sb.WriteString("\n")
		// Indent error details
		lines := strings.Split(res.Error.Format(), "\n")
		for _, line := range lines {
			fmt.Fprintf(sb, "   %s\n", line)
		}
	} else {
		sb.WriteString("\n")
	}
}

// Checker runs diagnostic checks against the current environment.
type Checker struct {
	cfg       *config.Config
	homeDir   string
	configDir string // where config.json lives; honors COVE_CONFIG_DIR
	cwd       string // where a project .cove.json would be

	// Host probes, replaceable in tests.
	goos     string
	shell    func() shell.Shell
	lookPath func(string) (string, error)

	// Engine state, replaceable in tests; nil falls back to the package-level
	// the running session (SetSession), then to what is on disk.
	background func() BackgroundStatus
	policyErr  func() error
	// writeFile is the disk-space probe's write; nil means os.WriteFile.
	writeFile func(name string, data []byte, perm os.FileMode) error
}

// NewChecker creates a new diagnostic checker.
func NewChecker(cfg *config.Config) *Checker {
	home, _ := os.UserHomeDir()
	cfgDir, err := config.ConfigDir()
	if err != nil {
		cfgDir = filepath.Join(home, ".cove")
	}
	cwd, _ := os.Getwd()
	return &Checker{
		cfg:       cfg,
		homeDir:   home,
		configDir: cfgDir,
		cwd:       cwd,
		goos:      runtime.GOOS,
		shell:     shell.Default,
		lookPath:  exec.LookPath,
	}
}

// RunAll executes all diagnostic checks and returns a report.
func (c *Checker) RunAll(ctx context.Context) *Report {
	start := time.Now()
	report := &Report{}

	checks := []func(context.Context) CheckResult{
		c.checkConfigExists,
		c.checkConfigValid,
		c.checkAPIKey,
		c.checkModelValid,
		c.checkPermissionMode,
		c.checkNetworkReachable,
		c.checkShellAvailable,
		c.checkGit,
		c.checkDataDir,
		c.checkDiskSpace,
		c.checkSessionIntegrity,
		c.checkPolicyFile,
		c.checkBackgroundLearning,
	}

	for _, check := range checks {
		result := check(ctx)
		report.Results = append(report.Results, result)
		if result.Error != nil && result.Error.Fixed {
			report.AutoFixed++
		}
	}

	report.Duration = time.Since(start)
	return report
}

// RunQuick runs only fast checks (no network) suitable for startup.
func (c *Checker) RunQuick() *Report {
	start := time.Now()
	report := &Report{}

	checks := []func(context.Context) CheckResult{
		c.checkConfigExists,
		c.checkConfigValid,
		c.checkAPIKey,
		c.checkModelValid,
		c.checkPermissionMode,
		c.checkShellAvailable,
		c.checkDataDir,
	}

	ctx := context.Background()
	for _, check := range checks {
		result := check(ctx)
		report.Results = append(report.Results, result)
		if result.Error != nil && result.Error.Fixed {
			report.AutoFixed++
		}
	}

	report.Duration = time.Since(start)
	return report
}

func (c *Checker) checkConfigExists(_ context.Context) CheckResult {
	res := CheckResult{Name: "config_exists", Title: "配置文件"}
	cfgPath := filepath.Join(c.configDir, "config.json")

	// A missing config.json used to be "auto-fixed" with a template holding the
	// API key "sk-xxxxxxxx…" (and telemetry on). This runs from QuickCheck on
	// every start, and a key in the file beats the environment, so a user who
	// keeps the key in DEEPSEEK_API_KEY got 401s from the second launch on.
	// cove runs fine without the file; it is only worth a warning when no key
	// is available either, and nothing is written.
	if _, err := os.Stat(cfgPath); os.IsNotExist(err) {
		if c.cfg != nil && c.cfg.EffectiveProvider().APIKey != "" {
			res.Status = SevInfo
			return res
		}
		res.Status = SevWarning
		res.Error = New(ErrConfigMissing, cfgPath)
	} else {
		res.Status = SevInfo
	}
	return res
}

func (c *Checker) checkConfigValid(_ context.Context) CheckResult {
	res := CheckResult{Name: "config_valid", Title: "配置格式"}
	if c.cfg == nil {
		res.Status = SevFatal
		res.Error = New(ErrConfigInvalid, "config is nil")
		return res
	}
	// This only checked that a config was in memory, so a config.json with a
	// syntax error — whose settings Load had just thrown away — passed.
	for _, p := range []string{filepath.Join(c.configDir, "config.json"), filepath.Join(c.cwd, ".cove.json")} {
		if err := config.CheckFile(p); err != nil {
			res.Status = SevError
			res.Error = New(ErrConfigInvalid, err.Error())
			return res
		}
	}
	res.Status = SevInfo
	return res
}

// placeholderKey matches "sk-" followed only by x's: the key the old
// missing-config auto-fix wrote, which some configs still carry.
func placeholderKey(key string) bool {
	rest, ok := strings.CutPrefix(strings.ToLower(key), "sk-")
	return ok && rest != "" && strings.Trim(rest, "x") == ""
}

func (c *Checker) checkAPIKey(_ context.Context) CheckResult {
	res := CheckResult{Name: "api_key", Title: "API Key"}
	if c.cfg == nil {
		res.Skipped = true
		return res
	}

	// Resolve the key exactly as the client will. This used to test four
	// hard-coded variables: GLM_API_KEY, KIMI_API_KEY ... were "missing", a
	// DEEPSEEK_API_KEY counted for the openai provider, and an empty provider
	// name (which runs as anthropic) was reported as fatal.
	pc := c.cfg.EffectiveProvider()
	switch {
	case placeholderKey(pc.APIKey):
		res.Status = SevFatal
		res.Error = New(ErrConfigAPIKeyPlaceholder, pc.APIKey)
	case pc.APIKey == "":
		res.Status = SevFatal
		res.Error = New(ErrConfigAPIKeyMissing, pc.Name)
	default:
		res.Status = SevInfo
	}
	return res
}

func (c *Checker) checkPermissionMode(_ context.Context) CheckResult {
	res := CheckResult{Name: "permission_mode", Title: "权限模式"}
	if c.cfg == nil {
		res.Skipped = true
		return res
	}
	// Startup ignores a mode it does not know and runs in "default", so a
	// typo in permission_mode had no visible effect at all.
	if m := c.cfg.PermissionMode; m != "" && !permission.ValidMode(permission.Mode(m)) {
		res.Status = SevWarning
		res.Error = New(ErrConfigPermMode, m)
		return res
	}
	res.Status = SevInfo
	return res
}

func (c *Checker) checkModelValid(_ context.Context) CheckResult {
	res := CheckResult{Name: "model_valid", Title: "模型配置"}
	if c.cfg == nil {
		res.Skipped = true
		return res
	}

	model := c.cfg.Model
	if model == "" || strings.EqualFold(model, "auto") {
		// Auto-resolved at runtime, always valid
		res.Status = SevInfo
		return res
	}

	// Basic sanity: model name should not contain spaces or special chars
	if strings.ContainsAny(model, " \t\n{}[]") {
		res.Status = SevError
		res.Error = New(ErrConfigModelInvalid, model)
		return res
	}

	res.Status = SevInfo
	return res
}

func (c *Checker) checkNetworkReachable(ctx context.Context) CheckResult {
	res := CheckResult{Name: "network", Title: "API 网络连通"}
	if c.cfg == nil {
		res.Skipped = true
		return res
	}
	// Probe the URL the client will actually use. This read only
	// provider.base_url, so the usual setup (no base_url, provider default or
	// LLM_BASE_URL) skipped the check. It also dialed TCP and then forced a TLS
	// handshake, which reported every http:// endpoint as "TLS失败" and went
	// around HTTPS_PROXY. An HTTP request covers DNS, proxy, TCP and TLS, and
	// any status code at all proves the server is reachable. No credentials
	// are sent.
	pc := c.cfg.EffectiveProvider()
	baseURL := pc.BaseURL
	if baseURL == "" {
		baseURL = api.DefaultBaseURL(pc.Name)
	}
	u, err := url.Parse(baseURL)
	if err != nil || u.Host == "" {
		res.Status = SevError
		res.Error = New(ErrAPIUnreachable, fmt.Sprintf("%s (base_url 无效)", displayURL(baseURL)))
		return res
	}

	reqCtx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, u.String(), nil)
	if err == nil {
		var resp *http.Response
		transport := &http.Transport{Proxy: http.ProxyFromEnvironment, DisableKeepAlives: true}
		client := &http.Client{Transport: transport}
		if resp, err = client.Do(req); err == nil {
			_ = resp.Body.Close()
		}
		// A throw-away transport keeps its idle connection (and two
		// goroutines) until the server closes it; each /diagnose left one.
		transport.CloseIdleConnections()
	}
	if err != nil {
		// *url.Error's text repeats the full URL, query string included.
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		res.Status = SevError
		res.Error = New(ErrAPIUnreachable, fmt.Sprintf("%s (%v)", displayURL(baseURL), err))
		return res
	}

	res.Status = SevInfo
	return res
}

// displayURL is scheme://host/path: base URLs sometimes carry a key in the
// query string or userinfo, and the report is shown on screen.
func displayURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return "<无法解析的地址>"
	}
	return u.Scheme + "://" + u.Host + u.Path
}

func (c *Checker) checkShellAvailable(_ context.Context) CheckResult {
	res := CheckResult{Name: "shell", Title: "Shell 可用性"}

	// Check the shell cove really runs commands with. On Windows this used to
	// look for PowerShell, while the bash tool uses Git Bash first — so the
	// check passed on machines where cove had fallen back to PowerShell (and
	// the model's bash syntax failed), and it could never have noticed the
	// WSL launcher being picked as "bash".
	sh := c.shell()
	if _, err := c.lookPath(sh.Path); err != nil {
		res.Status = SevError
		res.Error = New(ErrToolShellMiss, sh.Path)
		return res
	}
	if c.goos == "windows" {
		if sh.Kind == shell.Bash && isWSLLauncher(sh.Path) {
			res.Status = SevError
			res.Error = New(ErrToolShellWSL, sh.Path)
			return res
		}
		if sh.Kind != shell.Bash {
			res.Status = SevWarning
			res.Error = New(ErrToolNoGitBash, sh.Describe())
			return res
		}
	}

	res.Status = SevInfo
	return res
}

// isWSLLauncher mirrors internal/shell's guard: System32\bash.exe and the
// WindowsApps alias hand commands to a WSL distro, not the Windows toolchain.
func isWSLLauncher(path string) bool {
	lower := strings.ToLower(strings.ReplaceAll(path, "/", `\`))
	return strings.HasSuffix(lower, `\system32\bash.exe`) ||
		strings.HasSuffix(lower, `\sysnative\bash.exe`) ||
		strings.Contains(lower, `\windowsapps\`)
}

func (c *Checker) checkGit(_ context.Context) CheckResult {
	res := CheckResult{Name: "git", Title: "Git"}
	// Checkpoints (/rewind), worktrees and the file list rely on git.
	if _, err := c.lookPath("git"); err != nil {
		res.Status = SevWarning
		res.Error = New(ErrToolGitMissing)
		return res
	}
	res.Status = SevInfo
	return res
}

func (c *Checker) checkDataDir(_ context.Context) CheckResult {
	res := CheckResult{Name: "data_dir", Title: "数据目录"}
	if c.homeDir == "" {
		// No home directory (HOME/USERPROFILE unset): the data directory
		// cannot be located; probing "./.cove" would create it inside the
		// user's project.
		res.Status, res.Skipped = SevInfo, true
		return res
	}
	dataDir := filepath.Join(c.homeDir, ".cove")

	if _, err := os.Stat(dataDir); os.IsNotExist(err) {
		// Try to create
		if err := os.MkdirAll(dataDir, 0750); err != nil {
			res.Status = SevError
			res.Error = New(ErrFSPermission, dataDir)
			return res
		}
		res.Status = SevInfo
		return res
	}

	// Test write access
	testFile := filepath.Join(dataDir, ".diag_test")
	if err := os.WriteFile(testFile, []byte("test"), 0640); err != nil {
		res.Status = SevError
		res.Error = New(ErrFSPermission, dataDir)
		return res
	}
	_ = os.Remove(testFile)

	res.Status = SevInfo
	return res
}

func (c *Checker) checkDiskSpace(_ context.Context) CheckResult {
	res := CheckResult{Name: "disk_space", Title: "磁盘空间"}
	if c.homeDir == "" {
		res.Status, res.Skipped = SevInfo, true
		return res
	}

	// Use a simple write test with a small file — cross-platform
	dataDir := filepath.Join(c.homeDir, ".cove")
	testFile := filepath.Join(dataDir, ".space_test")

	// Try writing a small test file
	testData := make([]byte, 4096)
	write := c.writeFile
	if write == nil {
		write = os.WriteFile
	}
	if err := write(testFile, testData, 0640); err != nil {
		// Only a full disk is a disk-space problem. Every failed write used
		// to be reported as a fatal "磁盘空间不足", so a ~/.cove the user
		// cannot write to — which checkDataDir already reports, with the
		// right remedy — came out a second time as a wrong fatal telling
		// them to free disk space.
		if !isDiskFull(err) {
			res.Skipped = true
			res.Detail = "无法写入测试文件，未检测磁盘空间（见“数据目录”检查）: " + err.Error()
			return res
		}
		res.Status = SevFatal
		res.Error = New(ErrFSDiskFull, "已耗尽（"+err.Error()+"）")
		return res
	}
	_ = os.Remove(testFile)

	res.Status = SevInfo
	return res
}

// isDiskFull reports whether err says the disk (or the user's quota) is
// full: ENOSPC/EDQUOT, and on Windows ERROR_DISK_FULL and
// ERROR_HANDLE_DISK_FULL, which the syscall package does not map to ENOSPC.
func isDiskFull(err error) bool {
	if errors.Is(err, syscall.ENOSPC) || errors.Is(err, syscall.EDQUOT) {
		return true
	}
	var errno syscall.Errno
	if runtime.GOOS == "windows" && errors.As(err, &errno) {
		return errno == 112 || errno == 39 // ERROR_DISK_FULL, ERROR_HANDLE_DISK_FULL
	}
	return false
}

func (c *Checker) checkSessionIntegrity(_ context.Context) CheckResult {
	res := CheckResult{Name: "sessions", Title: "会话完整性"}
	if c.homeDir == "" {
		res.Status, res.Skipped = SevInfo, true
		return res
	}
	sessDir := filepath.Join(c.homeDir, ".cove", "sessions")

	if _, err := os.Stat(sessDir); os.IsNotExist(err) {
		// No sessions yet, that's fine
		res.Status = SevInfo
		return res
	}

	entries, err := os.ReadDir(sessDir)
	if err != nil {
		res.Status = SevWarning
		res.Error = New(ErrSessionCorrupt, "无法读取会话目录")
		return res
	}

	corrupt := 0
	for _, entry := range entries {
		// Only session files (<id>.jsonl, legacy <id>.json): index.json is
		// the list index and may legitimately be tiny.
		if entry.IsDir() || !session.IsSessionFile(entry.Name()) {
			continue
		}
		path := filepath.Join(sessDir, entry.Name())
		info, err := entry.Info()
		if err != nil || info.Size() == 0 {
			corrupt++
			// Auto-fix: remove empty/corrupt session files
			_ = os.Remove(path)
		}
	}

	if corrupt > 0 {
		res.Status = SevRecovered
		res.Error = NewFixed(ErrSessionCorrupt, fmt.Sprintf("%d 个损坏会话已清理", corrupt))
	} else {
		res.Status = SevInfo
	}
	return res
}

// QuickCheck performs a minimal startup check and returns errors that
// should be shown to the user immediately. Returns nil if everything is fine.
func QuickCheck(cfg *config.Config) []*DiagError {
	c := NewChecker(cfg)
	report := c.RunQuick()

	var issues []*DiagError
	for _, r := range report.Results {
		if r.Error != nil && r.Status >= SevError {
			issues = append(issues, r.Error)
		}
	}
	return issues
}
