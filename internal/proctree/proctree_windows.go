//go:build windows

package proctree

import (
	"os/exec"
	"strconv"
)

// setupProcessGroup is a no-op on Windows; taskkill /T handles the whole tree.
func setupProcessGroup(cmd *exec.Cmd) {}

// killProcessTree terminates cmd and all of its descendants. On Windows we shell
// out to taskkill with /T (tree) and /F (force) because there is no portable
// "kill process group" syscall. If taskkill is unavailable or fails we fall back
// to killing just the lead process.
func KillTree(cmd *exec.Cmd) error {
	if cmd == nil || cmd.Process == nil {
		return nil
	}
	pid := strconv.Itoa(cmd.Process.Pid)
	if err := exec.Command("taskkill", "/F", "/T", "/PID", pid).Run(); err != nil {
		return cmd.Process.Kill()
	}
	return nil
}
