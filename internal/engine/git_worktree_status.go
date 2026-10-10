package engine

import (
	"context"
	"encoding/hex"
	"fmt"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/liuzhixin405/cove-agent/internal/api"
	"github.com/liuzhixin405/cove-agent/internal/permission"
	"github.com/liuzhixin405/cove-agent/internal/safety"
	"github.com/liuzhixin405/cove-agent/internal/session"
)

// gitStatusTimeout bounds the turn-end `git status`; a slow repository must
// not hold the prompt back.
const gitStatusTimeout = 3 * time.Second

func (e *Engine) recordGitEvidence(ctx context.Context, tc api.ToolCall, cwd, output string, failed bool) {
	if !permission.IsShellTool(tc.Name) {
		return
	}
	e.acceptanceMu.Lock()
	active := e.acceptance != nil && e.acceptance.Outcome == "running" && session.SameProjectDir(e.acceptance.Cwd, cwd)
	e.acceptanceMu.Unlock()
	if !active {
		return
	}
	command, _ := tc.Input["command"].(string)
	commands := safety.SimpleCommands(command)
	if e.perm != nil && e.perm.ShellKindFor(tc.Name) == permission.ShellPOSIX {
		commands = safety.SimpleCommandsPOSIX(command)
	}
	if len(commands) != 1 || len(commands[0].Redirects) != 0 || len(safety.NestedCommands(command)) != 0 || safety.HasHostileCharacters(command) || strings.ContainsRune(command, '&') {
		return
	}
	words := commands[0].Words
	if e.perm != nil && e.perm.ShellKindFor(tc.Name) == permission.ShellPOSIX && len(words) > 1 && words[0] == "time" {
		words = words[1:]
		if words[0] == "-p" {
			words = words[1:]
		}
	}
	if len(words) < 2 || safety.ProgramName(words[0]) != "git" {
		return
	}
	switch words[1] {
	case "commit", "tag", "push", "ls-remote":
	default:
		return
	}
	evidence := GitEvidence{Action: words[1], Command: command, Cwd: session.NormalizeProjectDir(cwd), Status: "passed", Reason: "Git 命令执行成功；不代表测试通过"}
	if failed {
		evidence.Status, evidence.Reason = "failed", truncateTail(output, 1000)
	} else {
		readCtx, cancel := context.WithTimeout(ctx, gitStatusTimeout)
		defer cancel()
		git := func(ref string) (string, error) {
			cmd := exec.CommandContext(readCtx, "git", "rev-parse", "--verify", ref)
			cmd.Dir = cwd
			value, err := cmd.Output()
			return strings.TrimSpace(string(value)), err
		}
		evidence.Commit, _ = git("HEAD")
		if words[1] == "ls-remote" {
			evidence.RemoteRefs = map[string]string{}
			evidence.Status, evidence.Reason = "unverified", "未获得可与本地引用核对的远端证据"
			valid := true
			for _, line := range strings.Split(strings.TrimSpace(output), "\n") {
				fields := strings.Fields(line)
				if len(fields) != 2 || !strings.HasPrefix(fields[1], "refs/") || len(evidence.RemoteRefs) >= 64 {
					valid = false
					break
				}
				bytes, err := hex.DecodeString(fields[0])
				if err != nil || len(bytes) != 20 && len(bytes) != 32 {
					valid = false
					break
				}
				if _, duplicate := evidence.RemoteRefs[fields[1]]; duplicate {
					valid = false
					break
				}
				evidence.RemoteRefs[fields[1]] = fields[0]
			}
			if valid && len(evidence.RemoteRefs) > 0 {
				evidence.Status, evidence.Reason = "passed", "返回的远端引用均与本地同名引用一致"
				for ref, hash := range evidence.RemoteRefs {
					local, err := git(ref)
					if err != nil {
						evidence.Status, evidence.Reason = "unverified", "本地引用不可读取: "+ref
						break
					}
					if local != hash {
						evidence.Status, evidence.Reason = "failed", "远端引用与本地不一致: "+ref
						break
					}
				}
			}
		}
	}
	e.acceptanceMu.Lock()
	defer e.acceptanceMu.Unlock()
	if e.acceptance == nil || e.acceptance.Outcome != "running" || !session.SameProjectDir(e.acceptance.Cwd, cwd) {
		return
	}
	if len(e.acceptance.Git) >= 32 {
		e.acceptance.Git = e.acceptance.Git[len(e.acceptance.Git)-31:]
	}
	e.acceptance.Git = append(e.acceptance.Git, evidence)
}

// gitWorkState is what `git status --porcelain --branch` says about the
// working tree: whether the changes are committed, and the commits pushed.
type gitWorkState struct {
	Branch   string // "" when detached or unknown
	Upstream string // "" when the branch has no upstream
	Ahead    int    // commits not on the upstream (as of the last fetch/push)
	Behind   int    // upstream commits not on the branch (as of the last fetch)
	Changed  int    // files with uncommitted changes (untracked included)
}

// gitAheadRe and gitBehindRe find "ahead N" and "behind N" in the branch
// header's bracket.
var (
	gitAheadRe  = regexp.MustCompile(`\bahead (\d+)`)
	gitBehindRe = regexp.MustCompile(`\bbehind (\d+)`)
)

// parseGitStatus reads `git status --porcelain=v1 --branch` output. The first
// line is the branch header:
//
//	## main...origin/main [ahead 2, behind 1]
//	## feature
//	## No commits yet on main
//	## HEAD (no branch)
func parseGitStatus(out string) gitWorkState {
	var s gitWorkState
	for i, line := range strings.Split(strings.ReplaceAll(out, "\r\n", "\n"), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		if i == 0 && strings.HasPrefix(line, "## ") {
			head := strings.TrimPrefix(line, "## ")
			if m := gitAheadRe.FindStringSubmatch(head); m != nil {
				s.Ahead, _ = strconv.Atoi(m[1])
			}
			if m := gitBehindRe.FindStringSubmatch(head); m != nil {
				s.Behind, _ = strconv.Atoi(m[1])
			}
			gone := strings.Contains(head, "[gone]")
			if j := strings.Index(head, " ["); j >= 0 {
				head = head[:j]
			}
			switch {
			case strings.HasPrefix(head, "No commits yet on "):
				s.Branch = strings.TrimPrefix(head, "No commits yet on ")
			case strings.HasPrefix(head, "HEAD (no branch)"):
			default:
				s.Branch, s.Upstream, _ = strings.Cut(head, "...")
			}
			if gone {
				// The upstream branch was deleted: nothing is pushed anywhere.
				s.Upstream = ""
			}
			continue
		}
		s.Changed++
	}
	return s
}

// Line is the one status line shown after a turn, in Chinese, stating
// facts only: what is uncommitted, what is committed but not pushed.
func (s gitWorkState) Line() string {
	var parts []string
	if s.Changed > 0 {
		parts = append(parts, fmt.Sprintf("%d 个文件有未提交的改动", s.Changed))
	}
	switch {
	case s.Branch == "":
		if s.Changed == 0 {
			parts = append(parts, "工作区干净（当前不在任何分支上）")
		}
	case s.Upstream == "":
		if s.Changed == 0 {
			parts = append(parts, "改动已提交")
		}
		parts = append(parts, fmt.Sprintf("分支 %s 没有上游，未推送", s.Branch))
	case s.Ahead > 0:
		if s.Changed == 0 {
			parts = append(parts, "改动已提交")
		}
		parts = append(parts, fmt.Sprintf("%d 个提交未推送到 %s", s.Ahead, s.Upstream))
	case s.Changed == 0:
		// A state, not an action: it used to say "已提交并推送到 origin/main",
		// which after a turn that only looked (or changed nothing) read as if
		// that turn had committed and pushed. The ahead count is as of the
		// last fetch or push, hence "按本地记录".
		if s.Behind > 0 {
			parts = append(parts, fmt.Sprintf("工作区干净，没有未推送的提交，落后 %s %d 个提交（按本地记录）", s.Upstream, s.Behind))
		} else {
			parts = append(parts, fmt.Sprintf("工作区干净，与 %s 同步（按本地记录）", s.Upstream))
		}
	}
	return "git：" + strings.Join(parts, "，")
}

// readGitWorkState runs `git status` in dir.
func readGitWorkState(ctx context.Context, dir string) (gitWorkState, error) {
	ctx, cancel := context.WithTimeout(ctx, gitStatusTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", "status", "--porcelain=v1", "--branch")
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		return gitWorkState{}, err
	}
	return parseGitStatus(string(out)), nil
}

// gitCommandRe matches a shell command that runs git (git commit, git push,
// cd x && git add ...), not a word that merely contains "git". The program
// may be path-qualified and quoted: `/usr/bin/git push` and
// `"C:\Program Files\Git\bin\git.exe" commit` were not seen as git. The
// match ends after the whitespace that follows the program name, where its
// arguments start.
var gitCommandRe = regexp.MustCompile(`(?i)(?:^|[\s;&|(])(?:"(?:[^"]*[/\\])?git(?:\.exe)?"|'(?:[^']*[/\\])?git(?:\.exe)?'|(?:[^\s;&|()"']*[/\\])?git(?:\.exe)?)\s`)

// gitRepoChangingCommands are the git subcommands that can change the
// repository's state (the working tree, the index, refs or the remote), so
// the status line after the turn says something new. Subcommands not listed
// (log, diff, status, show, blame, fetch, remote, config, grep…) only look.
// stash, tag and branch also have read-only forms; gitChangesRepo tells
// them apart.
var gitRepoChangingCommands = map[string]bool{
	"add": true, "am": true, "apply": true, "checkout": true, "cherry-pick": true,
	"clean": true, "clone": true, "commit": true, "init": true, "merge": true,
	"mv": true, "pull": true, "push": true, "rebase": true, "reset": true,
	"restore": true, "revert": true, "rm": true, "switch": true,
	"stash": true, "tag": true, "branch": true,
}

// gitGlobalOptsWithValue are git's global options that take the next word
// as their value (git -C dir commit).
var gitGlobalOptsWithValue = map[string]bool{
	"-C": true, "-c": true, "--git-dir": true, "--work-tree": true, "--namespace": true, "--config-env": true,
}

// gitChangesRepo reports whether a shell command runs a git subcommand that
// can change the repository (gitRepoChangingCommands). A read-only
// `git log` or `git status` used to count like a commit and put the status
// line under a turn that had only looked.
func gitChangesRepo(cmd string) bool {
	for _, loc := range gitCommandRe.FindAllStringIndex(cmd, -1) {
		if gitArgsChangeRepo(shellWords(cmd[loc[1]:])) {
			return true
		}
	}
	return false
}

// gitArgsChangeRepo reports whether git's arguments (global options, the
// subcommand and its own arguments) name a repository-changing command.
func gitArgsChangeRepo(args []string) bool {
	for len(args) > 0 && strings.HasPrefix(args[0], "-") {
		if gitGlobalOptsWithValue[args[0]] {
			args = args[1:]
		}
		if len(args) > 0 {
			args = args[1:]
		}
	}
	if len(args) == 0 || !gitRepoChangingCommands[args[0]] {
		return false
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "stash":
		return len(rest) == 0 || (rest[0] != "list" && rest[0] != "show")
	case "tag":
		return refCommandChanges(rest,
			[]string{"-d", "--delete", "-a", "--annotate", "-s", "--sign", "-f", "--force", "-m", "-F"},
			[]string{"-l", "--list", "--contains", "--no-contains", "--points-at", "--merged", "--no-merged"})
	case "branch":
		return refCommandChanges(rest,
			[]string{"-d", "-D", "--delete", "-m", "-M", "--move", "-c", "-C", "--copy", "-f", "--force",
				"-u", "--set-upstream-to", "--unset-upstream", "--edit-description"},
			[]string{"-l", "--list", "-a", "--all", "-r", "--remotes", "-v", "-vv", "--verbose",
				"--contains", "--no-contains", "--merged", "--no-merged", "--points-at", "--show-current"})
	}
	return true
}

// refCommandChanges tells a changing `git tag` / `git branch` (deleting,
// moving or creating a ref) from a listing one: a changing option, or a
// name given without a listing option, changes refs.
func refCommandChanges(args, changing, listing []string) bool {
	has := func(opts []string, a string) bool {
		name, _, _ := strings.Cut(a, "=")
		for _, o := range opts {
			if name == o {
				return true
			}
		}
		return false
	}
	named, listed := false, false
	for _, a := range args {
		switch {
		case has(changing, a):
			return true
		case has(listing, a):
			listed = true
		case !strings.HasPrefix(a, "-"):
			named = true
		}
	}
	return named && !listed
}

// shellWords splits the start of a shell command into words, honoring
// quotes, up to the first unquoted command separator (; & | ) or newline).
func shellWords(s string) []string {
	var words []string
	var cur strings.Builder
	inWord := false
	var quote rune
	for _, r := range s {
		switch {
		case quote != 0:
			if r == quote {
				quote = 0
			} else {
				cur.WriteRune(r)
			}
		case r == '"' || r == '\'':
			quote, inWord = r, true
		case r == ';' || r == '&' || r == '|' || r == ')' || r == '\n':
			if inWord {
				words = append(words, cur.String())
			}
			return words
		case r == ' ' || r == '\t' || r == '\r':
			if inWord {
				words = append(words, cur.String())
				cur.Reset()
				inWord = false
			}
		default:
			cur.WriteRune(r)
			inWord = true
		}
	}
	if inWord {
		words = append(words, cur.String())
	}
	return words
}

// noteGitInvocation records a shell call that runs a repository-changing
// git command, before the call runs. It used to be recorded only once the
// call had succeeded, so a `git push` that timed out or failed left no
// status line, which is exactly when where the commits stand matters.
func (e *Engine) noteGitInvocation(tc api.ToolCall) {
	if e.projCtx == nil || (tc.Name != "bash" && tc.Name != "powershell") {
		return
	}
	cmd, _ := tc.Input["command"].(string)
	if !gitChangesRepo(cmd) {
		return
	}
	e.fileMu.Lock()
	e.turnRanGit = true
	e.fileMu.Unlock()
}

// readGitWorkStateFn is readGitWorkState, a variable for tests.
var readGitWorkStateFn = readGitWorkState

// reportGitWorkState shows, after a turn that changed files or ran a
// repository-changing git command in a repository, where the changes stand:
// uncommitted, committed but not pushed, or in sync with the upstream. It
// comes from git, not from the model: a final report that ended with "the
// commit and push steps are…" read as if they had been run when nothing was
// committed.
func (e *Engine) reportGitWorkState(ctx context.Context) {
	if e.projCtx == nil || !e.projCtx.IsGitRepo {
		return
	}
	e.fileMu.Lock()
	relevant := e.turnFilesChanged || e.turnRanGit
	e.fileMu.Unlock()
	if !relevant {
		return
	}
	s, err := readGitWorkStateFn(ctx, e.projectCwd())
	if err != nil {
		return
	}
	e.engineOutput("  \x1b[2m" + s.Line() + "\x1b[0m")
}
