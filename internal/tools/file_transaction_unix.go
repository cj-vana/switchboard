//go:build !windows

package tools

import "os"

func replaceMutationPath(from, to string) error {
	return os.Rename(from, to)
}

func syncMutationDirectory(path string) error {
	dir, err := os.Open(path)
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}
