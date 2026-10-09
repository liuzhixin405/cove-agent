package workspace

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

type Baseline struct {
	Root   string `json:"root"`
	Commit string `json:"commit"`
	Branch string `json:"branch"`
}

func Git(ctx context.Context, dir string, input []byte, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...)
	cmd.WaitDelay = 2 * time.Second
	for _, variable := range os.Environ() {
		key, _, _ := strings.Cut(variable, "=")
		if !strings.HasPrefix(strings.ToUpper(key), "GIT_") {
			cmd.Env = append(cmd.Env, variable)
		}
	}
	cmd.Env = append(cmd.Env, "GIT_TERMINAL_PROMPT=0", "GIT_CONFIG_COUNT=0")
	if input != nil {
		cmd.Stdin = bytes.NewReader(input)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	output, err := cmd.Output()
	if err != nil {
		if ctx.Err() != nil {
			err = ctx.Err()
		}
		return nil, fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, stderr.String())
	}
	return output, nil
}

func ResolvePath(path string) (string, error) {
	current, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	var missing []string
	for {
		resolved, err := filepath.EvalSymlinks(current)
		if err == nil {
			for index := len(missing) - 1; index >= 0; index-- {
				resolved = filepath.Join(resolved, missing[index])
			}
			return resolved, nil
		}
		if !errors.Is(err, os.ErrNotExist) || filepath.Dir(current) == current {
			return "", err
		}
		missing = append(missing, filepath.Base(current))
		current = filepath.Dir(current)
	}
}

func Capture(ctx context.Context, dir string) (Baseline, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return Baseline{}, err
	}
	abs, err = filepath.EvalSymlinks(abs)
	if err != nil {
		return Baseline{}, err
	}
	if _, err := os.Lstat(filepath.Join(abs, ".git")); err != nil {
		return Baseline{}, errors.New("race requires a local Git project root with .git; isolation cannot be simulated")
	}
	root, err := Git(ctx, abs, nil, "rev-parse", "--show-toplevel")
	if err != nil {
		return Baseline{}, err
	}
	actual, err := filepath.EvalSymlinks(filepath.FromSlash(strings.TrimSpace(string(root))))
	if err != nil || !strings.EqualFold(actual, abs) {
		return Baseline{}, errors.New("race requires the Git project root (not a subdirectory)")
	}
	status, err := Git(ctx, abs, nil, "status", "--porcelain=v1", "--untracked-files=all", "--ignored=matching")
	if err != nil {
		return Baseline{}, err
	}
	if len(status) != 0 {
		return Baseline{}, errors.New("race requires a clean repository, including untracked/ignored files; user changes are not omitted or overwritten")
	}
	commit, err := Git(ctx, abs, nil, "rev-parse", "HEAD")
	if err != nil {
		return Baseline{}, err
	}
	branch, err := Git(ctx, abs, nil, "symbolic-ref", "--quiet", "HEAD")
	if err != nil {
		if ctx.Err() != nil {
			// A cancelled or timed-out git call is not a detached HEAD; keep
			// the context error so callers (and the timeout report) see it.
			return Baseline{}, err
		}
		return Baseline{}, errors.New("race requires a target branch, not detached HEAD")
	}
	return Baseline{abs, strings.TrimSpace(string(commit)), strings.TrimSpace(string(branch))}, nil
}

func (b Baseline) Check(ctx context.Context) error {
	now, err := Capture(ctx, b.Root)
	if err != nil {
		return err
	}
	if now != b {
		return errors.New("target branch/base commit drift; selection refused")
	}
	return nil
}

func (b Baseline) Add(ctx context.Context, path string) error {
	_, err := Git(ctx, b.Root, nil, "-c", "core.hooksPath=", "worktree", "add", "--detach", "--", path, b.Commit)
	return err
}

func (b Baseline) Remove(ctx context.Context, path string) error {
	_, err := Git(ctx, b.Root, nil, "worktree", "remove", "--force", "--", path)
	return err
}

func safePath(path string) bool {
	if path == "" || strings.ContainsAny(path, "\\:\x00\r\n") || strings.HasPrefix(path, "/") {
		return false
	}
	for _, part := range strings.Split(path, "/") {
		if part == "" || part == "." || part == ".." || strings.EqualFold(part, ".git") {
			return false
		}
	}
	return true
}

func Patch(ctx context.Context, dir, commit string) ([]byte, error) {
	// Build caches and test artefacts (__pycache__, node_modules, bin/) are
	// ignored files the generation or the verifier leaves behind in the fresh
	// worktree; they can never be part of the patch, so remove them before the
	// capture instead of failing every Python/Node/.NET candidate. Nested Git
	// repositories survive `clean` without -ff and are still reported below.
	if _, err := Git(ctx, dir, nil, "clean", "-fdX", "-q", "--", "."); err != nil {
		return nil, err
	}
	ignored, err := Git(ctx, dir, nil, "ls-files", "--others", "--ignored", "--exclude-standard", "-z")
	if err != nil {
		return nil, err
	}
	if len(ignored) != 0 {
		return nil, errors.New("candidate contains ignored files that cannot be included in the verified patch")
	}
	if _, err := Git(ctx, dir, nil, "add", "-A", "--", "."); err != nil {
		return nil, err
	}
	entries, err := Git(ctx, dir, nil, "ls-files", "--stage", "-z")
	if err != nil {
		return nil, err
	}
	for _, entry := range strings.Split(string(entries), "\x00") {
		if entry == "" {
			continue
		}
		meta, path, ok := strings.Cut(entry, "\t")
		if !ok || !safePath(path) || (!strings.HasPrefix(meta, "100644 ") && !strings.HasPrefix(meta, "100755 ")) {
			return nil, fmt.Errorf("unsafe candidate path or mode: %q", path)
		}
	}
	return Git(ctx, dir, nil, "diff", "--cached", "--binary", "--no-ext-diff", "--no-textconv", "--full-index", commit, "--", ".")
}

func Hash(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func (b Baseline) Apply(ctx context.Context, patch []byte, hash string) error {
	if hash == "" || Hash(patch) != hash {
		return errors.New("patch hash mismatch")
	}
	if len(patch) == 0 {
		return errors.New("candidate has no patch")
	}
	if err := b.Check(ctx); err != nil {
		return err
	}
	if _, err := Git(ctx, b.Root, patch, "apply", "--check", "--whitespace=nowarn", "-"); err != nil {
		return err
	}
	if err := b.Check(ctx); err != nil {
		return err
	}
	// The final apply must not be killed half-way by a Ctrl+C or shutdown
	// cancellation: git apply is atomic against hunk failures but not against
	// being killed, and a partially applied patch leaves the project dirty with
	// nothing to roll back to. Detach from the cancellable context and keep an
	// independent bound instead.
	applyCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 60*time.Second)
	defer cancel()
	if _, err := Git(applyCtx, b.Root, patch, "apply", "--whitespace=nowarn", "-"); err != nil {
		return fmt.Errorf("%w; inspect `git status` before retrying", err)
	}
	return nil
}
