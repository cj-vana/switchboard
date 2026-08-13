//go:build unix

package session

import (
	"os"
	"syscall"
)

// The kernel releases a flock when the file descriptor closes, including on
// SIGKILL. A crashed process therefore leaves no stale lock behind, which
// matters because the interrupted session is exactly the one being resumed.
func acquireLock(f *os.File) error {
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return ErrSessionLocked
	}
	return nil
}

func releaseLock(f *os.File) error {
	return syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
}
