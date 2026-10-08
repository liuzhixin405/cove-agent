package automation

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

const maxOutput = 4 * 1024 * 1024

type boundedOutput struct {
	mu        sync.Mutex
	data      []byte
	truncated bool
}

func (b *boundedOutput) Write(data []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	remaining := maxOutput - len(b.data)
	if len(data) > remaining {
		b.truncated = true
	}
	b.data = append(b.data, data[:min(len(data), remaining)]...)
	return len(data), nil
}

func runCommand(ctx context.Context, cwd string, env []string, argv []string) (Check, error) {
	check := Check{Command: append([]string(nil), argv...), ExitCode: -1}
	if len(argv) == 0 {
		return check, errors.New("empty command")
	}
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Dir = cwd
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		if !strings.HasPrefix(strings.ToUpper(key), "GIT_") {
			cmd.Env = append(cmd.Env, entry)
		}
	}
	cmd.Env = append(cmd.Env, env...)
	cmd.Stdin = strings.NewReader("")
	cmd.WaitDelay = time.Second
	configureProcess(cmd)
	output := &boundedOutput{}
	cmd.Stdout, cmd.Stderr = output, output
	err := cmd.Start()
	if err == nil {
		release, attachErr := attachProcess(cmd)
		if attachErr != nil {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
			return check, attachErr
		}
		defer release()
		err = cmd.Wait()
	}
	if cmd.ProcessState != nil {
		check.ExitCode = cmd.ProcessState.ExitCode()
	}
	check.Output = string(output.data)
	if output.truncated {
		return check, errors.Join(err, errors.New("command output exceeded 4 MiB; patch/log is incomplete"))
	}
	return check, err
}

type CommandRunner struct {
	Executable string
	Prepare    func(worktree string, budgetUSD float64, spec Spec) (env []string, cleanup func(), err error)
}

func (r CommandRunner) Run(ctx context.Context, worktree string, spec Spec, budgetUSD float64) (Check, error) {
	if r.Executable == "" || r.Prepare == nil {
		return Check{ExitCode: -1}, errors.New("cove runner requires an executable and isolated budget configuration")
	}
	env, cleanup, err := r.Prepare(worktree, budgetUSD, spec)
	if err != nil {
		return Check{ExitCode: -1}, err
	}
	if cleanup != nil {
		defer cleanup()
	}
	prompt := spec.Prompt + "\nWork only in the current isolated worktree. Do not apply, merge, push, or modify other worktrees. Return a reviewable maintenance result."
	return runCommand(ctx, worktree, env, []string{r.Executable, "--no-auto", "--max-turns", fmtInt(spec.MaxTurns), "-p", prompt})
}
