package main

import (
	"bytes"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/liuzhixin405/cove-agent/internal/engine"
	"github.com/liuzhixin405/cove-agent/internal/permission"
	"github.com/liuzhixin405/cove-agent/internal/repl"
	"github.com/liuzhixin405/cove-agent/internal/termui"
)

func TestPermissionAnswerDecision(t *testing.T) {
	cases := []struct {
		in      string
		allow   bool
		always  bool
		persist bool
	}{
		{"y", true, false, false},
		{"Y", true, false, false},
		{"\r\ny\r\n", true, false, false},
		{"yes", true, false, false},
		{"是", true, false, false},
		{"a", true, true, false},
		{"ALWAYS", true, true, false},
		{"p", true, true, true},
		{"P", true, true, true},
		{"permanent", true, true, true},
		{"永久", true, true, true},
		{"n", false, false, false},
		{"no", false, false, false},
		{"", false, false, false},
		{"what?", false, false, false},
	}
	for _, c := range cases {
		allow, always, persist := permissionAnswerDecision(c.in)
		if allow != c.allow || always != c.always || persist != c.persist {
			t.Errorf("permissionAnswerDecision(%q) = (%v,%v,%v), want (%v,%v,%v)", c.in, allow, always, persist, c.allow, c.always, c.persist)
		}
	}
}

// persistingRules records what a "p" answer asked the engine to persist.
// Like *engine.Engine, a successful persist also installs the rules in the
// manager; a separate AddPermissionRule is counted in added.
type persistingRules struct {
	managerRules
	persisted []permission.Rule
	scopes    []string
	scope     string
	calls     int
	added     int
	err       error
}

func (p *persistingRules) AddPermissionRule(d permission.Decision, r permission.Rule) {
	p.added++
	p.managerRules.AddPermissionRule(d, r)
}

func (p *persistingRules) PersistPermissionRules(rs []permission.Rule, scope string) error {
	p.calls++
	if p.err != nil {
		return p.err
	}
	p.persisted = append(p.persisted, rs...)
	p.scopes = append(p.scopes, scope)
	for _, r := range rs {
		p.AddRule(permission.DAllow, r)
	}
	return nil
}

// A successful "p" leaves installing the rule to the persister (which
// registers it as a disk rule dropped on /cd); the prompt must not add a
// second, session-only copy that would outlive /cd.
func TestPermanentAnswerAddsNoSessionCopy(t *testing.T) {
	dir := t.TempDir()
	m := permission.NewManager(permission.Default)
	rules := &persistingRules{managerRules: managerRules{m}, scope: permission.ProjectRoot(dir)}
	if allow, _ := answerPrompt(t, rules, "bash", map[string]any{"command": "pytest ./..."}, "p"); !allow {
		t.Fatal("answer \"p\" must allow the current call")
	}
	if rules.added != 0 {
		t.Fatalf("prompt added %d session rules besides the persisted one", rules.added)
	}
}

// When writing policies.json fails, "p" degrades to a session rule and says so.
func TestPermanentAnswerPersistFailureFallsBackToSessionRule(t *testing.T) {
	m := permission.NewManager(permission.Default)
	rules := &persistingRules{managerRules: managerRules{m}, scope: "/p", err: errors.New("disk full")}
	allow, out := answerPrompt(t, rules, "bash", map[string]any{"command": "pytest ./..."}, "p")
	if !allow {
		t.Fatal("answer \"p\" must allow the current call")
	}
	if rules.added != 1 {
		t.Fatalf("session fallback rules added = %d, want 1", rules.added)
	}
	if d := checkCommand(m, "bash", "pytest ./x"); d != permission.DAllow {
		t.Errorf("session fallback rule missing: %v", d)
	}
	if !strings.Contains(out, "未能写入，仅本次会话有效") || !strings.Contains(out, "disk full") {
		t.Errorf("failure not reported:\n%s", out)
	}
}

func (p *persistingRules) PermissionScope() string { return p.scope }

// Answering "p" allows the call and persists the rule scoped to the current
// project; the persister (the engine) installs it for this session, so the
// rule applies right away without a separate session copy.
func TestPermanentAnswerPersistsRuleForThisProject(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	m := permission.NewManager(permission.Default)
	rules := &persistingRules{managerRules: managerRules{m}, scope: permission.ProjectRoot(dir)}
	allow, out := answerPrompt(t, rules, "bash", map[string]any{"command": "pytest ./..."}, "p")
	if !allow {
		t.Fatal("answer \"p\" must allow the current call")
	}
	if !strings.Contains(out, "[p] 本项目记住") {
		t.Errorf("prompt does not offer [p]:\n%s", out)
	}
	if !strings.Contains(out, "[a] 本会话记住") || !strings.Contains(out, "[n] 拒绝") {
		t.Errorf("prompt lost the [a]/[n] options:\n%s", out)
	}
	if d := checkCommand(m, "bash", "pytest -run X ./pkg"); d != permission.DAllow {
		t.Errorf("session rule missing after \"p\": %v", d)
	}
	if len(rules.persisted) != 1 || rules.persisted[0].CommandPrefix != "pytest" || rules.persisted[0].ToolPattern != "bash" {
		t.Fatalf("persisted = %+v, want one bash/pytest rule", rules.persisted)
	}
	if want := permission.ProjectRoot(dir); !permission.SameProject(rules.scopes[0], want) {
		t.Errorf("scope = %q, want project root %q", rules.scopes[0], want)
	}
}

// The confirmation names the file the rule went to: policies.json in the
// config directory, which COVE_CONFIG_DIR moves away from ~/.cove.
func TestPermanentAnswerNamesTheActualPoliciesFile(t *testing.T) {
	cfgDir := t.TempDir()
	t.Setenv("COVE_CONFIG_DIR", cfgDir)
	dir := t.TempDir()
	t.Chdir(dir)
	rules := &persistingRules{managerRules: managerRules{permission.NewManager(permission.Default)}, scope: permission.ProjectRoot(dir)}
	_, out := answerPrompt(t, rules, "bash", map[string]any{"command": "pytest ./..."}, "p")
	if want := filepath.Join(cfgDir, "policies.json"); !strings.Contains(out, want) {
		t.Fatalf("confirmation does not name %s:\n%s", want, out)
	}
	if strings.Contains(out, "~/.cove/policies.json") {
		t.Fatalf("confirmation still names ~/.cove/policies.json:\n%s", out)
	}
}

// Without a persister (or for an unscopable command) "p" still only allows
// once or for the session; nothing is written.
func TestPermanentAnswerWithoutPersisterFallsBackToSession(t *testing.T) {
	m := permission.NewManager(permission.Default)
	allow, _ := answerPrompt(t, managerRules{m}, "bash", map[string]any{"command": "pytest ./..."}, "p")
	if !allow {
		t.Fatal("answer \"p\" must allow the current call")
	}
	if d := checkCommand(m, "bash", "pytest ./x"); d != permission.DAllow {
		t.Errorf("session rule missing: %v", d)
	}
	rules := &persistingRules{managerRules: managerRules{permission.NewManager(permission.Default)}}
	if allow, _ := answerPrompt(t, rules, "bash", map[string]any{"command": "sudo pytest"}, "p"); !allow {
		t.Fatal("\"p\" on an unscopable command must still allow once")
	}
	if len(rules.persisted) != 0 {
		t.Errorf("persisted a rule for an unscopable command: %+v", rules.persisted)
	}
}

// TestAskToolPermissionRelay covers the plumbing the REPL loop depends on: the
// prompt registers a channel the loop can take, and the answer line it writes
// back becomes the decision returned to the engine. A nil engine is enough here
// because neither the allow nor the timeout path touches it.
func TestAskToolPermissionRelay(t *testing.T) {
	oldInteractive := replInteractive
	replInteractive = true
	t.Cleanup(func() { replInteractive = oldInteractive; repl.ClearPermInputCh() })

	done := make(chan bool, 1)
	go func() {
		done <- askToolPermission(nil, "write", map[string]any{"file_path": "a.go"}, "")
	}()

	answerCh := waitForPermInputCh(t)
	answerCh <- "y"

	select {
	case allow := <-done:
		if !allow {
			t.Fatal("answer \"y\" must allow the tool call")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("askToolPermission did not return after the answer was relayed")
	}
}

func TestAskToolPermissionDeniesWhenNotInteractive(t *testing.T) {
	oldInteractive := replInteractive
	replInteractive = false
	t.Cleanup(func() { replInteractive = oldInteractive })

	if askToolPermission(nil, "bash", map[string]any{"command": "echo hi"}, "") {
		t.Fatal("a non-interactive run must deny gated tools, not block on a prompt")
	}
	if ch := repl.TakePermInputCh(); ch != nil {
		t.Fatal("no input channel should be registered when there is no interactive loop")
	}
}

func TestAskToolPermissionTimesOut(t *testing.T) {
	oldInteractive, oldTimeout := replInteractive, permissionPromptTimeout
	replInteractive = true
	permissionPromptTimeout = 50 * time.Millisecond
	t.Cleanup(func() {
		replInteractive = oldInteractive
		permissionPromptTimeout = oldTimeout
		repl.ClearPermInputCh()
	})

	// Nothing drains the relayed channel, so the prompt stays unanswered and
	// must fall back to a denial instead of blocking forever.
	start := time.Now()
	allow := askToolPermission(nil, "bash", map[string]any{"command": "echo hi"}, "")
	if allow {
		t.Fatal("an unanswered prompt must deny the tool call")
	}
	if time.Since(start) > 10*time.Second {
		t.Fatal("timeout path took too long; the deadline is not being honoured")
	}
	if ch := repl.TakePermInputCh(); ch != nil {
		t.Fatal("the input channel must be unregistered after a timeout")
	}
}

// TestInstallPermissionPromptWiresHandler checks the REPL hookup: the engine
// must end up with a prompt callback, and answering "a" must both allow the
// call and register a session rule without panicking.
func TestInstallPermissionPromptWiresHandler(t *testing.T) {
	oldInteractive := replInteractive
	replInteractive = true
	t.Cleanup(func() { replInteractive = oldInteractive; repl.ClearPermInputCh() })
	// The engine opens its session store under HOME and project notes under
	// the working directory; this test used to create both in the user's
	// real ~/.cove and in the package directory.
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("COVE_CONFIG_DIR", filepath.Join(home, ".cove"))
	t.Chdir(t.TempDir())

	eng, err := engine.New(engine.Config{})
	if err != nil {
		t.Fatalf("engine.New: %v", err)
	}
	installPermissionPrompt(eng)
	if eng.PermissionPrompt == nil {
		t.Fatal("installPermissionPrompt left the engine without a prompt callback")
	}

	done := make(chan bool, 1)
	go func() {
		done <- eng.PermissionPrompt("write", map[string]any{"file_path": "a.go"}, "write a.go")
	}()

	answerCh := waitForPermInputCh(t)
	answerCh <- "a"

	select {
	case allow := <-done:
		if !allow {
			t.Fatal("answer \"a\" (always) must allow the tool call")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("engine prompt did not return after the answer was relayed")
	}
}

// managerRules lets the prompt add session rules straight to a Manager, so a
// test can ask the Manager what the "a" answer actually allows afterwards.
type managerRules struct{ *permission.Manager }

func (m managerRules) AddPermissionRule(d permission.Decision, r permission.Rule) { m.AddRule(d, r) }

func (m managerRules) ExplainPermissionGap(tool string, input map[string]any) string {
	return m.ExplainUncovered(tool, input)
}

// A prompt for a line the remembered rules do not cover says why, so the
// person sees the cause rather than "asked again".
func TestPromptExplainsWhyRememberedRulesDidNotApply(t *testing.T) {
	m := permission.NewManager(permission.Default)
	m.SetShellKind(permission.ShellPOSIX)
	m.AddRule(permission.DAllow, permission.Rule{ToolPattern: "bash", CommandPrefix: "mkdir"})
	_, out := answerPrompt(t, managerRules{m}, "bash", map[string]any{"command": "mkdir a && dotnet new sln"}, "n")
	if !strings.Contains(out, "未覆盖这一行") || !strings.Contains(out, "dotnet new") {
		t.Fatalf("prompt does not explain the gap:\n%s", out)
	}
	fresh := permission.NewManager(permission.Default)
	_, out = answerPrompt(t, managerRules{fresh}, "bash", map[string]any{"command": "rm -rf build"}, "n")
	if strings.Contains(out, "未覆盖") {
		t.Fatalf("nothing remembered, yet an explanation was shown:\n%s", out)
	}
}

// The explanation is information, not a warning: it used to be a yellow line
// of its own under nearly every compound command, repeating "按 y 允许本次，
// n 拒绝" under the answer line that said just that.
func TestPromptExplanationIsQuiet(t *testing.T) {
	m := permission.NewManager(permission.Default)
	m.SetShellKind(permission.ShellPOSIX)
	// A line that cannot be remembered: no [a], and nothing more to say.
	_, out := answerPrompt(t, managerRules{m}, "bash", map[string]any{"command": `sed "s/a/b/" s.json > s2.json`}, "n")
	if strings.Contains(out, "[a]") || strings.Contains(out, "无法") || strings.Contains(out, "按 y 允许本次") {
		t.Fatalf("a line that cannot be remembered is still explained:\n%s", out)
	}
	// A remembered rule that does not cover the line is explained, dim.
	m.AddRule(permission.DAllow, permission.Rule{ToolPattern: "bash", CommandPrefix: "mkdir"})
	_, out = answerPrompt(t, managerRules{m}, "bash", map[string]any{"command": "mkdir a && dotnet new sln"}, "n")
	if !strings.Contains(out, "未覆盖这一行") || strings.Contains(out, termui.Yellow+"已记住") {
		t.Fatalf("gap explanation missing or yellow:\n%s", out)
	}
}

// How to answer is said once per process, not under every prompt.
func TestPromptKeyHintShownOnce(t *testing.T) {
	keyHintShown.Store(false)
	t.Cleanup(func() { keyHintShown.Store(false) })
	m := permission.NewManager(permission.Default)
	_, first := answerPrompt(t, managerRules{m}, "bash", map[string]any{"command": "rm -rf build"}, "n")
	_, second := answerPrompt(t, managerRules{m}, "bash", map[string]any{"command": "rm -rf dist"}, "n")
	if !strings.Contains(first, "按键即答") || strings.Contains(second, "按键即答") {
		t.Fatalf("first has hint=%v, second has hint=%v", strings.Contains(first, "按键即答"), strings.Contains(second, "按键即答"))
	}
}

// answerPrompt runs one prompt for toolName/input, answers it and returns the
// decision together with everything the prompt printed.
func answerPrompt(t *testing.T, rules permissionRuleAdder, toolName string, input map[string]any, answer string) (bool, string) {
	t.Helper()
	oldInteractive := replInteractive
	replInteractive = true
	var buf bytes.Buffer
	termui.SetWriter(&buf)
	t.Cleanup(func() {
		replInteractive = oldInteractive
		termui.SetWriter(nil)
		repl.ClearPermInputCh()
	})

	done := make(chan bool, 1)
	go func() { done <- askToolPermission(rules, toolName, input, "") }()
	waitForPermInputCh(t) <- answer
	select {
	case allow := <-done:
		return allow, buf.String()
	case <-time.After(5 * time.Second):
		t.Fatal("askToolPermission did not return after the answer was relayed")
		return false, ""
	}
}

func checkCommand(m *permission.Manager, tool, cmd string) permission.Decision {
	d, _ := m.Check(tool, map[string]any{"command": cmd}, permission.DAsk)
	return d
}

// Answering "a" for a shell command must remember only that command's prefix:
// the old whole-tool rule let every later bash command, rm -rf included, run
// without a prompt.
func TestAlwaysAnswerScopesShellToolsToCommandPrefix(t *testing.T) {
	m := permission.NewManager(permission.Default)
	allow, out := answerPrompt(t, managerRules{m}, "bash", map[string]any{"command": "pytest ./..."}, "a")
	if !allow {
		t.Fatal("answer \"a\" must allow the current call")
	}
	if !strings.Contains(out, `记住范围: bash 中 "pytest" 开头的命令`) {
		t.Errorf("prompt output does not name the remembered prefix:\n%s", out)
	}
	// The scope is stated once, below the options, not inside them.
	if strings.Contains(out, `[a] 本会话记住 "pytest"`) || strings.Contains(out, `[p] 本项目记住 "pytest"`) {
		t.Errorf("options repeat the scope:\n%s", out)
	}

	if d := checkCommand(m, "bash", "pytest -run TestX ./pkg"); d != permission.DAllow {
		t.Errorf("later pytest = %v, want allow", d)
	}
	for _, cmd := range []string{"rm -rf src", "pytest ./... && rm -rf x", "sudo pytest"} {
		if d := checkCommand(m, "bash", cmd); d != permission.DAsk {
			t.Errorf("%q after allowing pytest = %v, want ask", cmd, d)
		}
	}
}

func TestAlwaysAnswerKeepsWholeToolScopeForOtherTools(t *testing.T) {
	m := permission.NewManager(permission.Default)
	allow, _ := answerPrompt(t, managerRules{m}, "write", map[string]any{"file_path": "a.go"}, "a")
	if !allow {
		t.Fatal("answer \"a\" must allow the current call")
	}
	if d, _ := m.Check("write", map[string]any{"file_path": "b.go"}, permission.DAsk); d != permission.DAllow {
		t.Errorf("write after \"a\" = %v, want allow", d)
	}
}

// A command no prefix can safely describe is allowed once and nothing is
// remembered, and the prompt says so instead of offering a scope it lacks.
func TestAlwaysAnswerForUnscopableCommandAllowsOnlyOnce(t *testing.T) {
	m := permission.NewManager(permission.Default)
	allow, out := answerPrompt(t, managerRules{m}, "bash", map[string]any{"command": "sudo pytest"}, "a")
	if !allow {
		t.Fatal("answer \"a\" must still allow the current call")
	}
	if strings.Contains(out, "记住范围") || strings.Contains(out, "[a]") {
		t.Errorf("prompt offered an always-allow scope for an unscopable command:\n%s", out)
	}
	if d := checkCommand(m, "bash", "sudo pytest"); d != permission.DAsk {
		t.Errorf("sudo pytest after \"a\" = %v, want ask", d)
	}
	if d := checkCommand(m, "bash", "ls"); d != permission.DAsk {
		t.Errorf("ls after \"a\" on sudo pytest = %v, want ask", d)
	}
}

// A routine git write is remembered as the git group, and the read-only
// companions the model chains around it (cd, echo, git log) are not listed:
// the transcript this came from offered `"cd"、"git push"、"echo"、"git
// log"、"git status" 开头的命令` for a line whose only question was the push.
func TestAlwaysAnswerRemembersGitRoutineGroup(t *testing.T) {
	m := permission.NewManager(permission.Default)
	m.SetShellKind(permission.ShellPOSIX)
	cmd := `cd G:/x && git push && echo "=== push done ===" && git log --oneline -2 && git status --short`
	allow, out := answerPrompt(t, managerRules{m}, "bash", map[string]any{"command": cmd}, "a")
	if !allow {
		t.Fatal("answer \"a\" must allow the current call")
	}
	if !strings.Contains(out, "记住范围: bash 中 git 常规操作") {
		t.Errorf("prompt does not offer the git group:\n%s", out)
	}
	for _, noise := range []string{`"cd"`, `"echo"`, `"git log"`, `"git status"`, `"git push"`} {
		if strings.Contains(out, noise) {
			t.Errorf("prompt lists %s although it needs no approval:\n%s", noise, out)
		}
	}
	for _, later := range []string{"git add -A && git commit -m x", "git push --set-upstream origin main", "cd G:/x && git remote -v && git pull"} {
		if d := checkCommand(m, "bash", later); d != permission.DAllow {
			t.Errorf("%q after remembering the git group = %v, want allow", later, d)
		}
	}
	for _, still := range []string{"git push --force", "git reset --hard", "rm -rf x"} {
		if d := checkCommand(m, "bash", still); d != permission.DAsk {
			t.Errorf("%q after remembering the git group = %v, want ask", still, d)
		}
	}
}

// Whole-tool rules say what they cover too.
func TestAlwaysAnswerNamesWholeToolScope(t *testing.T) {
	m := permission.NewManager(permission.Default)
	_, out := answerPrompt(t, managerRules{m}, "write", map[string]any{"file_path": "a.go"}, "a")
	if !strings.Contains(out, "记住范围: 工具 write 的所有调用") {
		t.Errorf("whole-tool scope not stated:\n%s", out)
	}
}

// waitForPermInputCh polls for the channel askToolPermission registers; the call
// runs on another goroutine, so its registration is not synchronous.
func waitForPermInputCh(t *testing.T) chan<- string {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if ch := repl.TakePermInputCh(); ch != nil {
			return ch
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatal("no permission input channel was registered")
	return nil
}

// All prefixes of one answer are persisted in a single call, scoped to the
// engine's project (PermissionScope), not to os.Getwd.
func TestPermanentAnswerPersistsAllPrefixesInOneCall(t *testing.T) {
	m := permission.NewManager(permission.Default)
	rules := &persistingRules{managerRules: managerRules{m}, scope: "/engine/project"}
	if allow, _ := answerPrompt(t, rules, "bash", map[string]any{"command": "pytest ./... | tee out.txt"}, "p"); !allow {
		t.Fatal("answer \"p\" must allow the call")
	}
	if rules.calls != 1 || len(rules.persisted) != 2 {
		t.Fatalf("persist calls = %d, rules = %+v; want one call with 2 rules", rules.calls, rules.persisted)
	}
	if rules.scopes[0] != "/engine/project" {
		t.Errorf("scope = %q, want the engine's PermissionScope", rules.scopes[0])
	}
}

// /cd looks for PolicyLoadError on the engine view it is given; the real
// program hands it a replEngineAdapter.
var _ interface{ PolicyLoadError() error } = replEngineAdapter{}

func TestPermissionDenyReasonParsing(t *testing.T) {
	cases := map[string]string{
		"n":                      "",
		"n 用 switch 别用 checkout": "用 switch 别用 checkout",
		"no  keep the file":      "keep the file",
		"拒绝 先补测试":                "先补测试",
		"y":                      "",
		"e":                      "",
	}
	for in, want := range cases {
		if got := permissionDenyReason(in); got != want {
			t.Errorf("permissionDenyReason(%q) = %q, want %q", in, got, want)
		}
	}
	for _, in := range []string{"n 理由", "拒绝 理由", "e", "说明"} {
		if !permissionAnswerAccepted(in) {
			t.Errorf("%q must be accepted as an answer", in)
		}
	}
	if permissionAnswerAccepted("帮我改成 switch") {
		t.Error("an ordinary sentence must still be the next instruction, not an answer")
	}
}

// "e" asks for a reason on a second line; the reason reaches the engine.
func TestPermissionPromptExplainedDenialCarriesReason(t *testing.T) {
	var buf lockedBuffer
	termui.SetWriter(&buf)
	oldInteractive := replInteractive
	replInteractive = true
	t.Cleanup(func() { replInteractive = oldInteractive; repl.ClearPermInputCh(); termui.SetWriter(nil) })

	done := make(chan engine.PermissionAnswer, 1)
	go func() {
		done <- askToolPermissionAnswer(nil, "bash", map[string]any{"command": "git checkout x"}, "")
	}()
	waitForPermInputCh(t) <- "e"
	waitForOutput(t, &buf, "拒绝理由")
	waitForPermInputCh(t) <- "用 switch\x1b[2J 别用 checkout"
	select {
	case got := <-done:
		if got.Allow || got.DenyReason != "用 switch\x1b[2J 别用 checkout" {
			t.Fatalf("answer = %+v", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("prompt did not return")
	}
	out := buf.String()
	if strings.Contains(out, "\x1b[2J") || !strings.Contains(out, `\e[2J`) {
		t.Errorf("reason echoed raw: %q", out)
	}
	if !strings.Contains(out, "[e] 拒绝并说明") {
		t.Errorf("option missing: %q", out)
	}
}

// Enter on the reason line is a plain denial.
func TestPermissionPromptExplainedDenialEmptyReason(t *testing.T) {
	var buf lockedBuffer
	termui.SetWriter(&buf)
	oldInteractive := replInteractive
	replInteractive = true
	t.Cleanup(func() { replInteractive = oldInteractive; repl.ClearPermInputCh(); termui.SetWriter(nil) })

	done := make(chan engine.PermissionAnswer, 1)
	go func() { done <- askToolPermissionAnswer(nil, "write", map[string]any{"file_path": "a.go"}, "") }()
	waitForPermInputCh(t) <- "e"
	waitForOutput(t, &buf, "拒绝理由")
	waitForPermInputCh(t) <- ""
	select {
	case got := <-done:
		if got.Allow || got.DenyReason != "" {
			t.Fatalf("answer = %+v, want a plain denial", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("prompt did not return")
	}
}

// Ctrl+C at the reason line denies without a reason.
func TestPermissionPromptExplainedDenialInterrupted(t *testing.T) {
	var buf lockedBuffer
	termui.SetWriter(&buf)
	oldInteractive := replInteractive
	replInteractive = true
	t.Cleanup(func() { replInteractive = oldInteractive; repl.ClearPermInputCh(); termui.SetWriter(nil) })

	done := make(chan engine.PermissionAnswer, 1)
	go func() { done <- askToolPermissionAnswer(nil, "write", map[string]any{"file_path": "a.go"}, "") }()
	waitForPermInputCh(t) <- "e"
	waitForOutput(t, &buf, "拒绝理由")
	waitForPermInputCh(t) <- promptInterrupt
	got := <-done
	if got.Allow || got.DenyReason != "" {
		t.Fatalf("answer = %+v", got)
	}
}

// The hint names only the keys that were offered.
func TestPermissionAnswerHintFollowsOptions(t *testing.T) {
	if h := permissionAnswerHintFor(false, false); strings.Contains(h, "本会话记住") || !strings.Contains(h, "e 拒绝并说明") {
		t.Errorf("hint without a/p: %q", h)
	}
	if h := permissionAnswerHintFor(true, true); !strings.Contains(h, "a 本会话记住") || !strings.Contains(h, "p 本项目记住") {
		t.Errorf("hint with a/p: %q", h)
	}
}

func TestPlanExitDescription(t *testing.T) {
	if got := planExitDescription(map[string]any{}); !strings.Contains(got, "模型未提供计划摘要") {
		t.Errorf("empty summary: %q", got)
	}
	got := planExitDescription(map[string]any{"summary": "## 计划\n- 改 A\n- 改 B\x1b[2J"})
	if !strings.Contains(got, "改 A") || !strings.Contains(got, "改 B") || strings.Contains(got, "\x1b[2J") || !strings.Contains(got, `\e[2J`) {
		t.Errorf("summary rendering: %q", got)
	}
	long := strings.Repeat("- 行\n", planSummaryMaxLines+5)
	got = planExitDescription(map[string]any{"summary": long})
	if !strings.Contains(got, "还有 5 行") {
		t.Errorf("truncation marker missing: %q", got)
	}
}

// The plan-exit prompt shows the summary and its own options; "e" asks for
// the revision the model should make.
func TestPlanExitPromptShowsSummaryAndRevisionOption(t *testing.T) {
	var buf lockedBuffer
	termui.SetWriter(&buf)
	oldInteractive := replInteractive
	replInteractive = true
	t.Cleanup(func() { replInteractive = oldInteractive; repl.ClearPermInputCh(); termui.SetWriter(nil) })

	done := make(chan engine.PermissionAnswer, 1)
	go func() {
		done <- askToolPermissionAnswer(nil, "exit_plan_mode", map[string]any{"summary": "1. 先写测试\n2. 再改实现"}, "exiting plan mode requires confirmation")
	}()
	waitForOutput(t, &buf, "再改实现")
	out := buf.String()
	for _, want := range []string{"[y] 执行计划", "[e] 修改计划", "[n] 继续规划"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in %q", want, out)
		}
	}
	for _, unwanted := range []string{"[a]", "[p]", "exiting plan mode requires confirmation"} {
		if strings.Contains(out, unwanted) {
			t.Errorf("unexpected %q in %q", unwanted, out)
		}
	}
	waitForPermInputCh(t) <- "e"
	waitForOutput(t, &buf, "修改意见")
	waitForPermInputCh(t) <- "第 2 步拆成两步"
	got := <-done
	if got.Allow || got.DenyReason != "第 2 步拆成两步" {
		t.Fatalf("answer = %+v", got)
	}
}

// The plan box bounds what the model listed: at most 40 items, each clipped.
func TestPlanExitDescriptionBoundsLists(t *testing.T) {
	files := make([]any, 0, 50)
	for i := 0; i < 50; i++ {
		files = append(files, strings.Repeat("f", 300)+".go")
	}
	got := planExitDescription(map[string]any{"summary": "s", "files": files})
	if n := strings.Count(got, "\n  • "); n != 40 {
		t.Fatalf("items shown = %d, want 40", n)
	}
	if strings.Contains(got, strings.Repeat("f", 300)) {
		t.Fatal("long items must be clipped")
	}
	if !strings.Contains(got, "还有 10 项") {
		t.Fatalf("omitted count missing: %q", got[len(got)-200:])
	}
}

// "e" is a typed answer, not a single key: with a prompt waiting, a line
// starting with e ("edit the test first") must not become a denial.
func TestExplainIsNotASingleKey(t *testing.T) {
	var buf lockedBuffer
	termui.SetWriter(&buf)
	oldInteractive := replInteractive
	replInteractive = true
	t.Cleanup(func() { replInteractive = oldInteractive; repl.ClearPermInputCh(); termui.SetWriter(nil) })
	done := make(chan engine.PermissionAnswer, 1)
	go func() { done <- askToolPermissionAnswer(nil, "bash", map[string]any{"command": "ls"}, "") }()
	waitForOutput(t, &buf, "需要授权")
	if repl.PromptKeyAnswers('e') {
		t.Fatal("e must not answer on its own")
	}
	waitForPermInputCh(t) <- "n"
	<-done
}
