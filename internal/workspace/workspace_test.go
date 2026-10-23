package workspace

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func cleanWorkspace(t *testing.T) string {
	t.Helper()
	ctx := context.Background()
	root := t.TempDir()
	git := func(args ...string) {
		t.Helper()
		if _, err := Git(ctx, root, nil, args...); err != nil {
			t.Fatal(err)
		}
	}
	git("init", "-b", "main")
	git("config", "core.autocrlf", "false")
	if err := os.WriteFile(filepath.Join(root, "value.txt"), []byte("base\n"), 0600); err != nil {
		t.Fatal(err)
	}
	git("add", ".")
	git("-c", "user.name=fixture", "-c", "user.email=fixture@example.invalid", "commit", "-m", "fixture")
	return root
}

func TestIsolationSelectionAndDrift(t *testing.T) {
	ctx := context.Background()
	root := cleanWorkspace(t)
	base, err := Capture(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	work := filepath.Join(t.TempDir(), "candidate")
	if err := base.Add(ctx, work); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := base.Remove(ctx, work); err != nil {
			t.Error(err)
		}
	})
	if err := os.WriteFile(filepath.Join(work, "value.txt"), []byte("candidate\n"), 0600); err != nil {
		t.Fatal(err)
	}
	patch, err := Patch(ctx, work, base.Commit)
	if err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(filepath.Join(root, "value.txt"))
	if string(data) != "base\n" {
		t.Fatalf("root changed before selection: %q", data)
	}
	if err := base.Apply(ctx, patch, "wrong"); err == nil {
		t.Fatal("accepted wrong hash")
	}
	if err := os.WriteFile(filepath.Join(root, "value.txt"), []byte("user edit\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := base.Apply(ctx, patch, Hash(patch)); err == nil {
		t.Fatal("accepted dirty baseline")
	}
	data, _ = os.ReadFile(filepath.Join(root, "value.txt"))
	if string(data) != "user edit\n" {
		t.Fatal("overwrote user edit")
	}
	if err := os.WriteFile(filepath.Join(root, "value.txt"), []byte("base\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := base.Apply(ctx, patch, Hash(patch)); err != nil {
		t.Fatal(err)
	}
	data, _ = os.ReadFile(filepath.Join(root, "value.txt"))
	if string(data) != "candidate\n" {
		t.Fatalf("selection did not apply: %q", data)
	}
}

func TestRefuseNoGitAndUnsafePaths(t *testing.T) {
	if _, err := Capture(context.Background(), t.TempDir()); err == nil {
		t.Fatal("faked Git isolation")
	}
	for _, path := range []string{"../escape", "a/../escape", "/absolute", "C:/escape", "a\\b", ".git/config", "a/.GIT/config"} {
		if safePath(path) {
			t.Errorf("unsafe path accepted: %q", path)
		}
	}
}

func TestHashRejectedBeforeWorkspaceAccess(t *testing.T) {
	err := (Baseline{Root: filepath.Join(t.TempDir(), "nonexistent")}).Apply(context.Background(), []byte("candidate patch"), "wrong-hash")
	if err == nil || !strings.Contains(err.Error(), "patch hash mismatch") {
		t.Fatalf("hash protection bypassed before workspace access: %v", err)
	}
}

func TestResolvePathWithMissingDirectories(t *testing.T) {
	root := t.TempDir()
	want := filepath.Join(root, "missing", "child")
	actual, err := ResolvePath(want)
	if err != nil || actual != want {
		t.Fatalf("resolve missing path: %s %v", actual, err)
	}
	if _, err := os.Stat(filepath.Join(root, "missing")); !os.IsNotExist(err) {
		t.Fatal("resolver created directories")
	}
}

func TestCaptureRejectsUntrackedIgnoredAndSubdirectory(t *testing.T) {
	ctx := context.Background()
	root := cleanWorkspace(t)
	if err := os.WriteFile(filepath.Join(root, ".git", "info", "exclude"), []byte("ignored.txt\n"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"untracked.txt", "ignored.txt"} {
		path := filepath.Join(root, name)
		if err := os.WriteFile(path, []byte("user content"), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := Capture(ctx, root); err == nil || !strings.Contains(err.Error(), "clean repository") {
			t.Fatalf("accepted dirty root with %s: %v", name, err)
		}
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
	}
	subdirectory := filepath.Join(root, "subdirectory")
	if err := os.Mkdir(subdirectory, 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := Capture(ctx, subdirectory); err == nil {
		t.Fatal("accepted non-root scope")
	}
}

func TestPatchRejectsIgnoredCandidateAndIncludesUntracked(t *testing.T) {
	ctx := context.Background()
	root := cleanWorkspace(t)
	baseline, err := Capture(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	work := filepath.Join(t.TempDir(), "candidate")
	if err := baseline.Add(ctx, work); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := baseline.Remove(ctx, work); err != nil {
			t.Error(err)
		}
	}()
	if err := os.WriteFile(filepath.Join(work, ".gitignore"), []byte("ignored.txt\n"), 0600); err != nil {
		t.Fatal(err)
	}
	ignored := filepath.Join(work, "ignored.txt")
	if err := os.WriteFile(ignored, []byte("verifier input absent from patch"), 0600); err != nil {
		t.Fatal(err)
	}
	// Ignored build artefacts are removed from the worktree before the capture
	// (they can never be in the patch, and a verifier must not see them), so
	// the candidate is captured without them rather than refused.
	if patch, err := Patch(ctx, work, baseline.Commit); err != nil || strings.Contains(string(patch), "b/ignored.txt") {
		t.Fatalf("ignored artefact handling: %v %s", err, patch)
	}
	if _, err := os.Stat(ignored); !os.IsNotExist(err) {
		t.Fatalf("ignored artefact survived the capture: %v", err)
	}
	if err := os.WriteFile(filepath.Join(work, "new.txt"), []byte("new candidate file\n"), 0600); err != nil {
		t.Fatal(err)
	}
	patch, err := Patch(ctx, work, baseline.Commit)
	if err != nil {
		t.Fatal(err)
	}
	if err := baseline.Apply(ctx, patch, Hash(patch)); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(root, "new.txt"))
	if err != nil || string(data) != "new candidate file\n" {
		t.Fatalf("untracked candidate omitted from patch: %q %v", data, err)
	}
}

func TestDescribeIgnoresDirtyTreeButMatchesCapture(t *testing.T) {
	ctx := context.Background()
	root := cleanWorkspace(t)
	clean, err := Capture(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "dirty.txt"), []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Capture(ctx, root); err == nil {
		t.Fatal("Capture must refuse a dirty tree")
	}
	described, err := Describe(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	if described != clean {
		t.Fatalf("Describe = %+v, Capture = %+v", described, clean)
	}
}

func TestDescribeRefusesSubdirectoryAndDetachedHead(t *testing.T) {
	ctx := context.Background()
	root := cleanWorkspace(t)
	sub := filepath.Join(root, "sub")
	if err := os.MkdirAll(sub, 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := Describe(ctx, sub); err == nil {
		t.Fatal("subdirectory accepted")
	}
	if _, err := Git(ctx, root, nil, "checkout", "--detach"); err != nil {
		t.Fatal(err)
	}
	if _, err := Describe(ctx, root); err == nil {
		t.Fatal("detached HEAD accepted")
	}
}

// TestPatchIgnoresUserDiffConfig pins the patch format against the user's
// global git config. workspace.Git drops every GIT_* variable (so
// GIT_CONFIG_GLOBAL cannot be used here); the global file is redirected
// through HOME/USERPROFILE/XDG_CONFIG_HOME instead.
func TestPatchIgnoresUserDiffConfig(t *testing.T) {
	home := t.TempDir()
	config := "[color]\n\tui = always\n\tdiff = always\n[diff]\n\tnoprefix = true\n\tmnemonicPrefix = true\n"
	if err := os.WriteFile(filepath.Join(home, ".gitconfig"), []byte(config), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(home, ".gitconfig"))

	ctx := context.Background()
	root := cleanWorkspace(t)
	// The redirect must really be in effect, or the test proves nothing.
	if got, err := Git(ctx, root, nil, "config", "--get", "color.ui"); err != nil || strings.TrimSpace(string(got)) != "always" {
		t.Fatalf("global config not picked up: %q %v", got, err)
	}
	baseline, err := Capture(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	work := filepath.Join(t.TempDir(), "candidate")
	if err := baseline.Add(ctx, work); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := baseline.Remove(ctx, work); err != nil {
			t.Error(err)
		}
	}()
	if err := os.WriteFile(filepath.Join(work, "value.txt"), []byte("changed\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(work, "new.txt"), []byte("new candidate file\n"), 0600); err != nil {
		t.Fatal(err)
	}
	patch, err := Patch(ctx, work, baseline.Commit)
	if err != nil {
		t.Fatal(err)
	}
	text := string(patch)
	if strings.Contains(text, "\x1b[") {
		t.Fatalf("patch carries ANSI colour codes: %q", text)
	}
	if !strings.Contains(text, "diff --git a/value.txt b/value.txt") || !strings.Contains(text, "+++ b/new.txt") {
		t.Fatalf("patch prefixes not pinned to a/ b/: %s", text)
	}
	if err := baseline.Apply(ctx, patch, Hash(patch)); err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(filepath.Join(root, "value.txt")); err != nil || string(data) != "changed\n" {
		t.Fatalf("patch not applied: %q %v", data, err)
	}
}
