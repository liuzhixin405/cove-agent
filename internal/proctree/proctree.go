// Package proctree makes a context's cancellation kill a command together
// with every process it started. exec.CommandContext alone kills only the
// lead process: a shell is killed and the `go test`, `grep` or `git repack`
// it spawned keeps running, holding the output pipes so Wait does not return
// until it exits on its own.
package proctree

import (
	"context"
	"os/exec"
	"time"
)

// Configure puts cmd in its own process group (Unix) and makes ctx
// cancellation kill the whole tree (taskkill /T on Windows). waitDelay bounds
// how long Wait keeps reading pipes a stray grandchild still holds after the
// tree kill. Call before cmd.Start.
func Configure(ctx context.Context, cmd *exec.Cmd, waitDelay time.Duration) {
	setupProcessGroup(cmd)
	if ctx != nil && ctx.Done() != nil {
		cmd.Cancel = func() error { return KillTree(cmd) }
	}
	if waitDelay > 0 {
		cmd.WaitDelay = waitDelay
	}
}
