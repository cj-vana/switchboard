//go:build windows

package checkpoint

import (
	"golang.org/x/sys/windows"
)

func replacePath(from, to string) error {
	fromp, err := windows.UTF16PtrFromString(from)
	if err != nil {
		return err
	}
	top, err := windows.UTF16PtrFromString(to)
	if err != nil {
		return err
	}
	return windows.MoveFileEx(fromp, top, windows.MOVEFILE_REPLACE_EXISTING|windows.MOVEFILE_WRITE_THROUGH)
}

// The temporary file itself is flushed before publication. Windows does not
// expose directory handles through os.Open, and MoveFileEx's WRITE_THROUGH
// flag supplies the corresponding rename durability guarantee.
func syncDirectory(string) error { return nil }
