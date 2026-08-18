//go:build linux

package extensions

import (
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

func publishInstall(directory *os.File, _ *os.Root, sourceRel, destinationRel string) error {
	fd := int(directory.Fd())
	return unix.Renameat2(
		fd,
		filepath.FromSlash(sourceRel),
		fd,
		filepath.FromSlash(destinationRel),
		unix.RENAME_NOREPLACE,
	)
}
