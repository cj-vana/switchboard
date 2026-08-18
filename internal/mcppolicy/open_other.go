//go:build !aix && !darwin && !dragonfly && !freebsd && !illumos && !linux && !netbsd && !openbsd && !solaris

package mcppolicy

import "os"

func openPolicyFile(name string) (*os.File, error) {
	return os.Open(name)
}

func openPolicyDirectory(name string) (*os.File, error) {
	return os.Open(name)
}
