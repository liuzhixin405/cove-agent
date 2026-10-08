package race

import (
	"fmt"
	"os/exec"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

func containProcess(cmd *exec.Cmd) (*processContainment, error) {
	resume := windows.NewLazySystemDLL("ntdll.dll").NewProc("NtResumeProcess")
	if err := resume.Find(); err != nil {
		return nil, err
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: windows.CREATE_SUSPENDED}
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return nil, err
	}
	limits := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{}
	limits.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	if _, err := windows.SetInformationJobObject(job, windows.JobObjectExtendedLimitInformation, uintptr(unsafe.Pointer(&limits)), uint32(unsafe.Sizeof(limits))); err != nil {
		windows.CloseHandle(job)
		return nil, err
	}
	cmd.Cancel = func() error {
		_ = windows.TerminateJobObject(job, 1)
		if cmd.Process == nil {
			return nil
		}
		return cmd.Process.Kill()
	}
	var reaped chan struct{}
	return &processContainment{
		attach: func() error {
			handle, err := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE|windows.PROCESS_SUSPEND_RESUME|windows.SYNCHRONIZE, false, uint32(cmd.Process.Pid))
			if err != nil {
				return err
			}
			if err := windows.AssignProcessToJobObject(job, handle); err != nil {
				windows.CloseHandle(handle)
				return err
			}
			reaped = make(chan struct{})
			go func() {
				defer close(reaped)
				defer windows.CloseHandle(handle)
				_, _ = windows.WaitForSingleObject(handle, windows.INFINITE)
				_ = windows.TerminateJobObject(job, 1)
			}()
			status, _, _ := resume.Call(uintptr(handle))
			if status != 0 {
				return fmt.Errorf("resume contained process: NTSTATUS %#x", status)
			}
			return nil
		},
		close: func() {
			if reaped != nil {
				<-reaped
			}
			_ = windows.CloseHandle(job)
		},
	}, nil
}
