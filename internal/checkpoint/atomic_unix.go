//go:build !windows

package checkpoint

import "os"

func replacePath(from, to string) error {
	return os.Rename(from, to)
}

func syncDirectory(path string) error {
	dir, err := os.Open(path)
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}
