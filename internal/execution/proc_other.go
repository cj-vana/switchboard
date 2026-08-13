//go:build !unix

package execution

import (
	"os/exec"
	"time"
)

func shellCommand(script string) (string, []string) {
	return "cmd.exe", []string{"/c", script}
}

// Windows has no process group signalling equivalent to killpg here, so a
// timeout kills only the direct child and descendants may survive. This is one
// of the reasons automatic execution is not offered on this platform: the
// runner cannot promise to clean up after a command it started (§19.3, §21.7).
func setProcessGroup(*exec.Cmd) {}

func terminateGroup(cmd *exec.Cmd, _ time.Duration) {
	if cmd.Process != nil {
		cmd.Process.Kill()
	}
}
