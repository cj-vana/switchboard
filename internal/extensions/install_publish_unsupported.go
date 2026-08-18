//go:build !darwin && !linux && !windows

package extensions

import (
	"errors"
	"os"
)

func publishInstall(_ *os.File, _ *os.Root, _, _ string) error {
	return errors.New("atomic no-replace plugin installation is unsupported on this platform")
}
