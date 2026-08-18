//go:build !unix && !windows

package mcp

import "os/exec"

type directStdioProcess struct {
	cmd *exec.Cmd
}

func configureStdioProcess(*exec.Cmd) {}

func attachStdioProcess(cmd *exec.Cmd) (stdioProcessTree, error) {
	return &directStdioProcess{cmd: cmd}, nil
}

func (p *directStdioProcess) terminate() error {
	if p.cmd.Process == nil {
		return nil
	}
	return p.cmd.Process.Kill()
}

func (*directStdioProcess) close() error { return nil }
