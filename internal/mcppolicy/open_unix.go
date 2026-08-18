//go:build aix || darwin || dragonfly || freebsd || illumos || linux || netbsd || openbsd || solaris

package mcppolicy

import (
	"os"

	"golang.org/x/sys/unix"
)

func openPolicyFile(name string) (*os.File, error) {
	return openPolicyPath(name, false)
}

func openPolicyDirectory(name string) (*os.File, error) {
	return openPolicyPath(name, true)
}

func openPolicyPath(name string, directory bool) (*os.File, error) {
	flags := unix.O_RDONLY | unix.O_CLOEXEC | unix.O_NONBLOCK | unix.O_NOFOLLOW
	if directory {
		flags |= unix.O_DIRECTORY
	}
	fd, err := unix.Open(name, flags, 0)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), name)
	if file == nil {
		_ = unix.Close(fd)
		return nil, os.ErrInvalid
	}
	return file, nil
}
