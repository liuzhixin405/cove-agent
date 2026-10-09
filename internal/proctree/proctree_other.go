//go:build !windows

package proctree

import (
	"os/exec"
	"syscall"
)

// setupProcessGroup places the child in its own process group so the entire
// group (the command and any children it spawns) can be signalled at once.
func setupProcessGroup(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
}

// killProcessTree terminates cmd and all of its descendants by signalling the
// whole process group (negative PID). Falls back to killing just the lead
// process if the group signal fails.
func KillTree(cmd *exec.Cmd) error {
	if cmd == nil || cmd.Process == nil {
		return nil
	}
	if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil {
		return cmd.Process.Kill()
	}
	return nil
}
