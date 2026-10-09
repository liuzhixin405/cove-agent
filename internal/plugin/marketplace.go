package plugin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/liuzhixin405/cove-agent/internal/fsatomic"
	"github.com/liuzhixin405/cove-agent/internal/log"
)

// MarketplaceEntry describes a plugin available in a marketplace.
type MarketplaceEntry struct {
	Name        string   `json:"name"`
	Description string   `json:"description"`
	Author      string   `json:"author"`
	Version     string   `json:"version"`
	Source      string   `json:"source"` // git URL or local path
	Keywords    []string `json:"keywords,omitempty"`
	Category    string   `json:"category,omitempty"`
	Stars       int      `json:"stars,omitempty"`
	Downloads   int      `json:"downloads,omitempty"`
	UpdatedAt   string   `json:"updated_at,omitempty"`
	// Marketplace is the name of the source the entry was listed by and Path
	// its directory inside that source's cached repository (slash-separated,
	// e.g. "plugins/foo"). An install copies exactly that directory; the cache
	// used to be searched across every marketplace by the entry's name.
	Marketplace string `json:"marketplace,omitempty"`
	Path        string `json:"path,omitempty"`
}

// MarketplaceSource is a registry that provides plugin listings.
type MarketplaceSource struct {
	Name    string `json:"name"`
	Type    string `json:"type"` // "git", "url", "file", "directory"
	URL     string `json:"url"`
	Enabled bool   `json:"enabled"`
}

// Lockfile tracks installed plugin versions and sources.
type Lockfile struct {
	Plugins map[string]LockEntry `json:"plugins"`
}

// LockEntry records install metadata for a plugin.
type LockEntry struct {
	Source      string `json:"source"`
	Version     string `json:"version"`
	CommitSHA   string `json:"commit_sha,omitempty"`
	InstalledAt string `json:"installed_at"`
	UpdatedAt   string `json:"updated_at,omitempty"`
	AutoUpdate  bool   `json:"auto_update"`
}

// Marketplace manages plugin discovery and remote installation.
type Marketplace struct {
	mu       sync.Mutex
	dir      string // ~/.cove/plugins
	cacheDir string // ~/.cove/marketplace/cache
	sources  []MarketplaceSource
	index    []MarketplaceEntry // cached index from all sources
	lockfile Lockfile
}

const defaultMarketplaceRepo = "https://github.com/anthropics/claude-plugins-official.git"

// NewMarketplace creates a marketplace manager.
func NewMarketplace(pluginDir string) *Marketplace {
	home, _ := os.UserHomeDir()
	cacheDir := filepath.Join(home, ".cove", "marketplace", "cache")
	_ = os.MkdirAll(cacheDir, 0755)

	m := &Marketplace{
		dir:      pluginDir,
		cacheDir: cacheDir,
		lockfile: Lockfile{Plugins: make(map[string]LockEntry)},
	}
	m.loadSources()
	m.loadLockfile()
	m.loadCachedIndex()
	return m
}

// --- Sources Management ---

func (m *Marketplace) sourcesFile() string {
	return filepath.Join(filepath.Dir(m.dir), "marketplace", "sources.json")
}

func (m *Marketplace) loadSources() {
	data, err := os.ReadFile(m.sourcesFile())
	if err != nil {
		// Default: official marketplace
		m.sources = []MarketplaceSource{
			{Name: "official", Type: "git", URL: defaultMarketplaceRepo, Enabled: true},
		}
		return
	}
	_ = json.Unmarshal(data, &m.sources)
	if len(m.sources) == 0 {
		m.sources = []MarketplaceSource{
			{Name: "official", Type: "git", URL: defaultMarketplaceRepo, Enabled: true},
		}
	}
}

func (m *Marketplace) saveSources() error {
	if err := os.MkdirAll(filepath.Dir(m.sourcesFile()), 0755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(m.sources, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(m.sourcesFile(), data, 0644)
}

// AddSource registers a new marketplace source.
func (m *Marketplace) AddSource(name, sourceType, url string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	for _, s := range m.sources {
		if s.Name == name {
			return fmt.Errorf("source %q already exists", name)
		}
	}
	if sourceType != "git" && sourceType != "url" && sourceType != "file" && sourceType != "directory" {
		return fmt.Errorf("unsupported source type %q (use: git, url, file, directory)", sourceType)
	}

	m.sources = append(m.sources, MarketplaceSource{
		Name: name, Type: sourceType, URL: url, Enabled: true,
	})
	return m.saveSources()
}

// RemoveSource removes a marketplace source.
func (m *Marketplace) RemoveSource(name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if name == "official" {
		return fmt.Errorf("cannot remove official marketplace")
	}
	for i, s := range m.sources {
		if s.Name == name {
			m.sources = append(m.sources[:i], m.sources[i+1:]...)
			return m.saveSources()
		}
	}
	return fmt.Errorf("source %q not found", name)
}

// Sources returns all configured marketplace sources.
func (m *Marketplace) Sources() []MarketplaceSource {
	m.mu.Lock()
	defer m.mu.Unlock()
	result := make([]MarketplaceSource, len(m.sources))
	copy(result, m.sources)
	return result
}

// --- Index / Search ---

func (m *Marketplace) indexFile() string {
	return filepath.Join(filepath.Dir(m.dir), "marketplace", "index.json")
}

func (m *Marketplace) loadCachedIndex() {
	data, err := os.ReadFile(m.indexFile())
	if err != nil {
		return
	}
	_ = json.Unmarshal(data, &m.index)
}

func (m *Marketplace) saveIndex() error {
	if err := os.MkdirAll(filepath.Dir(m.indexFile()), 0755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(m.index, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(m.indexFile(), data, 0644)
}

// Refresh fetches the latest index from all enabled sources.
func (m *Marketplace) Refresh() error {
	return m.RefreshContext(context.Background())
}

func (m *Marketplace) RefreshContext(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()

	var allEntries []MarketplaceEntry
	var errs []string

	for _, src := range m.sources {
		if err := ctx.Err(); err != nil {
			return err
		}
		if !src.Enabled {
			continue
		}
		entries, err := m.fetchSourceContext(ctx, src)
		if err != nil {
			errs = append(errs, fmt.Sprintf("%s: %v", src.Name, err))
			if !errors.Is(err, ErrMarketplaceStale) {
				continue
			}
		}
		allEntries = append(allEntries, entries...)
	}

	if err := ctx.Err(); err != nil {
		return err
	}
	m.index = allEntries
	if err := m.saveIndex(); err != nil {
		errs = append(errs, fmt.Sprintf("cache index: %v", err))
	}

	if len(errs) > 0 {
		return fmt.Errorf("some sources failed: %s", strings.Join(errs, "; "))
	}
	return nil
}

func (m *Marketplace) fetchSource(src MarketplaceSource) ([]MarketplaceEntry, error) {
	return m.fetchSourceContext(context.Background(), src)
}

func (m *Marketplace) fetchSourceContext(ctx context.Context, src MarketplaceSource) ([]MarketplaceEntry, error) {
	switch src.Type {
	case "git":
		return m.fetchGitSourceContext(ctx, src)
	case "file":
		return m.fetchFileSource(src.URL)
	case "directory":
		return m.fetchDirectorySource(src.URL)
	default:
		return nil, fmt.Errorf("unsupported source type: %s", src.Type)
	}
}

// claudePluginJSON represents the .claude-plugin/plugin.json format used by
// anthropics/claude-plugins-official.
type claudePluginJSON struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Version     string `json:"version"`
	Author      struct {
		Name  string `json:"name"`
		Email string `json:"email"`
	} `json:"author"`
	Homepage string `json:"homepage"`
}

func (m *Marketplace) fetchGitSource(src MarketplaceSource) ([]MarketplaceEntry, error) {
	return m.fetchGitSourceContext(context.Background(), src)
}

func (m *Marketplace) fetchGitSourceContext(ctx context.Context, src MarketplaceSource) ([]MarketplaceEntry, error) {
	repoDir := filepath.Join(m.cacheDir, sanitizeName(src.Name))
	// stale is set when the cached clone could not be updated; the entries
	// read from it are still returned, with this error, so /plugin refresh
	// says the index is old instead of "已更新".
	var stale error
	withStale := func(entries []MarketplaceEntry, err error) ([]MarketplaceEntry, error) {
		if err != nil {
			return entries, err
		}
		return entries, stale
	}

	// git's output is CAPTURED, never inherited.
	//
	// cmd.Stderr = os.Stderr hands the child process the real terminal, which
	// no front end can intercept: `git clone --progress` writes a live meter
	// built from carriage returns and erase sequences, straight past whatever
	// the UI thinks is on screen. The output goes to the log instead, and the
	// front end decides what to do with it.
	if _, err := os.Stat(filepath.Join(repoDir, ".git")); err == nil {
		// Pull latest
		if out, err := runGitCommand(ctx, "-C", repoDir, "pull", "--ff-only"); err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			log.Debugf("marketplace pull %s: %v: %s", src.Name, err, strings.TrimSpace(string(out)))
			stale = fmt.Errorf("%w (%v)", ErrMarketplaceStale, err)
		}
	} else {
		// Clone
		_ = os.MkdirAll(filepath.Dir(repoDir), 0755)
		log.Infof("正在克隆 marketplace 源: %s ...", src.Name)
		// --progress is dropped along with the inherited terminal: it only
		// draws a meter for a tty, and there is no tty to draw it on now.
		if err := cloneRepoContext(ctx, src.URL, repoDir); err != nil {
			return nil, fmt.Errorf("%s: %w", src.URL, err)
		}
	}

	// Try registry.json first (flat index format)
	registryPath := filepath.Join(repoDir, "registry.json")
	if _, err := os.Stat(registryPath); err == nil {
		entries, err := m.fetchFileSource(registryPath)
		for i := range entries {
			entries[i].Marketplace = src.Name
		}
		return withStale(entries, err)
	}

	// Fall back to Claude plugins official format:
	// scan plugins/*/.claude-plugin/plugin.json and external_plugins/*/.claude-plugin/plugin.json
	return withStale(m.fetchClaudePluginsRepo(repoDir, src))
}

// ErrMarketplaceStale: the source's clone could not be updated (offline, or
// the upstream was force-pushed so --ff-only refuses); the index was built
// from the cached copy.
var ErrMarketplaceStale = errors.New("marketplace index not updated, using cached copy")

// fetchClaudePluginsRepo scans a repo with anthropics/claude-plugins-official layout.
func (m *Marketplace) fetchClaudePluginsRepo(repoDir string, src MarketplaceSource) ([]MarketplaceEntry, error) {
	sourceURL := src.URL
	var entries []MarketplaceEntry

	dirs := []string{
		filepath.Join(repoDir, "plugins"),
		filepath.Join(repoDir, "external_plugins"),
	}

	for _, dir := range dirs {
		subdirs, err := os.ReadDir(dir)
		if err != nil {
			continue // directory may not exist
		}
		for _, sub := range subdirs {
			if !sub.IsDir() {
				continue
			}
			pluginJSONPath := filepath.Join(dir, sub.Name(), ".claude-plugin", "plugin.json")
			data, err := os.ReadFile(pluginJSONPath)
			if err != nil {
				continue
			}
			var cp claudePluginJSON
			if err := json.Unmarshal(data, &cp); err != nil {
				continue
			}
			name := cp.Name
			if name == "" {
				name = sub.Name()
			}
			// Drop entries whose name could not be used as a directory. Doing
			// it here keeps a hostile index entry out of the index (and out of
			// the lockfile) entirely, rather than only failing at install.
			if err := ValidatePluginName(name); err != nil {
				continue
			}
			version := cp.Version
			if version == "" {
				version = "latest"
			}
			// Derive source URL for individual plugin install
			plugSrc := sourceURL
			if cp.Homepage != "" {
				plugSrc = cp.Homepage
			}
			entries = append(entries, MarketplaceEntry{
				Name:        name,
				Description: cp.Description,
				Author:      cp.Author.Name,
				Version:     version,
				Source:      plugSrc,
				Category:    filepath.Base(dir), // "plugins" or "external_plugins"
				Marketplace: src.Name,
				Path:        filepath.Base(dir) + "/" + sub.Name(),
			})
		}
	}

	if len(entries) == 0 {
		return nil, fmt.Errorf("no registry.json and no .claude-plugin/plugin.json found in repo")
	}
	return entries, nil
}

func (m *Marketplace) fetchFileSource(path string) ([]MarketplaceEntry, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var entries []MarketplaceEntry
	if err := json.Unmarshal(data, &entries); err != nil {
		return nil, fmt.Errorf("parse registry: %w", err)
	}
	// registry.json comes from the cloned marketplace repo, so its names are
	// remote input. Filter here so a hostile entry never reaches the index,
	// the lockfile, or a path join.
	kept := entries[:0]
	for _, e := range entries {
		if err := ValidatePluginName(e.Name); err != nil {
			continue
		}
		kept = append(kept, e)
	}
	return kept, nil
}

func (m *Marketplace) fetchDirectorySource(dir string) ([]MarketplaceEntry, error) {
	// Each subdirectory is a plugin with a manifest.json
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var result []MarketplaceEntry
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, e.Name(), "manifest.json"))
		if err != nil {
			continue
		}
		var manifest Manifest
		if err := json.Unmarshal(data, &manifest); err != nil {
			continue
		}
		if err := ValidatePluginName(manifest.Name); err != nil {
			continue
		}
		result = append(result, MarketplaceEntry{
			Name:        manifest.Name,
			Description: manifest.Description,
			Author:      manifest.Author,
			Version:     manifest.Version,
			Source:      filepath.Join(dir, e.Name()),
		})
	}
	return result, nil
}

// Search finds plugins matching a query in the cached index.
func (m *Marketplace) Search(query string) []MarketplaceEntry {
	m.mu.Lock()
	defer m.mu.Unlock()

	if query == "" {
		result := make([]MarketplaceEntry, len(m.index))
		copy(result, m.index)
		return result
	}

	q := strings.ToLower(query)
	var matches []MarketplaceEntry
	for _, entry := range m.index {
		score := 0
		if strings.Contains(strings.ToLower(entry.Name), q) {
			score += 10
		}
		if strings.Contains(strings.ToLower(entry.Description), q) {
			score += 5
		}
		if strings.Contains(strings.ToLower(entry.Category), q) {
			score += 3
		}
		for _, kw := range entry.Keywords {
			if strings.Contains(strings.ToLower(kw), q) {
				score += 2
			}
		}
		if score > 0 {
			matches = append(matches, entry)
		}
	}

	// Sort by relevance (name match first, then description)
	sort.Slice(matches, func(i, j int) bool {
		iName := strings.Contains(strings.ToLower(matches[i].Name), q)
		jName := strings.Contains(strings.ToLower(matches[j].Name), q)
		if iName != jName {
			return iName
		}
		return matches[i].Downloads > matches[j].Downloads
	})

	return matches
}

// List returns all available plugins from the cached index.
func (m *Marketplace) List() []MarketplaceEntry {
	return m.Search("")
}

// --- Remote Installation ---

// InstallFromMarketplace installs a plugin by name from the registry.
func (m *Marketplace) InstallFromMarketplace(name string) error {
	return m.InstallFromMarketplaceContext(context.Background(), name)
}

func (m *Marketplace) InstallFromMarketplaceContext(ctx context.Context, name string) error {
	_, err := m.installFromMarketplaceContext(ctx, name)
	return err
}

// installFromMarketplace installs the index entry matching name (ignoring
// case) and returns the entry's name, which is the directory it went into.
func (m *Marketplace) installFromMarketplace(name string) (string, error) {
	return m.installFromMarketplaceContext(context.Background(), name)
}

func (m *Marketplace) installFromMarketplaceContext(ctx context.Context, name string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	m.mu.Lock()
	defer m.mu.Unlock()

	// Find in index
	var entry *MarketplaceEntry
	for i := range m.index {
		if strings.EqualFold(m.index[i].Name, name) {
			entry = &m.index[i]
			break
		}
	}
	if entry == nil {
		return "", fmt.Errorf("plugin %q not found in marketplace (try: /plugin refresh)", name)
	}

	e := *entry
	return e.Name, m.installFromSourceContext(ctx, e.Name, e.Source, e.Version, &e)
}

// InstallFromGit installs a plugin directly from a git URL.
func (m *Marketplace) InstallFromGit(url string) error {
	return m.InstallFromGitContext(context.Background(), url)
}

func (m *Marketplace) InstallFromGitContext(ctx context.Context, url string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	// Derive name from URL (last path segment without .git)
	name := deriveNameFromURL(url)
	if name == "" {
		return fmt.Errorf("cannot derive plugin name from URL: %s", url)
	}

	// No index entry: the URL the user gave is cloned, never a cached plugin
	// that merely has the same name.
	return m.installFromSourceContext(ctx, name, url, "", nil)
}

// installFromSource installs name from source. entry is the index entry being
// installed, if any; only its own marketplace's cache is used for a copy.
func (m *Marketplace) installFromSource(name, source, version string, entry *MarketplaceEntry) error {
	return m.installFromSourceContext(context.Background(), name, source, version, entry)
}

func (m *Marketplace) installFromSourceContext(ctx context.Context, name, source, version string, entry *MarketplaceEntry) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	// name reaches here from the marketplace index, which is built by reading
	// manifest "name" fields out of a CLONED REMOTE REPOSITORY. It is joined
	// into a path that is then git-cloned into and RemoveAll'd on failure, so
	// an entry naming itself "../../../.ssh" redirected both outside m.dir.
	pluginDir, err := pluginDirFor(m.dir, name)
	if err != nil {
		return err
	}

	// Check already installed
	if _, err := os.Stat(pluginDir); err == nil {
		return fmt.Errorf("plugin %q already installed (use /plugin update %s)", name, name)
	}
	if _, err := os.Stat(pluginDir + ".disabled"); err == nil {
		return fmt.Errorf("plugin %q already installed (disabled)", name)
	}

	// Try to find plugin in local marketplace cache (from claude-plugins-official layout)
	if cachedDir := m.findCachedPlugin(entry); cachedDir != "" {
		if err := copyDir(cachedDir, pluginDir); err != nil {
			_ = os.RemoveAll(pluginDir)
			return fmt.Errorf("copy from cache: %w", err)
		}
		if err := ctx.Err(); err != nil {
			_ = os.RemoveAll(pluginDir)
			return err
		}
		// Generate manifest.json from .claude-plugin/plugin.json if needed
		m.ensureManifest(pluginDir)

		if version == "" {
			version = readManifestVersion(pluginDir)
		}
		m.lockfile.Plugins[name] = LockEntry{
			Source:      source,
			Version:     version,
			InstalledAt: time.Now().Format(time.RFC3339),
			AutoUpdate:  true,
		}
		if err := m.saveLockfile(); err != nil {
			return fmt.Errorf("save lockfile: %w", err)
		}
		return nil
	}

	// Validate source URL
	if !isValidSource(source) {
		return fmt.Errorf("invalid plugin source: %s", source)
	}

	// Clone the plugin repo
	if isGitURL(source) {
		if err := cloneRepoContext(ctx, source, pluginDir); err != nil {
			_ = os.RemoveAll(pluginDir) // cleanup on failure
			return err
		}
	} else {
		// Local path: copy directory
		if err := copyDir(source, pluginDir); err != nil {
			_ = os.RemoveAll(pluginDir)
			return fmt.Errorf("copy plugin: %w", err)
		}
	}

	// Generate manifest.json from .claude-plugin/plugin.json if needed
	m.ensureManifest(pluginDir)

	// Validate manifest exists
	manifestPath := filepath.Join(pluginDir, "manifest.json")
	if _, err := os.Stat(manifestPath); os.IsNotExist(err) {
		_ = os.RemoveAll(pluginDir)
		return fmt.Errorf("plugin has no manifest.json or .claude-plugin/plugin.json")
	}

	// Get commit SHA for version lock
	sha := getGitSHAContext(ctx, pluginDir)
	if err := ctx.Err(); err != nil {
		_ = os.RemoveAll(pluginDir)
		return err
	}
	if version == "" {
		version = readManifestVersion(pluginDir)
	}

	// Update lockfile
	m.lockfile.Plugins[name] = LockEntry{
		Source:      source,
		Version:     version,
		CommitSHA:   sha,
		InstalledAt: time.Now().Format(time.RFC3339),
		AutoUpdate:  true,
	}
	if err := m.saveLockfile(); err != nil {
		return fmt.Errorf("save lockfile: %w", err)
	}
	return nil
}

// --- Update ---

// Update updates a single plugin to latest.
func (m *Marketplace) Update(name string) error {
	return m.UpdateContext(context.Background(), name)
}

func (m *Marketplace) UpdateContext(ctx context.Context, name string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()

	lock, ok := m.lockfile.Plugins[name]
	if !ok {
		return fmt.Errorf("plugin %q not tracked (was it installed via marketplace?)", name)
	}

	// The lockfile was written from index data, so the name is no more
	// trustworthy here than at install time.
	pluginDir, err := pluginDirFor(m.dir, name)
	if err != nil {
		return err
	}
	if _, err := os.Stat(filepath.Join(pluginDir, ".git")); err != nil {
		return fmt.Errorf("plugin %q is not a git repo, cannot update", name)
	}

	// Pull latest
	if out, err := runGitCommand(ctx, "-C", pluginDir, "pull", "--ff-only", "--quiet"); err != nil {
		return fmt.Errorf("git pull failed: %s: %w", strings.TrimSpace(string(out)), err)
	}

	// Update lock
	lock.CommitSHA = getGitSHAContext(ctx, pluginDir)
	if err := ctx.Err(); err != nil {
		return err
	}
	lock.Version = readManifestVersion(pluginDir)
	lock.UpdatedAt = time.Now().Format(time.RFC3339)
	m.lockfile.Plugins[name] = lock
	if err := m.saveLockfile(); err != nil {
		return fmt.Errorf("save lockfile: %w", err)
	}
	return nil
}

// UpdateAll updates all plugins with auto_update=true.
func (m *Marketplace) UpdateAll() (updated []string, errs []string) {
	return m.UpdateAllContext(context.Background())
}

func (m *Marketplace) UpdateAllContext(ctx context.Context) (updated []string, errs []string) {
	m.mu.Lock()
	defer m.mu.Unlock()

	for name, lock := range m.lockfile.Plugins {
		if err := ctx.Err(); err != nil {
			errs = append(errs, err.Error())
			break
		}
		if !lock.AutoUpdate {
			continue
		}
		pluginDir, err := pluginDirFor(m.dir, name)
		if err != nil {
			errs = append(errs, fmt.Sprintf("%s: %v", name, err))
			continue
		}
		if _, err := os.Stat(filepath.Join(pluginDir, ".git")); err != nil {
			// A copy out of the marketplace cache has no .git and cannot be
			// pulled. Skipping it silently made "/plugin update" answer that
			// everything was current, so the plugin quietly never updated.
			if _, statErr := os.Stat(pluginDir); statErr == nil {
				errs = append(errs, fmt.Sprintf("%s: 从 marketplace 缓存复制安装，不是 git 仓库，无法原地更新（先 /plugin refresh，再卸载重装）", name))
			}
			continue
		}

		if out, err := runGitCommand(ctx, "-C", pluginDir, "pull", "--ff-only", "--quiet"); err != nil {
			errs = append(errs, fmt.Sprintf("%s: %v: %s", name, err, strings.TrimSpace(string(out))))
			continue
		}

		newSHA := getGitSHAContext(ctx, pluginDir)
		if err := ctx.Err(); err != nil {
			errs = append(errs, err.Error())
			break
		}
		if newSHA != lock.CommitSHA {
			lock.CommitSHA = newSHA
			lock.Version = readManifestVersion(pluginDir)
			lock.UpdatedAt = time.Now().Format(time.RFC3339)
			m.lockfile.Plugins[name] = lock
			updated = append(updated, name)
		}
	}

	if len(updated) > 0 {
		if err := m.saveLockfile(); err != nil {
			errs = append(errs, fmt.Sprintf("save lockfile: %v", err))
		}
	}
	return
}

// --- Lockfile ---

func (m *Marketplace) lockfilePath() string {
	return filepath.Join(filepath.Dir(m.dir), "marketplace", "lock.json")
}

func (m *Marketplace) loadLockfile() {
	data, err := os.ReadFile(m.lockfilePath())
	if err != nil {
		return
	}
	_ = json.Unmarshal(data, &m.lockfile)
	if m.lockfile.Plugins == nil {
		m.lockfile.Plugins = make(map[string]LockEntry)
	}
}

func (m *Marketplace) saveLockfile() error {
	if err := os.MkdirAll(filepath.Dir(m.lockfilePath()), 0755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(m.lockfile, "", "  ")
	if err != nil {
		return err
	}
	return fsatomic.WriteFile(m.lockfilePath(), data, 0644)
}

// recordInstall adds a lock entry for a plugin cloned into dir outside the
// marketplace (from a URL the user gave), so update can find it.
func (m *Marketplace) recordInstall(name, source, dir, commitSHA string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	previous, existed := m.lockfile.Plugins[name]
	m.lockfile.Plugins[name] = LockEntry{
		Source:      source,
		Version:     readManifestVersion(dir),
		CommitSHA:   commitSHA,
		InstalledAt: time.Now().Format(time.RFC3339),
		AutoUpdate:  true,
	}
	if err := m.saveLockfile(); err != nil {
		if existed {
			m.lockfile.Plugins[name] = previous
		} else {
			delete(m.lockfile.Plugins, name)
		}
		return err
	}
	return nil
}

// forget drops a plugin's lock entry after it has been uninstalled.
func (m *Marketplace) forget(name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.lockfile.Plugins[name]; !ok {
		return nil
	}
	delete(m.lockfile.Plugins, name)
	return m.saveLockfile()
}

// LockInfo returns the lock entry for a plugin.
func (m *Marketplace) LockInfo(name string) (LockEntry, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	e, ok := m.lockfile.Plugins[name]
	return e, ok
}

// --- Helpers ---

func sanitizeName(s string) string {
	s = strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' {
			return r
		}
		return '_'
	}, s)
	return s
}

func deriveNameFromURL(url string) string {
	url = strings.TrimSuffix(url, ".git")
	url = strings.TrimRight(url, "/")
	parts := strings.Split(url, "/")
	if len(parts) == 0 {
		return ""
	}
	name := parts[len(parts)-1]
	return sanitizeName(name)
}

func isGitURL(s string) bool {
	return strings.HasPrefix(s, "https://") || strings.HasPrefix(s, "http://") ||
		strings.HasPrefix(s, "git@") || strings.HasPrefix(s, "ssh://")
}

func isValidSource(source string) bool {
	if source == "" {
		return false
	}
	// Block path traversal
	if strings.Contains(source, "..") {
		return false
	}
	// Must be a git URL or absolute/relative path
	if isGitURL(source) {
		return true
	}
	// Local path
	return filepath.IsAbs(source) || strings.HasPrefix(source, "./")
}

func getGitSHA(dir string) string {
	return getGitSHAContext(context.Background(), dir)
}

func getGitSHAContext(ctx context.Context, dir string) string {
	out, err := runGitCommand(ctx, "-C", dir, "rev-parse", "HEAD")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func readManifestVersion(dir string) string {
	data, err := os.ReadFile(filepath.Join(dir, "manifest.json"))
	if err != nil {
		return "unknown"
	}
	var m Manifest
	if err := json.Unmarshal(data, &m); err != nil {
		return "unknown"
	}
	if m.Version == "" {
		return "0.0.0"
	}
	return m.Version
}

// copyDir copies a plugin directory tree out of the marketplace cache.
//
// Symlinks are SKIPPED, not dereferenced. filepath.Walk reports them via Lstat,
// so info.IsDir() is false and the previous os.ReadFile(path) followed the link
// and copied the *target's* content into the plugin directory. A marketplace
// repo — remote, untrusted content — could therefore ship a symlink named
// like an ordinary plugin file and have ~/.ssh/id_rsa or ~/.aws/credentials
// copied into a directory whose files are read back into model prompts.
//
// File modes are also normalized: info.Mode() on a symlink carries the
// ModeSymlink bit, and passing it to WriteFile left the permission bits as
// 0777 (world-writable) after masking.
func copyDir(src, dst string) error {
	return filepath.Walk(src, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)

		if info.Mode()&os.ModeSymlink != 0 {
			return nil
		}
		if info.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		// Anything that is not a regular file (device, socket, fifo) has no
		// business in a plugin and must not be read.
		if !info.Mode().IsRegular() {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		perm := os.FileMode(0o644)
		if info.Mode().Perm()&0o111 != 0 {
			perm = 0o755 // preserve the executable bit for hook scripts
		}
		return os.WriteFile(target, data, perm)
	})
}

// findCachedPlugin returns the directory of entry's plugin in its own
// marketplace's cached repository, or "" to install from entry's source.
//
// It used to scan every cached marketplace and take the first directory named
// like the entry, so an install could copy another marketplace's plugin (one
// that merely shared the name) while the lockfile recorded this entry's
// source. Now only the marketplace that listed the entry is looked at, and
// the directory the index recorded for it (entry.Path) is used.
func (m *Marketplace) findCachedPlugin(entry *MarketplaceEntry) string {
	if entry == nil {
		return ""
	}
	repoDir := m.entryRepoDir(entry)
	if repoDir == "" {
		return ""
	}
	if entry.Path != "" {
		candidate := filepath.Join(repoDir, filepath.FromSlash(entry.Path))
		// Path comes from the index file; keep it inside this repository.
		rel, err := filepath.Rel(repoDir, candidate)
		if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return ""
		}
		if isPluginDir(candidate) {
			return candidate
		}
		return ""
	}
	// An index written by an older cove has no path: look where the layouts
	// put a plugin of that name, and only take it if it declares that name.
	for _, candidate := range []string{
		filepath.Join(repoDir, "plugins", entry.Name),
		filepath.Join(repoDir, "external_plugins", entry.Name),
		filepath.Join(repoDir, entry.Name),
	} {
		if isPluginDir(candidate) && declaredPluginName(candidate) == entry.Name {
			return candidate
		}
	}
	return ""
}

// entryRepoDir is the cache directory of the git marketplace that listed
// entry, or "" when that is unknown or the source is no longer configured.
func (m *Marketplace) entryRepoDir(entry *MarketplaceEntry) string {
	for _, s := range m.sources {
		if s.Type != "git" {
			continue
		}
		// An index from an older cove does not name the marketplace; its
		// entries carry the marketplace repository's URL as their source.
		if (entry.Marketplace != "" && s.Name == entry.Marketplace) ||
			(entry.Marketplace == "" && s.URL != "" && s.URL == entry.Source) {
			return filepath.Join(m.cacheDir, sanitizeName(s.Name))
		}
	}
	return ""
}

func isPluginDir(dir string) bool {
	for _, f := range []string{"manifest.json", filepath.Join(".claude-plugin", "plugin.json")} {
		if info, err := os.Stat(filepath.Join(dir, f)); err == nil && info.Mode().IsRegular() {
			return true
		}
	}
	return false
}

// declaredPluginName is the name dir's manifest.json or .claude-plugin/plugin.json
// gives, or the directory name when neither names it.
func declaredPluginName(dir string) string {
	for _, f := range []string{"manifest.json", filepath.Join(".claude-plugin", "plugin.json")} {
		data, err := os.ReadFile(filepath.Join(dir, f))
		if err != nil {
			continue
		}
		var v struct {
			Name string `json:"name"`
		}
		if json.Unmarshal(data, &v) == nil && v.Name != "" {
			return v.Name
		}
	}
	return filepath.Base(dir)
}

// ensureManifest generates a manifest.json from .claude-plugin/plugin.json if the
// plugin does not already have a manifest.json (Claude official format compatibility).
func (m *Marketplace) ensureManifest(pluginDir string) {
	ensureManifest(pluginDir)
}

// ensureManifest generates a manifest.json from .claude-plugin/plugin.json if the
// plugin does not already have a manifest.json (Claude official format compatibility).
func ensureManifest(pluginDir string) {
	manifestPath := filepath.Join(pluginDir, "manifest.json")
	if _, err := os.Stat(manifestPath); err == nil {
		return // already has manifest.json
	}

	cpPath := filepath.Join(pluginDir, ".claude-plugin", "plugin.json")
	data, err := os.ReadFile(cpPath)
	if err != nil {
		return
	}

	var cp claudePluginJSON
	if err := json.Unmarshal(data, &cp); err != nil {
		return
	}

	name := cp.Name
	if name == "" {
		name = filepath.Base(pluginDir)
	}
	version := cp.Version
	if version == "" {
		version = "0.0.0"
	}

	// Scan for skills, commands, agents
	var skills, commands []string
	if entries, err := os.ReadDir(filepath.Join(pluginDir, "skills")); err == nil {
		for _, e := range entries {
			if e.IsDir() {
				skills = append(skills, e.Name())
			}
		}
	}
	if entries, err := os.ReadDir(filepath.Join(pluginDir, "commands")); err == nil {
		for _, e := range entries {
			if e.IsDir() {
				commands = append(commands, e.Name())
			}
		}
	}

	manifest := Manifest{
		Name:        name,
		Version:     version,
		Description: cp.Description,
		Author:      cp.Author.Name,
		Skills:      skills,
		Commands:    commands,
	}
	out, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return
	}
	if err := os.WriteFile(manifestPath, out, 0644); err != nil {
		// The plugin then loads without a manifest; say why.
		log.Warnf("plugin manifest %s not written: %v", manifestPath, err)
	}
}
