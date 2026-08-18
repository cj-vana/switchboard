//go:build aix || darwin || dragonfly || freebsd || illumos || linux || netbsd || openbsd || solaris

package mcpnative

import (
	"os"

	"golang.org/x/sys/unix"
)

// openConfigFile uses nonblocking and no-follow flags so a hostile project
// cannot swap a regular config for a FIFO or final-component symlink and hang
// discovery between metadata validation and open.
func openConfigFile(path string) (*os.File, error) {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NONBLOCK|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), path)
	if file == nil {
		_ = unix.Close(fd)
		return nil, os.ErrInvalid
	}
	return file, nil
}
