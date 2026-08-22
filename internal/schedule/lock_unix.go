//go:build unix

package schedule

import (
	"os"
	"syscall"
)

// The kernel releases a flock when the file descriptor closes, including on
// SIGKILL, so a crashed process never leaves the ledger locked behind it.
func acquireLock(f *os.File) error {
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return ErrLocked
	}
	return nil
}

func releaseLock(f *os.File) error {
	return syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
}
