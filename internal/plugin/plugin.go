package plugin

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/liuzhixin405/cove-agent/internal/textutil"
)

type State int

const (
	Disabled State = iota
	Enabled
	Error
)

type Manifest struct {
	Name        string   `json:"name"`
	Version     string   `json:"version"`
	Description string   `json:"description"`
	Author      string   `json:"author,omitempty"`
	Commands    []string `json:"commands,omitempty"`
	Tools       []string `json:"tools,omitempty"`
	Hooks       []string `json:"hooks,omitempty"`
	Skills      []string `json:"skills,omitempty"`
}

type Entry struct {
	Manifest Manifest
	Dir      string
	State    State
	Error    string
}

type Manager struct {
	plugins     map[string]*Entry
	dir         string
	marketplace *Marketplace
	installing  map[string]bool
	mu          sync.RWMutex
}

func NewManager() *Manager {
	home, _ := os.UserHomeDir()
	return &Manager{
		plugins: make(map[string]*Entry),
		dir:     filepath.Join(home, ".cove", "plugins"),
	}
}

func (m *Manager) Init() {
	_ = os.MkdirAll(m.dir, 0755)
	m.marketplace = NewMarketplace(m.dir)
	m.scanPlugins()
}

func (m *Manager) Refresh() {
	m.mu.Lock()
	m.plugins = make(map[string]*Entry)
	m.mu.Unlock()
	m.scanPlugins()
}

func (m *Manager) Dir() string {
	return m.dir
}

func (m *Manager) scanPlugins() {
	entries, _ := os.ReadDir(m.dir)
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if err := m.loadPlugin(e.Name()); err != nil {
			m.recordPluginError(e.Name(), err)
		}
	}
}

func (m *Manager) loadPlugin(name string) error {
	state := Enabled
	dirName := name
	if strings.HasSuffix(name, ".disabled") {
		state = Disabled
		dirName = strings.TrimSuffix(name, ".disabled")
	}
	dir := filepath.Join(m.dir, name)
	data, err := os.ReadFile(filepath.Join(dir, "manifest.json"))
	if err != nil {
		return fmt.Errorf("manifest: %w", err)
	}
	var manifest Manifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		return fmt.Errorf("parse manifest: %w", err)
	}
	if manifest.Name == "" {
		manifest.Name = dirName
	}
	m.mu.Lock()
	m.plugins[manifest.Name] = &Entry{
		Manifest: manifest,
		Dir:      dir,
		State:    state,
	}
	m.mu.Unlock()
	return nil
}

func (m *Manager) recordPluginError(name string, err error) {
	dirName := name
	state := Error
	if strings.HasSuffix(name, ".disabled") {
		dirName = strings.TrimSuffix(name, ".disabled")
	}
	m.mu.Lock()
	m.plugins[dirName] = &Entry{
		Manifest: Manifest{Name: dirName, Version: "unknown", Description: "Plugin could not be loaded"},
		Dir:      filepath.Join(m.dir, name),
		State:    state,
		Error:    err.Error(),
	}
	m.mu.Unlock()
}

func (m *Manager) Install(name string, url string) error {
	return m.InstallContext(context.Background(), name, url)
}

func (m *Manager) InstallContext(ctx context.Context, name string, url string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	pluginDir, err := pluginDirFor(m.dir, name)
	if err != nil {
		return err
	}
	m.mu.Lock()
	if m.installing[name] {
		m.mu.Unlock()
		return fmt.Errorf("plugin %s installation already in progress", name)
	}
	if _, err := os.Stat(pluginDir); err == nil {
		m.mu.Unlock()
		return fmt.Errorf("plugin %s already installed", name)
	}
	if _, err := os.Stat(pluginDir + ".disabled"); err == nil {
		m.mu.Unlock()
		return fmt.Errorf("plugin %s already installed", name)
	}
	if m.installing == nil {
		m.installing = make(map[string]bool)
	}
	m.installing[name] = true
	m.mu.Unlock()
	defer func() {
		m.mu.Lock()
		delete(m.installing, name)
		m.mu.Unlock()
	}()
	if err := os.MkdirAll(m.dir, 0755); err != nil {
		return err
	}
	stagingDir, err := os.MkdirTemp(filepath.Dir(m.dir), ".cove-plugin-install-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(stagingDir)
	if url != "" {
		if err := cloneRepoContext(ctx, url, stagingDir); err != nil {
			return err
		}
		ensureManifest(stagingDir)
	} else {
		manifest := Manifest{Name: name, Version: "0.1.0", Description: fmt.Sprintf("Plugin: %s", name)}
		data, err := json.MarshalIndent(manifest, "", "  ")
		if err != nil {
			return fmt.Errorf("encode manifest: %w", err)
		}
		if err := os.WriteFile(filepath.Join(stagingDir, "manifest.json"), data, 0644); err != nil {
			return fmt.Errorf("write manifest: %w", err)
		}
	}
	data, err := os.ReadFile(filepath.Join(stagingDir, "manifest.json"))
	if err != nil {
		return fmt.Errorf("cloned repo has no readable manifest.json or .claude-plugin/plugin.json: %w", err)
	}
	var manifest Manifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		return fmt.Errorf("parse manifest: %w", err)
	}
	if manifest.Name == "" {
		manifest.Name = name
	}
	var commitSHA string
	if url != "" {
		commitSHA = getGitSHAContext(ctx, stagingDir)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, err := os.Stat(pluginDir); err == nil {
		return fmt.Errorf("plugin %s already installed", name)
	}
	if _, err := os.Stat(pluginDir + ".disabled"); err == nil {
		return fmt.Errorf("plugin %s already installed", name)
	}
	if err := os.Rename(stagingDir, pluginDir); err != nil {
		return fmt.Errorf("commit plugin: %w", err)
	}
	if url != "" && m.marketplace != nil {
		if err := m.marketplace.recordInstall(name, url, pluginDir, commitSHA); err != nil {
			_ = os.RemoveAll(pluginDir)
			return fmt.Errorf("save lockfile: %w", err)
		}
	}
	m.plugins[manifest.Name] = &Entry{Manifest: manifest, Dir: pluginDir, State: Enabled}
	return nil
}

func (m *Manager) loadPluginLocked(name string) error {
	dir := filepath.Join(m.dir, name)
	data, err := os.ReadFile(filepath.Join(dir, "manifest.json"))
	if err != nil {
		return fmt.Errorf("manifest: %w", err)
	}
	var manifest Manifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		return fmt.Errorf("parse manifest: %w", err)
	}
	if manifest.Name == "" {
		manifest.Name = name
	}
	m.plugins[manifest.Name] = &Entry{
		Manifest: manifest,
		Dir:      dir,
		State:    Enabled,
	}
	return nil
}

func (m *Manager) Uninstall(name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	p, ok := m.plugins[name]
	if !ok {
		return fmt.Errorf("plugin %s not found", name)
	}
	delete(m.plugins, name)

	if err := os.RemoveAll(p.Dir); err != nil {
		return err
	}
	if err := os.RemoveAll(strings.TrimSuffix(p.Dir, ".disabled") + ".disabled"); err != nil {
		return err
	}
	// The lock entry is keyed by directory name, which can differ from the
	// manifest name the user typed. Left behind, it kept `/plugin update`
	// trying (and failing) to update a plugin that no longer exists.
	if m.marketplace != nil {
		if err := m.marketplace.forget(filepath.Base(strings.TrimSuffix(p.Dir, ".disabled"))); err != nil {
			return fmt.Errorf("save lockfile: %w", err)
		}
	}
	return nil
}

func (m *Manager) Disable(name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	p, ok := m.plugins[name]
	if !ok {
		return fmt.Errorf("plugin %s not found", name)
	}
	if p.State == Disabled {
		return nil
	}
	// An Error entry of a disabled plugin already points at "x.disabled";
	// appending again would rename it to "x.disabled.disabled".
	if strings.HasSuffix(p.Dir, ".disabled") {
		p.State = Disabled
		return nil
	}
	disabledDir := p.Dir + ".disabled"
	_ = os.RemoveAll(disabledDir)
	if err := os.Rename(p.Dir, disabledDir); err != nil {
		return err
	}
	p.Dir = disabledDir
	p.State = Disabled
	return nil
}

func (m *Manager) Enable(name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	p, ok := m.plugins[name]
	if !ok {
		return fmt.Errorf("plugin %s not found", name)
	}
	if p.State == Enabled {
		return nil
	}
	// A plugin that failed to load (recordPluginError) keeps its own
	// directory, without a ".disabled" suffix. Trimming the suffix was then a
	// no-op and the RemoveAll below deleted the plugin itself.
	if p.State == Error {
		return fmt.Errorf("插件 %s 加载失败：%s；请修复后重试", name, p.Error)
	}
	if !strings.HasSuffix(p.Dir, ".disabled") {
		return fmt.Errorf("插件 %s 的目录 %s 不是已禁用的目录，未做改动", name, p.Dir)
	}
	enabledDir := strings.TrimSuffix(p.Dir, ".disabled")
	// Never remove the plugin's own directory, whatever the entry says.
	if filepath.Clean(enabledDir) != filepath.Clean(p.Dir) {
		_ = os.RemoveAll(enabledDir)
	}
	if err := os.Rename(p.Dir, enabledDir); err != nil {
		return err
	}
	p.Dir = enabledDir
	p.State = Enabled
	return nil
}

func (m *Manager) AllPlugins() []Entry {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var result []Entry
	for _, p := range m.plugins {
		result = append(result, *p)
	}
	return result
}

func (m *Manager) EnabledTools() []string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var tools []string
	for _, p := range m.plugins {
		if p.State != Enabled {
			continue
		}
		tools = append(tools, p.Manifest.Tools...)
	}
	return tools
}

func (m *Manager) EnabledCommands() []string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var cmds []string
	for _, p := range m.plugins {
		if p.State != Enabled {
			continue
		}
		cmds = append(cmds, p.Manifest.Commands...)
	}
	return cmds
}

// CommandPrompt holds an executable plugin command: its prompt body (markdown)
// and a short description for help/completion.
type CommandPrompt struct {
	Plugin      string
	Description string
	Prompt      string
}

// CommandPrompts scans every enabled plugin's commands/ directory for markdown
// command files and returns them keyed by command name (filename without the
// .md extension). When a plugin command is invoked, its prompt body is injected
// into the engine as a user message. When two plugins define the same command,
// the plugin whose name sorts first wins. The plugins used to be visited in
// map order, which is random, so which plugin's command ran changed from one
// start to the next despite the "first writer wins" promise.
func (m *Manager) CommandPrompts() map[string]CommandPrompt {
	m.mu.RLock()
	names := make([]string, 0, len(m.plugins))
	for name, p := range m.plugins {
		if p.State == Enabled {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	dirs := make([]string, len(names))
	for i, name := range names {
		dirs[i] = m.plugins[name].Dir
	}
	m.mu.RUnlock()

	out := make(map[string]CommandPrompt)
	for i, dir := range dirs {
		cmdDir := filepath.Join(dir, "commands")
		entries, err := os.ReadDir(cmdDir)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(strings.ToLower(e.Name()), ".md") {
				continue
			}
			cmdName := strings.TrimSuffix(e.Name(), filepath.Ext(e.Name()))
			if cmdName == "" {
				continue
			}
			if _, exists := out[cmdName]; exists {
				continue // first writer wins
			}
			data, err := os.ReadFile(filepath.Join(cmdDir, e.Name()))
			if err != nil {
				continue
			}
			desc, body := parseCommandMarkdown(string(data))
			out[cmdName] = CommandPrompt{Plugin: names[i], Description: desc, Prompt: body}
		}
	}
	return out
}

// parseCommandMarkdown strips an optional YAML frontmatter block from a plugin
// command file and returns a short description plus the remaining prompt body.
// The description comes from a frontmatter "description:" field when present,
// otherwise the first non-empty content line.
func parseCommandMarkdown(content string) (description, body string) {
	// Windows editors may save a UTF-8 byte-order mark, which hid the
	// frontmatter from the "---" check below and sent it to the model.
	content = strings.TrimPrefix(content, string(rune(0xFEFF)))
	body = content
	if strings.HasPrefix(content, "---") {
		rest := content[3:]
		if idx := strings.Index(rest, "\n---"); idx >= 0 {
			front := rest[:idx]
			for _, line := range strings.Split(front, "\n") {
				line = strings.TrimSpace(line)
				if strings.HasPrefix(strings.ToLower(line), "description:") {
					description = strings.TrimSpace(line[len("description:"):])
					description = strings.Trim(description, "\"'")
				}
			}
			// Advance past the closing delimiter line.
			after := rest[idx+len("\n---"):]
			if nl := strings.IndexByte(after, '\n'); nl >= 0 {
				body = after[nl+1:]
			} else {
				body = ""
			}
		}
	}
	body = strings.TrimSpace(body)
	if description == "" {
		for _, line := range strings.Split(body, "\n") {
			line = strings.TrimSpace(strings.TrimLeft(line, "#"))
			if line != "" {
				description = line
				break
			}
		}
	}
	// Clipped by rune: description[:57] cut Chinese text mid-character.
	description = textutil.ClipRunes(description, 60)
	return description, body
}

// --- Marketplace bridge methods (satisfy command interface assertions) ---

// MarketplaceSearch searches the plugin marketplace.
func (m *Manager) MarketplaceSearch(query string) string {
	if m.marketplace == nil {
		return "marketplace 未初始化"
	}
	entries := m.marketplace.Search(query)
	if len(entries) == 0 {
		if query != "" {
			return fmt.Sprintf("未找到匹配 %q 的插件 (试试 /plugin refresh 更新索引)", query)
		}
		return "marketplace 索引为空 (试试 /plugin refresh)"
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "可用插件 (%d 个):\n\n", len(entries))

	for _, e := range entries {
		installed := ""
		if _, err := os.Stat(filepath.Join(m.dir, e.Name)); err == nil {
			installed = " [已安装]"
		}
		ver := e.Version
		if ver == "" || ver == "latest" {
			ver = ""
		} else {
			ver = " v" + ver
		}
		// Name + version + installed badge
		fmt.Fprintf(&sb, "  %s%s%s\n", e.Name, ver, installed)
		// Description (truncated to keep it readable)
		desc := textutil.ClipRunes(e.Description, 80)
		if desc != "" {
			fmt.Fprintf(&sb, "    %s\n", desc)
		}
		if e.Author != "" {
			fmt.Fprintf(&sb, "    by %s\n", e.Author)
		}
		sb.WriteString("\n")
	}
	sb.WriteString("安装: /plugin install <名称>")
	return sb.String()
}

// MarketplaceRefresh updates the marketplace index.
func (m *Manager) MarketplaceRefresh() error {
	return m.MarketplaceRefreshContext(context.Background())
}

func (m *Manager) MarketplaceRefreshContext(ctx context.Context) error {
	if m.marketplace == nil {
		return fmt.Errorf("marketplace 未初始化")
	}
	return m.marketplace.RefreshContext(ctx)
}

// MarketplaceUpdate updates one or all plugins.
func (m *Manager) MarketplaceUpdate(name string) (string, error) {
	return m.MarketplaceUpdateContext(context.Background(), name)
}

func (m *Manager) MarketplaceUpdateContext(ctx context.Context, name string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if m.marketplace == nil {
		return "", fmt.Errorf("marketplace 未初始化")
	}
	if name != "" {
		if err := m.marketplace.UpdateContext(ctx, name); err != nil {
			return "", err
		}
		lock, ok := m.marketplace.LockInfo(name)
		if !ok {
			return fmt.Sprintf("✓ %s 已更新", name), nil
		}
		return fmt.Sprintf("✓ %s 已更新到 %s (%s)", name, lock.Version, shortSHA(lock.CommitSHA)), nil
	}
	// Update all
	updated, errs := m.marketplace.UpdateAllContext(ctx)
	var sb strings.Builder
	if len(updated) > 0 {
		fmt.Fprintf(&sb, "✓ 已更新 %d 个插件: %s\n", len(updated), strings.Join(updated, ", "))
	} else if len(errs) == 0 {
		sb.WriteString("所有插件已是最新\n")
	} else {
		sb.WriteString("没有插件被更新\n")
	}
	if len(errs) > 0 {
		fmt.Fprintf(&sb, "⚠ %d 个失败: %s\n", len(errs), strings.Join(errs, "; "))
	}
	return sb.String(), nil
}

// shortSHA returns the first 7 characters of a commit SHA, the whole string if
// shorter, and "" when unknown — it never slices out of range on empty/short input
// (the former lock.CommitSHA[:7] panicked when the SHA was empty).
func shortSHA(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}

// Marketplace returns the marketplace instance (for advanced usage).
func (m *Manager) Marketplace() *Marketplace {
	return m.marketplace
}

// MarketplaceInstall installs a plugin from the marketplace by name.
func (m *Manager) MarketplaceInstall(name string) error {
	return m.MarketplaceInstallContext(context.Background(), name)
}

func (m *Manager) MarketplaceInstallContext(ctx context.Context, name string) error {
	if m.marketplace == nil {
		return fmt.Errorf("marketplace 未初始化")
	}
	// The index lookup ignores case and installs into the entry's own
	// directory name; reloading the name as typed ("Foo" for entry "foo")
	// failed after a successful install on a case-sensitive file system.
	installed, err := m.marketplace.installFromMarketplaceContext(ctx, name)
	if err != nil {
		return err
	}
	// Reload into plugin map
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.loadPluginLocked(installed)
}
