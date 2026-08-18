//go:build windows

package mcp

import (
	"fmt"
	"os/exec"
	"sync"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

var ntResumeProcess = windows.NewLazySystemDLL("ntdll.dll").NewProc("NtResumeProcess")

type windowsStdioProcessTree struct {
	mu     sync.Mutex
	job    windows.Handle
	killed bool
	closed bool
}

func configureStdioProcess(cmd *exec.Cmd) {
	// CREATE_SUSPENDED closes the containment race: no wrapper code can spawn
	// before attachStdioProcess assigns the root to the kill-on-close job.
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: windows.CREATE_NEW_PROCESS_GROUP | windows.CREATE_SUSPENDED}
}

func attachStdioProcess(cmd *exec.Cmd) (stdioProcessTree, error) {
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return nil, err
	}
	info := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{}
	info.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	if _, err := windows.SetInformationJobObject(
		job,
		windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)),
		uint32(unsafe.Sizeof(info)),
	); err != nil {
		_ = windows.CloseHandle(job)
		return nil, err
	}
	if err := ntResumeProcess.Find(); err != nil {
		_ = windows.CloseHandle(job)
		return nil, err
	}
	var attachErr error
	if err := cmd.Process.WithHandle(func(raw uintptr) {
		process := windows.Handle(raw)
		if err := windows.AssignProcessToJobObject(job, process); err != nil {
			attachErr = err
			return
		}
		status, _, _ := ntResumeProcess.Call(raw)
		if ntStatus := windows.NTStatus(status); ntStatus != 0 {
			attachErr = fmt.Errorf("resuming contained process: %w", ntStatus)
		}
	}); err != nil {
		_ = windows.CloseHandle(job)
		return nil, err
	}
	if attachErr != nil {
		_ = windows.CloseHandle(job)
		return nil, attachErr
	}
	return &windowsStdioProcessTree{job: job}, nil
}

func (p *windowsStdioProcessTree) terminate() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed || p.killed {
		return nil
	}
	if err := windows.TerminateJobObject(p.job, 1); err != nil {
		return err
	}
	p.killed = true
	return nil
}

func (p *windowsStdioProcessTree) close() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return nil
	}
	p.closed = true
	return windows.CloseHandle(p.job)
}
