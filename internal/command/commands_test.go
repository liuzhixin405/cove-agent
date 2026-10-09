package command

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/liuzhixin405/cove-agent/internal/api"
	"github.com/liuzhixin405/cove-agent/internal/config"
	ctxt "github.com/liuzhixin405/cove-agent/internal/context"
	"github.com/liuzhixin405/cove-agent/internal/mcp"
	"github.com/liuzhixin405/cove-agent/internal/permission"
	"github.com/liuzhixin405/cove-agent/internal/plugin"
	"github.com/liuzhixin405/cove-agent/internal/session"
	"github.com/liuzhixin405/cove-agent/internal/skills"
)

type fakeCostTracker struct{ summary string }

func (f fakeCostTracker) Summary() string { return f.summary }

type fakeEngine struct {
	msgs     []api.Message
	loaded   []api.Message
	override string
	cost     fakeCostTracker
	checkpts []string
	restored string
	restErr  error
	backup   string
	rlInfo   api.RateLimitInfo
}

func (f *fakeEngine) Messages() []api.Message         { return f.msgs }
func (f *fakeEngine) MessageCount() int               { return len(f.msgs) }
func (f *fakeEngine) LoadMessages(msgs []api.Message) { f.loaded = msgs; f.msgs = msgs }
func (f *fakeEngine) SetSystemOverride(prompt string) { f.override = prompt }
func (f *fakeEngine) SystemPrompt() string            { return f.override }
func (f *fakeEngine) CostTracker() CostTrackerView    { return f.cost }
func (f *fakeEngine) ListCheckpoints() []string       { return f.checkpts }
func (f *fakeEngine) RestoreCheckpoint(hash string) (string, error) {
	f.restored = hash
	return f.backup, f.restErr
}
func (f *fakeEngine) RateLimitInfo() api.RateLimitInfo { return f.rlInfo }

type fakePluginManager struct {
	plugins    []plugin.Entry
	installs   []string
	enables    []string
	disables   []string
	uninstalls []string
	refreshed  int
}

func (f *fakePluginManager) Install(name, url string) error {
	f.installs = append(f.installs, name+"|"+url)
	return nil
}
func (f *fakePluginManager) Uninstall(name string) error {
	f.uninstalls = append(f.uninstalls, name)
	return nil
}
func (f *fakePluginManager) Disable(name string) error {
	f.disables = append(f.disables, name)
	return nil
}
func (f *fakePluginManager) Enable(name string) error {
	f.enables = append(f.enables, name)
	return nil
}
func (f *fakePluginManager) AllPlugins() []plugin.Entry { return f.plugins }
func (f *fakePluginManager) Refresh()                   { f.refreshed++ }
func (f *fakePluginManager) Dir() string                { return "/tmp/.cove/plugins" }

type fakeSkillManager struct {
	all   []skills.Skill
	index map[string]skills.Skill
}

func (f *fakeSkillManager) All() []skills.Skill { return f.all }
func (f *fakeSkillManager) Get(name string) (skills.Skill, bool) {
	s, ok := f.index[name]
	return s, ok
}

type fakeSessionStore struct {
	records map[string]session.Record
}

func (f *fakeSessionStore) List() ([]session.Record, error) {
	out := make([]session.Record, 0, len(f.records))
	for _, r := range f.records {
		out = append(out, r)
	}
	return out, nil
}
func (f *fakeSessionStore) Load(id string) (*session.Record, error) {
	r := f.records[id]
	return &r, nil
}

type fakeMCPPool struct {
	servers      []*mcp.ManagedServer
	readContents map[string]*mcp.ReadResourceResult
}

func (f *fakeMCPPool) Connect(ctx context.Context, name string, cfg mcp.ServerConfig) error {
	return nil
}
func (f *fakeMCPPool) Disconnect(name string)           {}
func (f *fakeMCPPool) DisconnectAll()                   {}
func (f *fakeMCPPool) AllServers() []*mcp.ManagedServer { return f.servers }
func (f *fakeMCPPool) AllTools() []mcp.ToolRef          { return nil }
func (f *fakeMCPPool) AllResources() []mcp.ResourceRef  { return nil }
func (f *fakeMCPPool) ReadResource(ctx context.Context, serverName, uri string) (*mcp.ReadResourceResult, error) {
	if res, ok := f.readContents[serverName+"|"+uri]; ok {
		return res, nil
	}
	return nil, os.ErrNotExist
}

func TestConfigCmdUpdatesModelAndSaves(t *testing.T) {
	cfg := config.DefaultConfig()
	saved := 0
	cmd := NewConfigCmd()

	out, err := cmd.Execute(context.Background(), Input{
		Args:   []string{"model", "gpt-4o"},
		Config: cfg,
		SaveConfig: func(c *config.Config) error {
			saved++
			return nil
		},
	})
	if err != nil {
		t.Fatalf("Execute error: %v", err)
	}
	if cfg.Model != "gpt-4o" {
		t.Fatalf("expected model updated, got %q", cfg.Model)
	}
	if saved != 1 {
		t.Fatalf("expected save called once, got %d", saved)
	}
	if !strings.Contains(out.Message, "gpt-4o") {
		t.Fatalf("expected output mention new model, got %q", out.Message)
	}
}

func TestExportCmdWritesConversationMarkdown(t *testing.T) {
	tmp := t.TempDir()
	file := filepath.Join(tmp, "conversation.md")
	eng := &fakeEngine{msgs: []api.Message{{Role: "user", Content: "hi"}, {Role: "assistant", Content: "hello"}}}
	cmd := NewExportCmd()

	out, err := cmd.Execute(context.Background(), Input{Args: []string{file}, Cwd: tmp, Engine: eng})
	if err != nil {
		t.Fatalf("Execute error: %v", err)
	}
	if !strings.Contains(out.Message, file) {
		t.Fatalf("expected output mention file, got %q", out.Message)
	}
	data, err := os.ReadFile(file)
	if err != nil {
		t.Fatalf("read exported file: %v", err)
	}
	text := string(data)
	if !strings.Contains(text, "# 对话导出") || !strings.Contains(text, "**assistant**: hello") {
		t.Fatalf("unexpected export content: %s", text)
	}
}

func TestPluginCmdListAndInstall(t *testing.T) {
	pm := &fakePluginManager{plugins: []plugin.Entry{{Manifest: plugin.Manifest{Name: "demo", Version: "1.0.0", Description: "Demo plugin"}, State: plugin.Enabled}}}
	cmd := NewPluginCmd()

	out, err := cmd.Execute(context.Background(), Input{Args: []string{"list"}, PluginManager: pm})
	if err != nil {
		t.Fatalf("list error: %v", err)
	}
	if !strings.Contains(out.Message, "demo") || !strings.Contains(out.Message, "已启用") {
		t.Fatalf("expected listed plugin, got %q", out.Message)
	}
	if pm.refreshed != 1 {
		t.Fatalf("expected plugin list to refresh manager, got %d refreshes", pm.refreshed)
	}

	_, err = cmd.Execute(context.Background(), Input{Args: []string{"install", "alpha", "https://example.com/plugin"}, PluginManager: pm})
	if err != nil {
		t.Fatalf("install error: %v", err)
	}
	if len(pm.installs) != 1 || pm.installs[0] != "alpha|https://example.com/plugin" {
		t.Fatalf("unexpected installs: %#v", pm.installs)
	}
}

func TestPluginCmdListShowsPluginDirWhenEmpty(t *testing.T) {
	pm := &fakePluginManager{}
	cmd := NewPluginCmd()

	out, err := cmd.Execute(context.Background(), Input{Args: []string{"list"}, PluginManager: pm})
	if err != nil {
		t.Fatalf("list error: %v", err)
	}
	if !strings.Contains(out.Message, "暂无已安装插件") || !strings.Contains(out.Message, "/tmp/.cove/plugins") || !strings.Contains(out.Message, "/plugin install") {
		t.Fatalf("expected diagnostic empty plugin list, got %q", out.Message)
	}
}

// The list says where each skill comes from, so a user can tell a built-in
// from their own copy or a project skill that overrides it.
func TestSkillsCmdListShowsSource(t *testing.T) {
	sm := &fakeSkillManager{all: []skills.Skill{
		{Name: "plan", Description: "Plan", Source: skills.SourceBuiltin},
		{Name: "deploy", Description: "Deploy", Source: skills.SourceProject},
		{Name: "notes", Description: "Notes", Source: skills.SourceUser},
	}}
	out, err := NewSkillsCmd().Execute(context.Background(), Input{Args: []string{"list"}, SkillManager: sm})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"[内置]", "[项目]", "[用户]"} {
		if !strings.Contains(out.Message, want) {
			t.Errorf("list output lacks %s:\n%s", want, out.Message)
		}
	}
}

func TestSkillsCmdExportCopiesABuiltin(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	sm := &fakeSkillManager{}
	out, err := NewSkillsCmd().Execute(context.Background(), Input{Args: []string{"export", "plan"}, SkillManager: sm})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(home, ".cove", "skills", "plan", "SKILL.md")
	if _, statErr := os.Stat(path); statErr != nil {
		t.Fatalf("export did not write %s: %s", path, out.Message)
	}
	if !strings.Contains(out.Message, "不再随") {
		t.Fatalf("export must warn that the copy stops updating with the binary: %q", out.Message)
	}
}

func TestSkillsCmdListAndShowSkill(t *testing.T) {
	skill := skills.Skill{Name: "debug", Description: "Debug workflow", Prompt: "DEBUG WORKFLOW"}
	sm := &fakeSkillManager{all: []skills.Skill{skill}, index: map[string]skills.Skill{"debug": skill}}
	cmd := NewSkillsCmd()

	out, err := cmd.Execute(context.Background(), Input{Args: []string{"list"}, SkillManager: sm})
	if err != nil {
		t.Fatalf("list error: %v", err)
	}
	if !strings.Contains(out.Message, "debug") || !strings.Contains(out.Message, "Debug workflow") {
		t.Fatalf("unexpected list output: %q", out.Message)
	}

	out, err = cmd.Execute(context.Background(), Input{Args: []string{"debug"}, SkillManager: sm})
	if err != nil {
		t.Fatalf("show error: %v", err)
	}
	if !strings.Contains(out.Message, "DEBUG WORKFLOW") {
		t.Fatalf("expected prompt body, got %q", out.Message)
	}
}

func TestResumeCmdLoadsSessionMessages(t *testing.T) {
	store := &fakeSessionStore{records: map[string]session.Record{
		"abc": {ID: "abc", Title: "test", Messages: []api.Message{{Role: "user", Content: "resume me"}}, TokensIn: 10, TokensOut: 5},
	}}
	eng := &fakeEngine{}
	cmd := NewResumeCmd()

	out, err := cmd.Execute(context.Background(), Input{Args: []string{"abc"}, SessionStore: store, Engine: eng})
	if err != nil {
		t.Fatalf("resume error: %v", err)
	}
	if len(eng.loaded) != 1 || eng.loaded[0].Content != "resume me" {
		t.Fatalf("messages not loaded: %#v", eng.loaded)
	}
	if !strings.Contains(out.Message, "已恢复") {
		t.Fatalf("unexpected output: %q", out.Message)
	}
}

// statusEngine is a fakeEngine that also reports the live session /status
// reads (StatusSource).
type statusEngine struct {
	fakeEngine
	model, id string
	mode      permission.Mode
}

func (e *statusEngine) Model() string                   { return e.model }
func (e *statusEngine) PermissionMode() permission.Mode { return e.mode }
func (e *statusEngine) SessionID() string               { return e.id }

func TestStatusCmdUsesLiveEngineState(t *testing.T) {
	eng := &statusEngine{fakeEngine: fakeEngine{
		msgs: []api.Message{{Role: "user", Content: "hi"}, {Role: "assistant", Content: "hello"}},
		cost: fakeCostTracker{summary: "10 in | 5 out | $1.23 / $5.00"},
	}, model: "gpt-4o", mode: "auto", id: "session-123"}
	cmd := NewStatusCmd()

	out, err := cmd.Execute(context.Background(), Input{
		Cwd:    "/tmp/project",
		Engine: eng,
	})
	if err != nil {
		t.Fatalf("status error: %v", err)
	}
	if !strings.Contains(out.Message, "会话: session-123") {
		t.Fatalf("expected session id in output, got %q", out.Message)
	}
	if !strings.Contains(out.Message, "模型: gpt-4o") {
		t.Fatalf("expected live model in output, got %q", out.Message)
	}
	if !strings.Contains(out.Message, "消息数: 2") {
		t.Fatalf("expected live engine message count, got %q", out.Message)
	}
	if !strings.Contains(out.Message, "费用: 10 in | 5 out | $1.23 / $5.00") {
		t.Fatalf("expected live cost in output, got %q", out.Message)
	}
	if !strings.Contains(out.Message, "模式: auto") {
		t.Fatalf("expected permission mode in output, got %q", out.Message)
	}
}

func TestMcpCmdReadResource(t *testing.T) {
	cmd := NewMcpCmd()
	pool := &fakeMCPPool{readContents: map[string]*mcp.ReadResourceResult{
		"demo|file:///README.md": {
			Contents: []mcp.ContentBlock{{Type: "text", Text: "hello from resource"}},
		},
	}}

	out, err := cmd.Execute(context.Background(), Input{
		Args:    []string{"read", "demo", "file:///README.md"},
		MCPPool: pool,
	})
	if err != nil {
		t.Fatalf("mcp read error: %v", err)
	}
	if !strings.Contains(out.Message, "hello from resource") {
		t.Fatalf("expected resource content, got %q", out.Message)
	}
}

func TestCheckpointsCmdListsEntries(t *testing.T) {
	eng := &fakeEngine{checkpts: []string{"abc1234 msg1", "def5678 msg2"}}
	cmd := NewCheckpointsCmd()

	out, err := cmd.Execute(context.Background(), Input{Engine: eng})
	if err != nil {
		t.Fatalf("checkpoints error: %v", err)
	}
	if !strings.Contains(out.Message, "abc1234") || !strings.Contains(out.Message, "def5678") {
		t.Fatalf("expected checkpoints in output, got %q", out.Message)
	}
}

func TestUndoCmdRestoresLatestWhenNoArg(t *testing.T) {
	eng := &fakeEngine{}
	cmd := NewUndoCmd()

	out, err := cmd.Execute(context.Background(), Input{Engine: eng})
	if err != nil {
		t.Fatalf("undo error: %v", err)
	}
	if eng.restored != "" {
		t.Fatalf("expected empty hash for latest checkpoint restore, got %q", eng.restored)
	}
	if !strings.Contains(out.Message, "最近检查点") {
		t.Fatalf("unexpected output: %q", out.Message)
	}
}

func TestUndoCmdNamesTheBackup(t *testing.T) {
	eng := &fakeEngine{backup: "9f8e7d6c5b4a39281706f5e4d3c2b1a098765432"}
	out, _ := NewUndoCmd().Execute(context.Background(), Input{Engine: eng})
	if !strings.Contains(out.Message, "/undo 9f8e7d6c") {
		t.Fatalf("undo output %q does not say how to reverse it", out.Message)
	}
}

func TestRateLimitCmdRendersSnapshot(t *testing.T) {
	eng := &fakeEngine{rlInfo: api.RateLimitInfo{RequestsLimit: 100, RequestsRemaining: 88, TokensLimit: 100000, TokensRemaining: 92345}}
	cmd := NewRateLimitCmd()

	out, err := cmd.Execute(context.Background(), Input{Engine: eng})
	if err != nil {
		t.Fatalf("ratelimit error: %v", err)
	}
	if !strings.Contains(out.Message, "Requests: 88 / 100") || !strings.Contains(out.Message, "Tokens: 92345 / 100000") {
		t.Fatalf("unexpected ratelimit output: %q", out.Message)
	}
}

func TestContextCmdRefreshesGitStatus(t *testing.T) {
	tmp := t.TempDir()
	origWD, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	defer func() { _ = os.Chdir(origWD) }()
	if err := os.Chdir(tmp); err != nil {
		t.Fatalf("chdir temp: %v", err)
	}
	runGit(t, tmp, "init")
	runGit(t, tmp, "config", "user.email", "test@example.com")
	runGit(t, tmp, "config", "user.name", "Test User")
	if err := os.WriteFile(filepath.Join(tmp, "a.txt"), []byte("hello\n"), 0644); err != nil {
		t.Fatalf("write file: %v", err)
	}
	runGit(t, tmp, "add", "a.txt")
	runGit(t, tmp, "commit", "-m", "init")

	pc := ctxt.Collect()

	if err := os.WriteFile(filepath.Join(tmp, "a.txt"), []byte("hello changed\n"), 0644); err != nil {
		t.Fatalf("rewrite file: %v", err)
	}

	out, err := NewContextCmd().Execute(context.Background(), Input{ProjectContext: pc})
	if err != nil {
		t.Fatalf("context execute: %v", err)
	}
	if strings.Contains(out.Message, "(clean)") {
		t.Fatalf("expected refreshed dirty git status, got %q", out.Message)
	}
	if !strings.Contains(out.Message, "a.txt") {
		t.Fatalf("expected changed file in git status, got %q", out.Message)
	}
}

func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v failed: %v\n%s", args, err, string(out))
	}
}

// /undo and /commit rewrite the working tree or the repository a running task
// is editing, so a front end must refuse them until the task ends.
func TestUndoAndCommitMutateEngine(t *testing.T) {
	for _, c := range []Command{NewUndoCmd(), NewCommitCmd()} {
		if !Mutates(c, nil) || !Mutates(c, []string{"x"}) {
			t.Errorf("/%s does not declare MutatesEngine; it would run mid-task", c.Name())
		}
	}
}

func TestUndoCmdNamesTheBackupOnError(t *testing.T) {
	eng := &fakeEngine{backup: "9f8e7d6c5b4a39281706f5e4d3c2b1a098765432", restErr: errors.New("删除 x 失败")}
	out, _ := NewUndoCmd().Execute(context.Background(), Input{Engine: eng})
	if !strings.Contains(out.Message, "删除 x 失败") || !strings.Contains(out.Message, "/undo 9f8e7d6c") {
		t.Fatalf("undo output %q must report the error and still say how to reverse it", out.Message)
	}
}
