//go:build unix

package mcp

import (
	"errors"
	"os/exec"
	"sync"
	"syscall"
)

type unixStdioProcessTree struct {
	pgid   int
	mu     sync.Mutex
	killed bool
}

func configureStdioProcess(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

func attachStdioProcess(cmd *exec.Cmd) (stdioProcessTree, error) {
	return &unixStdioProcessTree{pgid: cmd.Process.Pid}, nil
}

func (p *unixStdioProcessTree) terminate() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.killed {
		return nil
	}
	err := syscall.Kill(-p.pgid, syscall.SIGKILL)
	if err == nil || errors.Is(err, syscall.ESRCH) {
		p.killed = true
		return nil
	}
	return err
}

func (*unixStdioProcessTree) close() error { return nil }
