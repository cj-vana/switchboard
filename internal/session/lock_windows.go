//go:build windows

package session

import (
	"errors"
	"fmt"
	"math"
	"os"

	"golang.org/x/sys/windows"
)

// LockFileEx is held on the session descriptor itself, from before migration
// and replay until Close. Kernel ownership means a crashed process releases
// the lock without leaving a stale sidecar behind.
func acquireLock(f *os.File) error {
	var overlapped windows.Overlapped
	err := windows.LockFileEx(
		windows.Handle(f.Fd()),
		windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY,
		0,
		math.MaxUint32,
		math.MaxUint32,
		&overlapped,
	)
	if err == nil {
		return nil
	}
	if errors.Is(err, windows.ERROR_LOCK_VIOLATION) || errors.Is(err, windows.ERROR_IO_PENDING) {
		return ErrSessionLocked
	}
	return fmt.Errorf("locking session: %w", err)
}

func releaseLock(f *os.File) error {
	var overlapped windows.Overlapped
	return windows.UnlockFileEx(
		windows.Handle(f.Fd()),
		0,
		math.MaxUint32,
		math.MaxUint32,
		&overlapped,
	)
}
