package plugin

import (
	"context"
	"errors"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// isolateHome points HOME/USERPROFILE at a temp dir and keeps the machine's
// system git config (credential helpers, core.symlinks) out of the test.
func isolateHome(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	t.Setenv("USERPROFILE", tmp)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	return tmp
}

// runGit runs git in dir and fails the test on error.
func runGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	full := append([]string{"-c", "user.name=t", "-c", "user.email=t@example.com", "-c", "commit.gpgsign=false"}, args...)
	cmd := exec.Command("git", full...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// newPluginRepo creates a local git repository holding a plugin whose
// manifest has the given version, and returns its path.
//
// Building one takes three git processes, a second or more on Windows; each
// (name, version) is built once per test binary and copied (with its .git)
// for every test that asks, so each test still gets a repository of its own.
func newPluginRepo(t *testing.T, name, version string) string {
	t.Helper()
	key := name + "@" + version
	pluginRepoMu.Lock()
	tmpl, ok := pluginRepoTemplates[key]
	if !ok {
		root, err := os.MkdirTemp("", "cove-plugin-tmpl-")
		if err != nil {
			pluginRepoMu.Unlock()
			t.Fatal(err)
		}
		tmpl = filepath.Join(root, "src-"+name)
		if err := os.MkdirAll(tmpl, 0o755); err != nil {
			pluginRepoMu.Unlock()
			t.Fatal(err)
		}
		runGit(t, tmpl, "init", "--quiet")
		writeManifestVersion(t, tmpl, name, version)
		runGit(t, tmpl, "add", "-A")
		runGit(t, tmpl, "commit", "--quiet", "-m", "v"+version)
		pluginRepoTemplates[key] = tmpl
	}
	pluginRepoMu.Unlock()

	dir := filepath.Join(t.TempDir(), "src-"+name)
	if err := copyTree(tmpl, dir); err != nil {
		t.Fatal(err)
	}
	return dir
}

var (
	pluginRepoMu        sync.Mutex
	pluginRepoTemplates = map[string]string{}
)

func TestInstallDoesNotBlockReadersOrDuplicateInstall(t *testing.T) {
	isolateHome(t)
	started := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		once.Do(func() { close(started) })
		<-release
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	defer close(release)
	mgr := NewManager()
	mgr.Init()
	installed := make(chan error, 1)
	go func() { installed <- mgr.Install("slow", srv.URL+"/slow.git") }()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("clone did not reach the test server")
	}
	read := make(chan struct{})
	go func() { mgr.AllPlugins(); close(read) }()
	select {
	case <-read:
	case <-time.After(time.Second):
		t.Error("plugin listing blocked behind git clone")
	}
	duplicate := make(chan error, 1)
	go func() { duplicate <- mgr.Install("slow", srv.URL+"/slow.git") }()
	select {
	case err := <-duplicate:
		if err == nil || !strings.Contains(err.Error(), "install") {
			t.Errorf("duplicate install error = %v", err)
		}
	case <-time.After(time.Second):
		t.Error("duplicate install waited for network instead of being rejected")
	}
	t.Cleanup(func() {
		select {
		case <-installed:
		case <-time.After(5 * time.Second):
			t.Error("install did not finish after test server released")
		}
	})
}

func TestInstallContextStopsGitAndCleansUp(t *testing.T) {
	for _, deadline := range []bool{false, true} {
		name := "cancel"
		if deadline {
			name = "deadline"
		}
		t.Run(name, func(t *testing.T) {
			isolateHome(t)
			started := make(chan struct{})
			release := make(chan struct{})
			var once sync.Once
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				once.Do(func() { close(started) })
				<-release
				w.WriteHeader(http.StatusInternalServerError)
			}))
			defer srv.Close()
			ctx, cancel := context.WithCancel(context.Background())
			if deadline {
				cancel()
				ctx, cancel = context.WithTimeout(context.Background(), 10*time.Second)
			}
			defer cancel()
			mgr := NewManager()
			mgr.Init()
			finished := make(chan error, 1)
			go func() { finished <- mgr.InstallContext(ctx, "slow", srv.URL+"/slow.git") }()
			select {
			case <-started:
			case <-time.After(8 * time.Second):
				close(release)
				t.Fatal("clone did not reach test server")
			}
			wantErr := context.Canceled
			if deadline {
				wantErr = context.DeadlineExceeded
			} else {
				cancel()
			}
			var err error
			select {
			case err = <-finished:
				close(release)
			case <-time.After(15 * time.Second):
				t.Error("git did not stop after context cancellation")
				close(release)
				err = <-finished
			}
			if !errors.Is(err, wantErr) {
				t.Errorf("install error = %v, want %v", err, wantErr)
			}
			if _, err := os.Stat(filepath.Join(mgr.Dir(), "slow")); !os.IsNotExist(err) {
				t.Errorf("cancelled installation left plugin directory: %v", err)
			}
			staging, err := filepath.Glob(filepath.Join(filepath.Dir(mgr.Dir()), ".cove-plugin-install-*"))
			if err != nil || len(staging) != 0 {
				t.Errorf("cancelled installation left staging directories: %v, %v", staging, err)
			}
			if err := mgr.Install("slow", ""); err != nil {
				t.Errorf("cancelled installation retained reservation: %v", err)
			}
		})
	}
}

func TestInstallLockfileFailureDoesNotTrackPlugin(t *testing.T) {
	isolateHome(t)
	source := newPluginRepo(t, "rollback", "1.0.0")
	mgr := NewManager()
	mgr.Init()
	lockPath := mgr.Marketplace().lockfilePath()
	if err := os.Mkdir(lockPath, 0700); err != nil {
		t.Fatal(err)
	}
	if err := mgr.Install("rollback", source); err == nil {
		t.Fatal("install succeeded despite lockfile write failure")
	}
	if _, ok := mgr.Marketplace().LockInfo("rollback"); ok {
		t.Error("failed installation retained an in-memory lock entry")
	}
	if _, err := os.Stat(filepath.Join(mgr.Dir(), "rollback")); !os.IsNotExist(err) {
		t.Errorf("failed installation retained its directory: %v", err)
	}
	if len(mgr.AllPlugins()) != 0 {
		t.Error("failed installation was published")
	}
	if err := os.Remove(lockPath); err != nil {
		t.Fatal(err)
	}
	if err := mgr.Install("rollback", source); err != nil {
		t.Fatalf("retry failed after fixing lockfile: %v", err)
	}
	lock, ok := mgr.Marketplace().LockInfo("rollback")
	if !ok || lock.Version != "1.0.0" || lock.CommitSHA == "" {
		t.Fatal("retry did not persist the installed version and commit")
	}
}

func TestMarketplaceContextStopsGit(t *testing.T) {
	for _, operation := range []string{"install", "refresh", "update", "update-all"} {
		t.Run(operation, func(t *testing.T) {
			isolateHome(t)
			started := make(chan struct{})
			release := make(chan struct{})
			var once sync.Once
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				once.Do(func() { close(started) })
				<-release
				w.WriteHeader(http.StatusInternalServerError)
			}))
			defer srv.Close()
			defer close(release)
			mgr := NewManager()
			mgr.Init()
			url := srv.URL + "/slow.git"
			switch operation {
			case "install":
				mgr.Marketplace().index = []MarketplaceEntry{{Name: "slow", Source: url}}
			case "refresh":
				mgr.Marketplace().sources = []MarketplaceSource{{Name: "slow", Type: "git", URL: url, Enabled: true}}
			default:
				if err := mgr.Install("slow", newPluginRepo(t, "slow", "1.0.0")); err != nil {
					t.Fatal(err)
				}
				runGit(t, filepath.Join(mgr.Dir(), "slow"), "remote", "set-url", "origin", url)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			finished := make(chan error, 1)
			go func() {
				switch operation {
				case "install":
					finished <- mgr.MarketplaceInstallContext(ctx, "slow")
				case "refresh":
					finished <- mgr.MarketplaceRefreshContext(ctx)
				default:
					name := "slow"
					if operation == "update-all" {
						name = ""
					}
					message, err := mgr.MarketplaceUpdateContext(ctx, name)
					if err == nil && strings.Contains(message, context.Canceled.Error()) {
						err = context.Canceled
					}
					finished <- err
				}
			}()
			select {
			case <-started:
			case <-time.After(8 * time.Second):
				t.Fatal("marketplace git did not reach server")
			}
			cancel()
			select {
			case err := <-finished:
				if !errors.Is(err, context.Canceled) {
					t.Errorf("operation error = %v, want cancellation", err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("marketplace operation ignored cancellation")
			}
			if operation == "install" {
				if _, err := os.Stat(filepath.Join(mgr.Dir(), "slow")); !os.IsNotExist(err) {
					t.Error("cancelled marketplace installation left a directory")
				}
				if _, ok := mgr.Marketplace().LockInfo("slow"); ok {
					t.Error("cancelled marketplace installation retained a lock entry")
				}
			}
		})
	}
}

// copyTree copies the directory src to dst, which must not exist.
func copyTree(src, dst string) error {
	return filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(target, data, 0o644)
	})
}

func writeManifestVersion(t *testing.T, dir, name, version string) {
	t.Helper()
	manifest := `{"name":"` + name + `","version":"` + version + `","description":"from repo"}`
	if err := os.WriteFile(filepath.Join(dir, "manifest.json"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
}

func findPlugin(t *testing.T, m *Manager, name string) Entry {
	t.Helper()
	for _, p := range m.AllPlugins() {
		if p.Manifest.Name == name {
			return p
		}
	}
	t.Fatalf("plugin %q not loaded; have %#v", name, m.AllPlugins())
	return Entry{}
}

// TestInstallClonesURLOnUnlistedHost covers `/plugin install demo <url>` where
// the URL is not on one of the few hard-coded hosts and has no .git suffix: a
// self-hosted Gitea/GitLab, or a local path. The URL used to be ignored and an
// empty scaffold created instead, reported as a successful install.
func TestInstallClonesURLOnUnlistedHost(t *testing.T) {
	isolateHome(t)
	src := newPluginRepo(t, "demo", "2.0.0")

	mgr := NewManager()
	mgr.Init()
	if err := mgr.Install("demo", src); err != nil {
		t.Fatalf("Install: %v", err)
	}
	p := findPlugin(t, mgr, "demo")
	if p.Manifest.Version != "2.0.0" || p.Manifest.Description != "from repo" {
		t.Fatalf("installed manifest = %+v, want the repository's (a scaffold was created instead)", p.Manifest)
	}
}

// TestInstallFromGitCanBeUpdated covers `/plugin update` for a plugin
// installed from a git URL. Only marketplace installs wrote a lockfile entry,
// so update answered "not tracked (was it installed via marketplace?)".
func TestInstallFromGitCanBeUpdated(t *testing.T) {
	isolateHome(t)
	src := newPluginRepo(t, "demo", "1.0.0")

	mgr := NewManager()
	mgr.Init()
	if err := mgr.Install("demo", src); err != nil {
		t.Fatalf("Install: %v", err)
	}

	writeManifestVersion(t, src, "demo", "1.1.0")
	runGit(t, src, "commit", "--quiet", "-am", "v1.1.0")

	msg, err := mgr.MarketplaceUpdate("demo")
	if err != nil {
		t.Fatalf("MarketplaceUpdate: %v", err)
	}
	if !strings.Contains(msg, "1.1.0") {
		t.Errorf("update message = %q, want it to report version 1.1.0", msg)
	}
	data, err := os.ReadFile(filepath.Join(mgr.Dir(), "demo", "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"1.1.0"`) {
		t.Errorf("plugin checkout was not updated: manifest = %s", data)
	}
}

// TestUninstallForgetsLockEntry: after uninstalling, the plugin must not stay
// in the lockfile, where `/plugin update <name>` kept finding it and failing
// with "is not a git repo".
func TestUninstallForgetsLockEntry(t *testing.T) {
	isolateHome(t)
	src := newPluginRepo(t, "demo", "1.0.0")

	mgr := NewManager()
	mgr.Init()
	if err := mgr.Install("demo", src); err != nil {
		t.Fatalf("Install: %v", err)
	}
	if _, ok := mgr.Marketplace().LockInfo("demo"); !ok {
		t.Fatal("precondition: install did not record a lock entry")
	}
	if err := mgr.Uninstall("demo"); err != nil {
		t.Fatalf("Uninstall: %v", err)
	}
	if _, err := os.Stat(filepath.Join(mgr.Dir(), "demo")); !os.IsNotExist(err) {
		t.Fatalf("plugin directory (a git clone, read-only pack files on Windows) not removed: %v", err)
	}
	if _, ok := mgr.Marketplace().LockInfo("demo"); ok {
		t.Error("lock entry survived uninstall")
	}
	reloaded := NewMarketplace(mgr.Dir())
	if _, ok := reloaded.LockInfo("demo"); ok {
		t.Error("lock entry survived uninstall on disk")
	}
}

// TestInstallDoesNotPromptForCredentials: a private or mistyped HTTPS repo
// answers 401, and git then asks for a username on the terminal (or opens the
// credential manager's login window). Under cove's TUI that prompt fights the
// UI for the terminal and the install hangs. git must fail instead.
func TestInstallDoesNotPromptForCredentials(t *testing.T) {
	isolateHome(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("WWW-Authenticate", `Basic realm="private"`)
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	mgr := NewManager()
	mgr.Init()
	err := mgr.Install("private", srv.URL+"/team/private.git")
	if err == nil {
		t.Fatal("Install succeeded against a server that only answers 401")
	}
	if !strings.Contains(err.Error(), "terminal prompts disabled") {
		t.Errorf("git was allowed to prompt for credentials: %v", err)
	}
}

// TestMarketplaceInstallDoesNotPromptForCredentials is the same guarantee for
// a marketplace entry whose source is a private repository.
func TestMarketplaceInstallDoesNotPromptForCredentials(t *testing.T) {
	isolateHome(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("WWW-Authenticate", `Basic realm="private"`)
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	mgr := NewManager()
	mgr.Init()
	mgr.Marketplace().index = []MarketplaceEntry{{Name: "private", Source: srv.URL + "/team/private.git"}}
	err := mgr.MarketplaceInstall("private")
	if err == nil {
		t.Fatal("MarketplaceInstall succeeded against a server that only answers 401")
	}
	if !strings.Contains(err.Error(), "terminal prompts disabled") {
		t.Errorf("git was allowed to prompt for credentials: %v", err)
	}
}

// TestInstallChecksOutSymlinksAsPlainFiles: a cloned plugin is untrusted, and
// its skills and commands are read into the model's prompt. A symlink named
// skills/x/SKILL.md pointing at ~/.ssh/id_rsa would have the key read and sent
// to the provider. Symlinks must be checked out as plain files holding the
// link text, whatever the user's git config says.
func TestInstallChecksOutSymlinksAsPlainFiles(t *testing.T) {
	home := isolateHome(t)
	// The user's global config asks for real symlinks.
	if err := os.WriteFile(filepath.Join(home, ".gitconfig"), []byte("[core]\n\tsymlinks = true\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	src := newPluginRepo(t, "linky", "1.0.0")
	// Commit a symlink without needing symlink privileges on this machine.
	cmd := exec.Command("git", "hash-object", "-w", "--stdin")
	cmd.Dir = src
	cmd.Stdin = strings.NewReader("../../../secret")
	blob, err := cmd.Output()
	if err != nil {
		t.Fatalf("hash-object: %v", err)
	}
	runGit(t, src, "update-index", "--add", "--cacheinfo", "120000,"+strings.TrimSpace(string(blob))+",skills/x/SKILL.md")
	runGit(t, src, "commit", "--quiet", "-m", "symlink")

	mgr := NewManager()
	mgr.Init()
	if err := mgr.Install("linky", src); err != nil {
		t.Fatalf("Install: %v", err)
	}
	path := filepath.Join(mgr.Dir(), "linky", "skills", "x", "SKILL.md")
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatalf("Lstat: %v", err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		t.Fatal("the plugin's symlink was checked out as a real symlink")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "../../../secret" {
		t.Errorf("content = %q, want the link text as a plain file", data)
	}
}
