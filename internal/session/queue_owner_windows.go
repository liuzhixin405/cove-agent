package session

import (
	"errors"

	"golang.org/x/sys/windows"
)

func queueOwnerRunning(pid int) bool {
	if pid <= 0 {
		return false
	}
	handle, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return !errors.Is(err, windows.ERROR_INVALID_PARAMETER)
	}
	defer func() { _ = windows.CloseHandle(handle) }()
	var exitCode uint32
	err = windows.GetExitCodeProcess(handle, &exitCode)
	return err != nil || exitCode == 259
}
