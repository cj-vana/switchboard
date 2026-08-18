//go:build !unix && !windows

package main

import (
	"errors"
	"os"
)

func openNativeMCPStateDataFile(path string) (*os.File, error) { return os.Open(path) }

func openNativeMCPStateLockFile(string) (*os.File, error) {
	return nil, errors.New("cross-process native MCP state locking is unsupported on this platform")
}

func tryNativeMCPStateFileLock(*os.File) (bool, error) { return false, errors.New("unsupported") }
func unlockNativeMCPStateFileLock(*os.File) error      { return nil }
func replaceNativeMCPStateFile(staged, target string) error {
	return os.Rename(staged, target)
}
