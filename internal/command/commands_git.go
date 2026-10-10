package command

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// gitOutput runs git in dir and returns its stdout. A failure carries git's
// own stderr: the commands used to discard it, so outside a repository (or
// without git on PATH) /commit, /review and /diff all reported "no changes".
func gitOutput(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) && len(strings.TrimSpace(string(ee.Stderr))) > 0 {
			return string(out), fmt.Errorf("git %s: %s", args[0], strings.TrimSpace(string(ee.Stderr)))
		}
		return string(out), fmt.Errorf("git %s: %w", args[0], err)
	}
	return string(out), nil
}

func (c *CommitCmd) Name() string { return "commit" }

func (c *CommitCmd) MutatesEngine([]string) bool { return true }
func (c *CommitCmd) Aliases() []string           { return nil }
func (c *CommitCmd) Description() string {
	return "预览并验证提交；默认仅提交已暂存内容"
}
func (c *CommitCmd) Help() string {
	return "/commit [--preview] [--all | --only <路径>... --] [消息] - 默认仅提交已暂存内容；不推送"
}
func (c *CommitCmd) Execute(ctx context.Context, in Input) (Output, error) {
	args := in.Args
	preview := len(args) > 0 && args[0] == "--preview"
	if preview {
		args = args[1:]
	}
	all := len(args) > 0 && args[0] == "--all"
	var paths []string
	if all {
		args = args[1:]
	} else if len(args) > 0 && args[0] == "--only" {
		separator := -1
		for index, arg := range args[1:] {
			if arg == "--" {
				separator = index + 1
				break
			}
		}
		if separator < 2 {
			return Output{}, fmt.Errorf("用法: /commit --only <路径>... -- [消息]")
		}
		paths, args = append([]string(nil), args[1:separator]...), args[separator+1:]
	}
	if len(args) > 0 && strings.HasPrefix(args[0], "--") {
		return Output{}, fmt.Errorf("未知提交参数: %s", args[0])
	}
	root, err := gitOutput(ctx, in.Cwd, "rev-parse", "--show-toplevel")
	if err != nil {
		return Output{}, fmt.Errorf("无法读取 git 状态: %w", err)
	}
	root = strings.TrimSpace(root)
	so, err := gitOutput(ctx, root, "status", "--porcelain")
	if err != nil {
		return Output{}, err
	}
	if strings.TrimSpace(so) == "" {
		return Output{Message: "没有可提交的更改"}, nil
	}
	for index, path := range paths {
		absolute := path
		if !filepath.IsAbs(path) {
			absolute = filepath.Join(in.Cwd, path)
		}
		relative, err := filepath.Rel(root, absolute)
		if path == "" || err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
			return Output{}, fmt.Errorf("提交路径必须在当前仓库内: %s", path)
		}
		paths[index] = filepath.ToSlash(relative)
	}
	msg := "auto-commit"
	if len(args) > 0 {
		msg = strings.Join(args, " ")
	}
	indexFile := ""
	run := func(arguments ...string) (string, error) {
		cmd := exec.CommandContext(ctx, "git", append([]string{"--literal-pathspecs"}, arguments...)...)
		cmd.Dir = root
		if indexFile != "" {
			cmd.Env = append(os.Environ(), "GIT_INDEX_FILE="+indexFile)
		}
		output, err := cmd.CombinedOutput()
		if err != nil {
			return string(output), fmt.Errorf("git %s 失败: %s: %w", arguments[0], strings.TrimSpace(string(output)), err)
		}
		return string(output), nil
	}
	if all || len(paths) > 0 {
		file, err := os.CreateTemp("", "cove-commit-index-*")
		if err != nil {
			return Output{}, err
		}
		indexFile = file.Name()
		defer os.Remove(indexFile)
		defer os.Remove(indexFile + ".lock")
		if err := file.Close(); err != nil {
			return Output{}, err
		}
		if err := os.Remove(indexFile); err != nil {
			return Output{}, err
		}
		tree := "--empty"
		if _, err := gitOutput(ctx, root, "rev-parse", "--verify", "HEAD"); err == nil {
			tree = "HEAD"
		}
		if _, err := run("read-tree", tree); err != nil {
			return Output{}, err
		}
		add := []string{"add", "-A"}
		if len(paths) > 0 {
			add = append(append(add, "--"), paths...)
		}
		if _, err := run(add...); err != nil {
			return Output{}, err
		}
	}
	staged, err := run("diff", "--cached", "--stat")
	if err != nil {
		return Output{}, err
	}
	if strings.TrimSpace(staged) == "" {
		return Output{}, fmt.Errorf("没有已暂存的更改；请先选择文件暂存，或明确使用 /commit --all [消息]")
	}
	files, err := run("diff", "--cached", "--name-only", "-z")
	if err != nil {
		return Output{}, err
	}
	selected := strings.Split(strings.TrimSuffix(files, "\x00"), "\x00")
	if preview {
		return Output{Message: "拟提交文件（未创建提交）:\n" + staged}, nil
	}
	if in.CommitPreview != nil {
		in.CommitPreview("拟提交文件:\n" + staged)
	}
	if _, err := run("diff", "--cached", "--check"); err != nil {
		return Output{}, err
	}
	tree, err := run("write-tree")
	if err != nil {
		return Output{}, err
	}
	if verifier, ok := in.Engine.(CommitVerifier); ok {
		check := func() error {
			changed, err := run(append([]string{"diff", "--name-only", "--"}, selected...)...)
			if err != nil {
				return err
			}
			if strings.TrimSpace(changed) != "" {
				return fmt.Errorf("拟提交内容与工作区不同，请重新暂存或使用 --only/--all: %s", changed)
			}
			return nil
		}
		if err := check(); err != nil {
			return Output{}, err
		}
		if err := verifier.VerifyCommit(ctx, selected); err != nil {
			return Output{}, fmt.Errorf("提交前验证未通过: %w", err)
		}
		if err := check(); err != nil {
			return Output{}, fmt.Errorf("验证期间文件发生变化，未提交: %w", err)
		}
	}
	verifiedTree, err := run("write-tree")
	if err != nil {
		return Output{}, err
	}
	if verifiedTree != tree {
		return Output{}, fmt.Errorf("验证期间暂存区发生变化，未提交")
	}
	if err := ctx.Err(); err != nil {
		return Output{}, err
	}
	co, err := run("commit", "-m", msg)
	if err != nil {
		return Output{}, fmt.Errorf("提交失败（原暂存区保留）: %w", err)
	}
	if indexFile != "" {
		committed, err := gitOutput(ctx, root, "diff-tree", "--root", "--no-commit-id", "--name-only", "-r", "-z", "HEAD")
		if err != nil {
			return Output{}, fmt.Errorf("已创建提交，但无法读取提交文件，请勿重试提交: %w", err)
		}
		if committed != "" {
			names := strings.Split(strings.TrimSuffix(committed, "\x00"), "\x00")
			if _, err := gitOutput(ctx, root, append([]string{"--literal-pathspecs", "reset", "-q", "HEAD", "--"}, names...)...); err != nil {
				return Output{}, fmt.Errorf("已创建提交，但暂存区同步失败，请勿重试提交: %w", err)
			}
		}
	}
	return Output{Message: fmt.Sprintf("已提交: %s（未推送）", msg), Data: co}, nil
}

func (c *ReviewCmd) Name() string        { return "review" }
func (c *ReviewCmd) Aliases() []string   { return nil }
func (c *ReviewCmd) Description() string { return "审查工作区更改" }
func (c *ReviewCmd) Help() string        { return "/review - 显示并分析未提交的更改" }
func (c *ReviewCmd) Execute(ctx context.Context, in Input) (Output, error) {
	var sections []string
	for _, part := range []struct {
		title string
		args  []string
	}{
		{"已暂存", []string{"diff", "--cached", "--stat"}},
		{"未暂存", []string{"diff", "--stat"}},
		{"未跟踪", []string{"ls-files", "--others", "--exclude-standard"}},
	} {
		out, err := gitOutput(ctx, in.Cwd, part.args...)
		if err != nil {
			return Output{}, fmt.Errorf("无法读取 git 差异: %w", err)
		}
		if text := strings.TrimSpace(out); text != "" {
			sections = append(sections, part.title+":\n"+text)
		}
	}
	if len(sections) == 0 {
		return Output{Message: "没有需要审查的更改"}, nil
	}
	return Output{Message: "变更文件:\n" + strings.Join(sections, "\n\n")}, nil
}

func (c *DiffCmd) Name() string        { return "diff" }
func (c *DiffCmd) Aliases() []string   { return nil }
func (c *DiffCmd) Description() string { return "显示 git diff" }
func (c *DiffCmd) Help() string        { return "/diff - 显示工作区差异" }
func (c *DiffCmd) Execute(ctx context.Context, in Input) (Output, error) {
	unstaged, err := gitOutput(ctx, in.Cwd, "diff")
	if err != nil {
		return Output{Message: fmt.Sprintf("无法读取 git 差异: %v", err)}, nil
	}
	staged, err := gitOutput(ctx, in.Cwd, "diff", "--cached")
	if err != nil {
		return Output{Message: fmt.Sprintf("无法读取 git 差异: %v", err)}, nil
	}
	// Both halves are shown: showing the staged diff only when the unstaged
	// one was empty hid staged changes exactly when there were both kinds.
	hasStaged, hasUnstaged := strings.TrimSpace(staged) != "", strings.TrimSpace(unstaged) != ""
	switch {
	case !hasStaged && !hasUnstaged:
		return Output{Message: "无差异"}, nil
	case hasStaged && hasUnstaged:
		return Output{Data: "# 已暂存 (staged)\n" + staged + "\n# 未暂存 (unstaged)\n" + unstaged}, nil
	case hasStaged:
		return Output{Data: staged}, nil
	default:
		return Output{Data: unstaged}, nil
	}
}
