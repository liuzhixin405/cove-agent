package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/liuzhixin405/cove-agent/internal/permission"
	"github.com/liuzhixin405/cove-agent/internal/shell"
	"github.com/liuzhixin405/cove-agent/internal/tool"
)

// VerifyResult captures the outcome of running one configured verification
// command against the current workspace.
type VerifyResult struct {
	Command  string
	Passed   bool
	ExitCode int
	Output   string
	Duration time.Duration
	// Skipped: the command already passed earlier in this turn (the model ran
	// it itself), so the gate took that run as the evidence.
	Skipped bool
	// TimedOut: the command did not finish within Timeout. It is not Passed,
	// but it is not a failure either: a slow build (a cold dotnet restore, a
	// large npm build) says nothing about the model's work, so the gate
	// neither rejects the completion nor escalates the model.
	TimedOut bool
	Timeout  time.Duration
}

// Default verification timeouts: most checks finish well within
// verifyTimeoutDefault; dotnet and npm builds (restores, bundlers) get
// verifyTimeoutSlow.
const (
	verifyTimeoutDefault = 120 * time.Second
	verifyTimeoutSlow    = 300 * time.Second
)

// defaultVerifyTimeout is the timeout of cmd when none is configured.
func defaultVerifyTimeout(cmd string) time.Duration {
	if f := strings.Fields(cmd); len(f) > 0 {
		name := strings.ToLower(filepath.Base(f[0]))
		name = strings.TrimSuffix(strings.TrimSuffix(name, ".exe"), ".cmd")
		switch name {
		case "dotnet", "npm", "pytest":
			return verifyTimeoutSlow
		case "go":
			if len(f) > 1 && f[1] == "test" {
				return verifyTimeoutSlow
			}
		}
	}
	return verifyTimeoutDefault
}

// VerifyGate runs a small, user-configured list of shell commands (e.g.
// "go build ./...", "go test ./...") before the engine accepts a model's
// "I'm done" (a response with no further tool calls) as actually done.
//
// This is the minimal version of the "完成合同 + 验证门禁" item from
// docs/核心优化项清单.md's EDCL proposal: no evidence-ledger UI, no scoring —
// just "run the commands, and if any fails, hand the failure back to the
// model instead of ending the turn." Every check is still appended to a
// JSONL file on disk so results are at least inspectable later, which is
// enough to satisfy the underlying goal (completion claims are backed by
// something checkable) without building the full ledger UI up front.
//
// It is opt-in and off by default (zero commands configured = no-op), and
// bounded: RunIfDue caps how many times it will re-reject a single user
// turn (maxRetries) so a flaky/always-failing command can't turn into an
// unbounded retry loop and blow through the cost budget it's supposed to
// protect.
type VerifyGate struct {
	commands   []string
	workDir    string
	maxRetries int
	// timeout, when positive, bounds every command (config
	// done_verify_timeout_seconds); 0 uses defaultVerifyTimeout.
	timeout    time.Duration
	ledgerPath string
	// onlyWhenFilesChanged limits the gate to turns that wrote or edited a
	// file. Set for automatically detected commands (newAutoVerifyGate).
	onlyWhenFilesChanged bool
	// needsTrust marks a gate of detected commands and automatic tests: it
	// runs only in a trusted project or in auto/bypass mode (verifyTrusted).
	needsTrust bool
	// dynamic, when set, returns further commands for the current turn, run
	// after the fixed ones (the tests of what the turn changed,
	// testCommandsFor).
	dynamic func() []string
	// runner executes one command; nil means runVerifyCommand (replaced in
	// tests).
	runner func(ctx context.Context, cmd, workDir string) (string, int, error)
}

// NewVerifyGate creates a gate for the given commands. An empty commands
// slice makes every method a no-op, which is the default: nothing changes
// for users who don't set done_verify_commands in config.json.
func NewVerifyGate(commands []string, workDir string) *VerifyGate {
	var ledger string
	if home, err := os.UserHomeDir(); err == nil {
		ledger = filepath.Join(home, ".cove", "verify_ledger.jsonl")
	}
	return &VerifyGate{
		commands:   commands,
		workDir:    workDir,
		maxRetries: 2,
		ledgerPath: ledger,
	}
}

// SetTimeout sets one timeout for every command; 0 or less restores the
// per-command defaults (defaultVerifyTimeout).
func (g *VerifyGate) SetTimeout(d time.Duration) {
	if g == nil {
		return
	}
	if d < 0 {
		d = 0
	}
	g.timeout = d
}

// timeoutFor is the timeout cmd runs under.
func (g *VerifyGate) timeoutFor(cmd string) time.Duration {
	if g.timeout > 0 {
		return g.timeout
	}
	return defaultVerifyTimeout(cmd)
}

// Enabled reports whether any verification commands are configured.
func (g *VerifyGate) Enabled() bool { return g != nil && (len(g.commands) > 0 || g.dynamic != nil) }

// turnCommands are the commands a check of the current turn runs.
func (g *VerifyGate) turnCommands() []string {
	cmds := append([]string(nil), g.commands...)
	if g.dynamic != nil {
		cmds = append(cmds, g.dynamic()...)
	}
	return cmds
}

// MaxRetries returns how many times the gate will reject a single turn's
// completion before giving up and letting it through anyway (to bound cost).
func (g *VerifyGate) MaxRetries() int {
	if g == nil {
		return 0
	}
	return g.maxRetries
}

// Run executes every configured command in order and stops at the first
// failure (fail-fast: no point running the test suite if the build itself
// is broken). It never returns an error itself — a command that can't even
// start is recorded as a failed result, not a Go-level error, since from the
// gate's point of view that's just as much "not verified" as a nonzero exit.
//
// alreadyPassed, when non-nil, names commands the model already ran with
// success this turn, after its last change (see noteVerifyEvidence): those
// are not run again and count as passed.
func (g *VerifyGate) Run(ctx context.Context, alreadyPassed func(cmd string) bool) (results []VerifyResult, allPassed bool) {
	return g.run(ctx, alreadyPassed, nil)
}

func (g *VerifyGate) run(ctx context.Context, alreadyPassed func(string) bool, observe func([]string, []VerifyResult)) (results []VerifyResult, allPassed bool) {
	if !g.Enabled() {
		return nil, true
	}
	commands := g.turnCommands()
	if observe != nil {
		defer func() { observe(commands, results) }()
	}
	runner := g.runner
	if runner == nil {
		runner = runVerifyCommand
	}
	allPassed = true
	for _, cmdStr := range commands {
		if alreadyPassed != nil && alreadyPassed(cmdStr) {
			results = append(results, VerifyResult{Command: cmdStr, Passed: true, Skipped: true})
			continue
		}
		start := time.Now()
		limit := g.timeoutFor(cmdStr)
		runCtx, cancel := context.WithTimeout(ctx, limit)
		out, exitCode, runErr := runner(runCtx, cmdStr, g.workDir)
		deadlineHit := errors.Is(runCtx.Err(), context.DeadlineExceeded) && ctx.Err() == nil
		cancel()
		if ctx.Err() != nil {
			// Cancelled (Ctrl+C): the command was killed, which says
			// nothing about the change. It used to go into the ledger as a
			// failure. The caller sees ctx.Err() and ends the turn.
			return results, false
		}
		if runErr != nil {
			out = out + "\n[verify_gate] failed to run command: " + runErr.Error()
			exitCode = -1
		}
		passed := exitCode == 0
		res := VerifyResult{
			Command: cmdStr, Passed: passed, ExitCode: exitCode,
			Output: out, Duration: time.Since(start),
			TimedOut: !passed && deadlineHit, Timeout: limit,
		}
		results = append(results, res)
		g.appendLedger(res)
		if !passed {
			allPassed = false
			break // fail-fast; remaining commands are skipped, not just unreported
		}
	}
	return results, allPassed
}

// TimedOut reports whether the check stopped at a command that did not
// finish in time (the last result): not a failure, so the gate neither
// retries nor escalates.
func TimedOut(results []VerifyResult) bool {
	return len(results) > 0 && results[len(results)-1].TimedOut
}

// Summary renders the check results as guidance to hand back to the model:
// which command(s) ran, which one failed, its exit code, and a truncated
// tail of its output (the part most likely to contain the actual error).
func Summary(results []VerifyResult) string {
	var sb strings.Builder
	if TimedOut(results) {
		r := results[len(results)-1]
		fmt.Fprintf(&sb, "[verify_gate] %s: verification did not finish within %d s; not counted as a failure.", r.Command, int(r.Timeout.Seconds()))
		return sb.String()
	}
	sb.WriteString("[verify_gate] Your completion was not accepted because a verification command failed. Results:\n")
	for _, r := range results {
		if r.Passed {
			fmt.Fprintf(&sb, "  OK   %s (%s)\n", r.Command, r.Duration.Round(time.Millisecond))
			continue
		}
		fmt.Fprintf(&sb, "  FAIL %s (exit %d, %s)\n", r.Command, r.ExitCode, r.Duration.Round(time.Millisecond))
		sb.WriteString("---\n")
		sb.WriteString(truncateTail(r.Output, 2000))
		sb.WriteString("\n---\n")
	}
	sb.WriteString("Fix the issue above before declaring the task complete again. Do not repeat the same fix if it already failed once — diagnose the actual error output first.")
	return sb.String()
}

func (g *VerifyGate) appendLedger(r VerifyResult) {
	if g.ledgerPath == "" {
		return
	}
	if dir := filepath.Dir(g.ledgerPath); dir != "" {
		_ = os.MkdirAll(dir, 0755)
	}
	f, err := os.OpenFile(g.ledgerPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return
	}
	defer func() { _ = f.Close() }()

	entry := map[string]any{
		"time":        time.Now().Format(time.RFC3339),
		"command":     r.Command,
		"passed":      r.Passed,
		"timed_out":   r.TimedOut,
		"exit_code":   r.ExitCode,
		"duration_ms": r.Duration.Milliseconds(),
		"output_tail": truncateTail(r.Output, 500),
	}
	line, err := json.Marshal(entry)
	if err != nil {
		return
	}
	_, _ = f.Write(append(line, '\n'))
}

// runVerifyCommand executes a single shell command in the same shell the bash
// tool uses, so a verify command written like the model's own commands runs.
func runVerifyCommand(ctx context.Context, cmdStr string, workDir string) (output string, exitCode int, err error) {
	sh := shell.Default()
	cmd := exec.CommandContext(ctx, sh.Path, sh.Args(cmdStr)...)
	if workDir != "" {
		cmd.Dir = workDir
	}
	cmd.Env = shell.Env(os.Environ())
	// On timeout kill the whole tree (the shell plus the go/npm/dotnet it
	// started), as the bash tool does; killing only the outer shell left the
	// build running, holding caches and writing into the workspace. WaitDelay
	// stays as a backstop for pipes a stray grandchild still holds.
	tool.ConfigureProcessTreeKill(ctx, cmd)
	cmd.WaitDelay = 5 * time.Second

	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf

	runErr := cmd.Run()
	out := buf.String()
	if len(out) > 20000 {
		out = tailBytes(out, 20000) // keep the tail: errors are usually at the end
	}
	if runErr == nil {
		return out, 0, nil
	}
	if exitErr := (*exec.ExitError)(nil); errors.As(runErr, &exitErr) {
		return out, exitErr.ExitCode(), nil
	}
	// Could not even start the command (bad shell, timeout before start, etc).
	return out, -1, runErr
}

func truncateTail(s string, maxBytes int) string {
	if len(s) <= maxBytes {
		return s
	}
	return "... (truncated)\n" + tailBytes(s, maxBytes)
}

// tailBytes is the last maxBytes bytes of s at most, starting on a rune
// boundary. Slicing bytes (as truncateTail and the 20000-byte cap did) cut
// Chinese compiler output mid-rune, and the model got invalid UTF-8.
func tailBytes(s string, maxBytes int) string {
	if len(s) <= maxBytes {
		return s
	}
	start := len(s) - maxBytes
	for start < len(s) && !utf8.RuneStart(s[start]) {
		start++
	}
	return s[start:]
}

// normalizeCommand collapses runs of whitespace, so "go  build ./... " and
// "go build ./..." are the same command. Nothing else is equivalent: a
// compound line ("go build ./... && go test ./...") is its own command.
func normalizeCommand(cmd string) string {
	return strings.Join(strings.Fields(cmd), " ")
}

// shellResultPassed reports whether a shell tool result is a success: no
// nonzero exit code, no error, not timed out or cancelled.
func shellResultPassed(out string) bool {
	return !strings.Contains(out, "[exit code:") && !strings.Contains(out, "Error") &&
		!strings.Contains(out, "[timed out") && !strings.Contains(out, "[cancelled]") &&
		!strings.HasPrefix(out, "BLOCKED")
}

// noteVerifyEvidence updates the turn's record of shell commands that passed
// since the last change to the workspace, from one tool result. A passing
// run is evidence only until something may have changed files after it: a
// write-capable tool, or a shell line that is not read-only or a build/test
// line. Read-only tools leave the evidence alone.
func (e *Engine) noteVerifyEvidence(l *turnLimits, name string, input map[string]any, result string, failed bool) {
	e.noteWorkTool(l, name, failed)
	agentType, _ := input["type"].(string)
	if !failed && (name == "regression_verify" || name == "agent" && strings.EqualFold(agentType, "verify")) {
		return
	}
	if permission.IsShellTool(name) {
		cmd, _ := input["command"].(string)
		cmd = normalizeCommand(cmd)
		if cmd == "" {
			return
		}
		autoOK := e.classifier != nil && e.classifier.AutoApproveLineFor(cmd, e.perm.ShellKindFor(name))
		if !autoOK {
			l.passedCmds = nil // the line may have changed files
			e.invalidateAcceptance("")
		}
		if !failed && shellResultPassed(result) {
			if l.passedCmds == nil {
				l.passedCmds = map[string]bool{}
			}
			l.passedCmds[cmd] = true
		} else {
			delete(l.passedCmds, cmd)
			e.invalidateAcceptance(cmd)
		}
		return
	}
	if t, ok := e.registry.Find(name); ok && t.Def().IsReadOnly {
		return
	}
	l.passedCmds = nil
	e.invalidateAcceptance("")
}

// verifyPassed reports whether cmd passed this turn after the last change
// (noteVerifyEvidence).
func (l *turnLimits) verifyPassed(cmd string) bool {
	return l.passedCmds[normalizeCommand(cmd)]
}
