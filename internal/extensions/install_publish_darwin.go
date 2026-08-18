//go:build darwin

package extensions

import (
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

func publishInstall(directory *os.File, _ *os.Root, sourceRel, destinationRel string) error {
	fd := int(directory.Fd())
	return unix.RenameatxNp(
		fd,
		filepath.FromSlash(sourceRel),
		fd,
		filepath.FromSlash(destinationRel),
		unix.RENAME_EXCL,
	)
}
