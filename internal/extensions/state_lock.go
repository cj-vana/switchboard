package extensions

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"time"
)

const pluginStateLockTimeout = 2 * time.Second

type pluginStateLock struct {
	file *os.File
}

func acquirePluginStateLock(ctx context.Context, statePath string) (*pluginStateLock, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := os.MkdirAll(filepath.Dir(statePath), 0o700); err != nil {
		return nil, fmt.Errorf("creating plugin state directory: %w", err)
	}
	path := statePath + ".lock"
	before, err := os.Lstat(path)
	if err != nil && !os.IsNotExist(err) {
		return nil, fmt.Errorf("inspecting plugin state lock: %w", err)
	}
	if err == nil && (before.Mode()&os.ModeSymlink != 0 || !before.Mode().IsRegular()) {
		return nil, errors.New("plugin state lock is not a regular file")
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("opening plugin state lock: %w", err)
	}
	closeWith := func(err error) (*pluginStateLock, error) {
		_ = file.Close()
		return nil, err
	}
	after, err := file.Stat()
	if err != nil {
		return closeWith(fmt.Errorf("inspecting opened plugin state lock: %w", err))
	}
	current, err := os.Lstat(path)
	if err != nil || current.Mode()&os.ModeSymlink != 0 || !current.Mode().IsRegular() || !os.SameFile(current, after) {
		return closeWith(errors.New("plugin state lock changed while it was opened"))
	}
	if before != nil && !os.SameFile(before, after) {
		return closeWith(errors.New("plugin state lock changed while it was opened"))
	}
	if runtime.GOOS != "windows" && after.Mode().Perm()&0o077 != 0 {
		return closeWith(fmt.Errorf("plugin state lock permissions are %04o, want 0600", after.Mode().Perm()))
	}
	deadline := time.Now().Add(pluginStateLockTimeout)
	for {
		if err := ctx.Err(); err != nil {
			return closeWith(err)
		}
		acquired, lockErr := tryPluginStateFileLock(file)
		if lockErr != nil {
			return closeWith(fmt.Errorf("locking plugin state: %w", lockErr))
		}
		if acquired {
			if err := ctx.Err(); err != nil {
				_ = unlockPluginStateFile(file)
				return closeWith(err)
			}
			return &pluginStateLock{file: file}, nil
		}
		if time.Now().After(deadline) {
			return closeWith(errors.New("plugin state is busy; timed out waiting for its lock"))
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func (lock *pluginStateLock) Close() error {
	if lock == nil || lock.file == nil {
		return nil
	}
	unlockErr := unlockPluginStateFile(lock.file)
	closeErr := lock.file.Close()
	lock.file = nil
	if unlockErr != nil {
		return unlockErr
	}
	return closeErr
}
