package checkpoint

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/liuzhixin405/cove-agent/internal/log"
)

// Manager handles file system checkpoints using a git shadow store.
type Manager struct {
	mu       sync.Mutex
	storeDir string // ~/.cove/checkpoints/store (shared git repo)
	refName  string // refs/cove/<hash(workdir)>
	workDir  string
	count    int
	// created counts checkpoints created by this manager, for the periodic
	// trim and gc; the rest is the background maintenance state (all under mu
	// except maintWG).
	created     int
	pendingTrim bool
	pendingGC   bool
	maintaining bool
	// maintDone is closed when the current maintenance goroutine ends.
	maintDone chan struct{}
}

// New creates a checkpoint manager for the given working directory.
func New(workDir string) (*Manager, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	storeDir := filepath.Join(home, ".cove", "checkpoints", "store")

	// Initialize bare git repo if not exists
	if _, err := os.Stat(filepath.Join(storeDir, "HEAD")); os.IsNotExist(err) {
		if err := os.MkdirAll(storeDir, 0700); err != nil {
			return nil, err
		}
		cmd := exec.Command("git", "init", "--bare", storeDir)
		if out, err := cmd.CombinedOutput(); err != nil {
			return nil, fmt.Errorf("git init failed: %s: %w", out, err)
		}
	}

	// Install the exclude rules and the byte-exact attributes into the shadow
	// store. See excludePatterns and storeAttributes.
	if err := writeStoreExcludes(storeDir); err != nil {
		return nil, err
	}
	if err := writeStoreAttributes(storeDir); err != nil {
		return nil, err
	}

	// Compute ref name from workdir hash
	h := sha256.Sum256([]byte(workDir))
	refName := "refs/cove/" + hex.EncodeToString(h[:8])

	return &Manager{
		storeDir: storeDir,
		refName:  refName,
		workDir:  workDir,
	}, nil
}

// excludePatterns lists what a checkpoint must never capture.
//
// These are applied through the shadow store's own info/exclude file, not on
// the `git add` command line. `git add --all --exclude=<p>` is not valid git:
// --exclude belongs to `git ls-files`, so every Create failed outright and silently
// fell back to the unfiltered `git add -A` in the error branch — which is why
// node_modules/ and target/ ended up inside the checkpoints all along.
var excludePatterns = []string{
	"node_modules", ".git", ".venv", "__pycache__",
	"*.exe", "*.dll", "*.so", "*.dylib",
	"target/", "dist/", "build/", ".next/",
}

// writeStoreExcludes renders excludePatterns into <storeDir>/info/exclude,
// which git applies to every add in this repository. The store is dedicated to
// checkpoints, so owning that file outright is safe.
func writeStoreExcludes(storeDir string) error {
	dir := filepath.Join(storeDir, "info")
	if err := os.MkdirAll(dir, 0700); err != nil {
		return fmt.Errorf("checkpoint store info dir: %w", err)
	}
	var sb strings.Builder
	sb.WriteString("# Managed by cove — regenerated on startup.\n")
	for _, p := range excludePatterns {
		sb.WriteString(p)
		sb.WriteByte('\n')
	}
	if err := os.WriteFile(filepath.Join(dir, "exclude"), []byte(sb.String()), 0600); err != nil {
		return fmt.Errorf("checkpoint store excludes: %w", err)
	}
	return nil
}

// storeAttributes makes every path in the shadow store opaque bytes.
//
// info/attributes outranks the project's own .gitattributes, so a project
// asking for `text eol=crlf` or an LFS filter cannot reshape what a checkpoint
// stores or what /undo writes back. Before this, attributes (and the user's
// config, see storeConfig) applied: /undo rewrote LF files as CRLF under Git for
// Windows' default core.autocrlf=true.
const storeAttributes = "# Managed by cove — regenerated on startup.\n* -text -filter -diff -merge -ident -working-tree-encoding\n"

func writeStoreAttributes(storeDir string) error {
	if err := os.WriteFile(filepath.Join(storeDir, "info", "attributes"), []byte(storeAttributes), 0600); err != nil {
		return fmt.Errorf("checkpoint store attributes: %w", err)
	}
	return nil
}

// storeConfig is prepended to every git invocation on the shadow store. The
// store inherited the user's global and system config, so core.autocrlf,
// core.eol or an LFS filter changed the bytes /undo wrote back, and the default
// core.quotePath=true turned non-ASCII names in parsed output into
// "\344\270\255..." strings that matched no file. None of that is a user
// preference about cove's private store.
var storeConfig = []string{
	"-c", "core.autocrlf=false",
	"-c", "core.eol=lf",
	"-c", "core.safecrlf=false",
	"-c", "core.quotePath=false",
	"-c", "core.precomposeUnicode=true",
	"-c", "filter.lfs.smudge=",
	"-c", "filter.lfs.clean=",
	"-c", "filter.lfs.process=",
	"-c", "filter.lfs.required=false",
	"-c", "advice.addEmbeddedRepo=false",
}

// Create snapshots the working tree and returns the checkpoint's commit hash.
// When nothing changed since the last checkpoint it returns that one instead
// of adding an identical entry.
// WorkDir is the project directory the manager snapshots.
func (m *Manager) WorkDir() string { return m.workDir }

func (m *Manager) Create(label string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if label == "" {
		label = fmt.Sprintf("checkpoint-%d", m.count+1)
	}
	hash, created, err := m.snapshot(m.refName, label)
	if err != nil {
		return "", err
	}
	if created {
		m.count++
		// Trim and gc run in the background (retention.go); a trim rewrites
		// the kept commits, so this hash may be replaced by then.
		m.scheduleMaintenanceLocked()
	}
	return hash, nil
}

// snapshot commits the current working tree onto ref and reports whether a new
// commit was needed.
//
// The commit is built with write-tree/commit-tree and chained on this
// project's own ref. `git commit` would have advanced the shared store's HEAD,
// so every project's checkpoint became the parent of the next one in any
// project: /checkpoints listed other projects' snapshots and /undo <hash>
// would check one of them out into this directory.
func (m *Manager) snapshot(ref, label string) (string, bool, error) {
	env := m.env()
	// The exclusions live in the store's info/exclude (written by
	// writeStoreExcludes), and git also honors the project's own .gitignore.
	if err := m.addAll(env); err != nil {
		return "", false, fmt.Errorf("git add failed: %w", err)
	}
	tree, err := m.gitOutput(env, "write-tree")
	if err != nil {
		return "", false, fmt.Errorf("git write-tree failed: %w", err)
	}
	tree = strings.TrimSpace(tree)

	parent := m.getRef(env, ref)
	if parent != "" && m.treeOf(parent) == tree {
		return parent, false, nil
	}

	msg := fmt.Sprintf("[cove] %s (%s)", label, time.Now().Format("15:04:05"))
	// The store is cove's own; commits need an identity even on a machine where
	// the user never configured one, or no checkpoint is ever created.
	args := []string{"-c", "user.name=cove", "-c", "user.email=cove@localhost", "commit-tree", tree, "-m", msg}
	if parent != "" {
		args = append(args, "-p", parent)
	}
	hash, err := m.gitOutput(env, args...)
	if err != nil {
		return "", false, fmt.Errorf("git commit-tree failed: %w", err)
	}
	hash = strings.TrimSpace(hash)
	if err := m.gitCmd(env, "update-ref", ref, hash); err != nil {
		return "", false, err
	}
	return hash, true, nil
}

// Restore rolls the working directory back to a checkpoint and returns the
// hash of a backup taken just before, so the rollback itself can be undone
// with Restore(backup).
//
// With an empty hash it goes to the newest checkpoint that differs from the
// current state, so repeated calls keep stepping back instead of restoring the
// same snapshot again.
func (m *Manager) Restore(commitHash string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	env := m.env()
	backupRef := m.refName + "-undo"
	backup, backedUp, err := m.snapshot(backupRef, "before undo")
	if err != nil {
		return "", fmt.Errorf("备份当前状态失败: %w", err)
	}
	if backedUp {
		// The undo-backup chain is kept to the same length as checkpoints.
		if trimmed, err := m.trimLocked(backupRef); err != nil {
			log.Warnf("[checkpoint] trim undo backups: %v", err)
		} else if trimmed != "" {
			backup = trimmed
		}
	}
	current := m.treeOf(backup)

	target := ""
	if commitHash == "" {
		out, err := m.gitOutput(env, "rev-list", m.refName)
		if err != nil {
			return "", fmt.Errorf("无可用检查点")
		}
		for _, h := range strings.Fields(out) {
			if m.treeOf(h) != current {
				target = h
				break
			}
		}
		if target == "" {
			return "", fmt.Errorf("没有与当前状态不同的检查点")
		}
	} else {
		full, err := m.gitOutput(env, "rev-parse", "--verify", "--quiet", commitHash+"^{commit}")
		if err != nil {
			return "", fmt.Errorf("检查点 %s 不存在", commitHash)
		}
		target = strings.TrimSpace(full)
		if !m.isAncestor(target, m.refName) && !m.isAncestor(target, backupRef) {
			return "", fmt.Errorf("检查点 %s 不属于当前项目", commitHash)
		}
	}

	// Paths that exist now but not in the target have to be deleted, not left
	// in place: `checkout <hash> -- .` only writes out what the commit
	// contains. The backup holds every one of them, so this is reversible.
	//
	// The list is read NUL-separated with modes (--raw -z): the newline form
	// quoted non-ASCII names, which then matched no file, and gave no way to
	// tell a nested repository (gitlink) from a file.
	out, err := m.gitOutput(env, "diff", "--raw", "-z", "--no-renames", "--diff-filter=A", target, backup)
	if err != nil {
		return "", err
	}
	added, err := parseRawAdded(out)
	if err != nil {
		return "", err
	}
	// From here on the working tree is being rewritten, so the backup is
	// returned with every error: it is the only way back.
	//
	// A checkpoint of an empty tree (taken in a fresh project before its
	// first file was written) has nothing to check out: `checkout <hash>
	// -- .` then fails with "pathspec '.' did not match any file(s)", and the
	// restore is the deletions below alone.
	if names, lsErr := m.gitOutput(env, "ls-tree", "-r", "--name-only", target); lsErr != nil || strings.TrimSpace(names) != "" {
		if err := m.gitCmd(env, "checkout", target, "--", "."); err != nil {
			return backup, err
		}
	}
	// Per-path failures are collected rather than returned on the first one:
	// the checkout above has already rewritten the tree, and stopping midway
	// left the remaining new files in place on top of the old state.
	var errs []error
	var nested []string
	root := filepath.Clean(m.workDir)
	for _, a := range added {
		// Guard against a path escaping the working directory (a maliciously
		// crafted commit, or a stray absolute path in the diff output).
		full := filepath.Join(root, filepath.FromSlash(a.path))
		if !strings.HasPrefix(full, root+string(os.PathSeparator)) {
			continue
		}
		if a.mode == gitlinkMode {
			// A nested repository is recorded as a bare commit id, its files
			// are in no checkpoint: removing it would lose them for good.
			nested = append(nested, a.path)
			continue
		}
		if err := os.Remove(full); err != nil && !os.IsNotExist(err) {
			errs = append(errs, fmt.Errorf("删除 %s 失败: %w", a.path, err))
			continue
		}
		pruneEmptyParents(root, filepath.Dir(full))
	}
	if len(nested) > 0 {
		errs = append(errs, fmt.Errorf("嵌套 git 仓库未纳入检查点，已跳过（保留原样）: %s", strings.Join(nested, ", ")))
	}
	return backup, errors.Join(errs...)
}

// gitlinkMode is the tree mode of a nested repository (a submodule entry).
const gitlinkMode = "160000"

type addedPath struct {
	mode string // new mode
	path string
}

// parseRawAdded reads `git diff --raw -z` output: each entry is
// ":<old mode> <new mode> <old id> <new id> <status>\0<path>\0".
func parseRawAdded(out string) ([]addedPath, error) {
	fields := strings.Split(out, "\x00")
	var res []addedPath
	for i := 0; i < len(fields); i++ {
		meta := fields[i]
		if meta == "" {
			continue
		}
		parts := strings.Fields(strings.TrimPrefix(meta, ":"))
		if !strings.HasPrefix(meta, ":") || len(parts) < 5 || i+1 >= len(fields) {
			return nil, fmt.Errorf("restore: unexpected git diff output %q", meta)
		}
		i++
		res = append(res, addedPath{mode: parts[1], path: fields[i]})
	}
	return res, nil
}

// pruneEmptyParents removes dir and its ancestors below root while they are
// empty. They were left behind before; only directories that end up empty go
// (os.Remove refuses anything else), so ignored or uncaptured files inside a
// new directory — which no checkpoint holds — are never deleted.
func pruneEmptyParents(root, dir string) {
	for dir != root && strings.HasPrefix(dir, root+string(os.PathSeparator)) {
		if os.Remove(dir) != nil {
			return
		}
		dir = filepath.Dir(dir)
	}
}

// List returns available checkpoints (most recent first).
func (m *Manager) List() []string {
	out, err := m.gitOutput(m.env(), "log", m.refName, "--oneline", "-20")
	if err != nil {
		return nil
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	var result []string
	for _, l := range lines {
		if l != "" {
			result = append(result, l)
		}
	}
	return result
}

// Count returns how many checkpoints have been made this session.
func (m *Manager) Count() int {
	// count is incremented under m.mu by Create, which runs on background
	// turn-end goroutines while the UI reads this for its status line.
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.count
}

// env points git at the shadow store, this project's work tree and this
// project's own index file, so no real repository and no other project is
// touched.
func (m *Manager) env() []string {
	return []string{
		"GIT_DIR=" + m.storeDir,
		"GIT_WORK_TREE=" + m.workDir,
		"GIT_INDEX_FILE=" + filepath.Join(m.storeDir, "index-"+m.refName[len("refs/cove/"):]),
	}
}

func (m *Manager) getRef(env []string, ref string) string {
	out, err := m.gitOutput(env, "rev-parse", "--verify", "--quiet", ref)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(out)
}

func (m *Manager) treeOf(commit string) string {
	out, err := m.gitOutput(m.env(), "rev-parse", commit+"^{tree}")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(out)
}

func (m *Manager) isAncestor(commit, ref string) bool {
	if m.getRef(m.env(), ref) == "" {
		return false
	}
	return m.gitCmd(m.env(), "merge-base", "--is-ancestor", commit, ref) == nil
}

// noCommitRepoRe finds the nested repositories git refuses to add because
// they have no commit yet ("error: 'sub/' does not have a commit checked out").
var noCommitRepoRe = regexp.MustCompile(`'([^']+)' does not have a commit checked out`)

// maxNestedExcludes bounds the retries of addAll, one per batch of nested
// repositories reported.
const maxNestedExcludes = 8

// addAll stages the work tree. A nested repository created with git init and
// no commit yet (right after "git init" in a subdirectory) made "git add
// --all" fail outright, so no checkpoint could be taken and /undo refused to
// run for want of its backup. Such a repository is left out of the snapshot,
// as nested repositories with commits already are in effect (they are stored
// as a bare gitlink, their content never captured).
func (m *Manager) addAll(env []string) error {
	args := []string{"add", "--all"}
	var excluded []string
	for i := 0; ; i++ {
		err := m.gitCmd(env, args...)
		if err == nil {
			return nil
		}
		found := noCommitRepoRe.FindAllStringSubmatch(err.Error(), -1)
		if len(found) == 0 || i >= maxNestedExcludes {
			return err
		}
		for _, f := range found {
			excluded = append(excluded, ":(exclude)"+f[1])
		}
		args = append([]string{"add", "--all", "--", "."}, excluded...)
	}
}

func (m *Manager) gitCmd(env []string, args ...string) error {
	_, err := m.gitOutput(env, args...)
	return err
}

// gitOutput runs git on the shadow store with storeConfig and returns stdout
// only. stderr used to be mixed in, so a warning git printed (a CRLF notice,
// an embedded-repo hint) became part of the output parsed as paths or hashes.
func (m *Manager) gitOutput(env []string, args ...string) (string, error) {
	cmd := exec.Command("git", append(append([]string(nil), storeConfig...), args...)...)
	cmd.Env = append(os.Environ(), env...)
	cmd.Dir = m.workDir
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("%w: %s", err, strings.TrimSpace(stdout.String()+stderr.String()))
	}
	return stdout.String(), nil
}
