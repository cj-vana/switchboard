package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"time"
)

const (
	nativeMCPStateLockWait = 5 * time.Second
	nativeMCPStateLockPoll = 10 * time.Millisecond
)

type nativeMCPStateFileLock struct {
	path string
	file *os.File
}

// acquireNativeMCPStateFileLock serializes read-modify-write state updates
// across Switchboard processes. The sidecar is permanent: unlinking a held
// lock would let a second process lock a different inode under the same name.
func acquireNativeMCPStateFileLock(ctx context.Context, statePath string) (*nativeMCPStateFileLock, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	directory := filepath.Dir(statePath)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return nil, fmt.Errorf("creating %s: %w", directory, err)
	}
	path := statePath + ".lock"
	file, err := openNativeMCPStateLockFile(path)
	if err != nil {
		return nil, fmt.Errorf("opening native MCP state lock %s: %w", path, err)
	}
	lock := &nativeMCPStateFileLock{path: path, file: file}
	if err := validateNativeMCPStateLockFile(file, path); err != nil {
		_ = file.Close()
		return nil, err
	}

	waitCtx, cancel := context.WithTimeout(ctx, nativeMCPStateLockWait)
	defer cancel()
	for {
		acquired, lockErr := tryNativeMCPStateFileLock(file)
		if lockErr != nil {
			_ = file.Close()
			return nil, fmt.Errorf("locking native MCP state %s: %w", statePath, lockErr)
		}
		if acquired {
			return lock, nil
		}
		timer := time.NewTimer(nativeMCPStateLockPoll)
		select {
		case <-waitCtx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			_ = file.Close()
			return nil, fmt.Errorf("locking native MCP state %s: %w", statePath, waitCtx.Err())
		case <-timer.C:
		}
	}
}

func validateNativeMCPStateLockFile(file *os.File, path string) error {
	info, err := file.Stat()
	if err != nil {
		return fmt.Errorf("reading native MCP state lock %s: %w", path, err)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("reading native MCP state lock %s: lock is not a regular file", path)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("reading native MCP state lock %s: permissions are %04o, want 0600", path, info.Mode().Perm())
	}
	return nil
}

func (l *nativeMCPStateFileLock) Close() error {
	if l == nil || l.file == nil {
		return nil
	}
	unlockErr := unlockNativeMCPStateFileLock(l.file)
	closeErr := l.file.Close()
	l.file = nil
	return errors.Join(unlockErr, closeErr)
}
