//go:build windows

package extensions

import (
	"os"
	"path/filepath"
)

// Windows rename semantics already reject an existing destination.
func publishInstall(_ *os.File, root *os.Root, sourceRel, destinationRel string) error {
	return root.Rename(filepath.FromSlash(sourceRel), filepath.FromSlash(destinationRel))
}
