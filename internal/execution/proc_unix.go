//go:build unix

package execution

import (
	"os/exec"
	"syscall"
	"time"
)

func shellCommand(script string) (string, []string) {
	return "/bin/sh", []string{"-c", script}
}

// setProcessGroup puts the child in a group of its own so its descendants can
// be signalled together. Without it, killing a shell leaves the compiler it
// spawned running, holding the workspace and the output pipe.
func setProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// terminateGroup signals the whole group, escalating to SIGKILL after a grace
// period so a well-behaved process can flush first.
func terminateGroup(cmd *exec.Cmd, grace time.Duration) {
	if cmd.Process == nil {
		return
	}
	pgid := -cmd.Process.Pid

	if err := syscall.Kill(pgid, syscall.SIGTERM); err != nil {
		// The group is already gone, or was never created because the exec
		// failed. Either way there is nothing left to escalate against.
		return
	}

	deadline := time.After(grace)
	poll := time.NewTicker(50 * time.Millisecond)
	defer poll.Stop()
	for {
		select {
		case <-deadline:
			syscall.Kill(pgid, syscall.SIGKILL)
			return
		case <-poll.C:
			// Signal 0 tests for the group's existence without delivering
			// anything, so a group that exited on SIGTERM is never SIGKILLed.
			if err := syscall.Kill(pgid, 0); err != nil {
				return
			}
		}
	}
}
