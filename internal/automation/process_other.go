//go:build !windows

package automation

import (
	"os/exec"
	"strconv"
	"syscall"
)

func fmtInt(value int) string { return strconv.Itoa(value) }

func configureProcess(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
}

func attachProcess(cmd *exec.Cmd) (func(), error) { return func() {}, nil }
