package tool

import (
	"context"
	"os/exec"

	"github.com/liuzhixin405/cove-agent/internal/proctree"
)

// ConfigureProcessTreeKill makes ctx cancellation (or a deadline) kill cmd and
// all of its descendants, the way the bash tool runs commands. See
// proctree.Configure; WaitDelay is left to the caller.
func ConfigureProcessTreeKill(ctx context.Context, cmd *exec.Cmd) {
	proctree.Configure(ctx, cmd, 0)
}
