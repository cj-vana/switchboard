//go:build !aix && !darwin && !dragonfly && !freebsd && !illumos && !linux && !netbsd && !openbsd && !solaris && !windows

package mcpnative

import "os"

// Unknown platforms are disabled rather than using a potentially blocking
// plain open after the pre-open metadata check. This preserves the bounded
// discovery contract until the platform has a no-follow/special-file-safe
// implementation.
func openConfigFile(path string) (*os.File, error) {
	return nil, os.ErrPermission
}
