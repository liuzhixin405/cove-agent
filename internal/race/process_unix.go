//go:build !windows

package race

import (
	"os/exec"
	"syscall"
)

func containProcess(cmd *exec.Cmd) (*processContainment, error) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	kill := func() error {
		if cmd.Process == nil {
			return nil
		}
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	cmd.Cancel = kill
	return &processContainment{attach: func() error { return nil }, close: func() { _ = kill() }}, nil
}
