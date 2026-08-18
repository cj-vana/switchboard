//go:build windows

package session

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestWindowsLockFileExRejectsASecondWriterAndReleases(t *testing.T) {
	path := filepath.Join(t.TempDir(), "session.log")
	first, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	second, err := os.OpenFile(path, os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()

	if err := acquireLock(first); err != nil {
		t.Fatal(err)
	}
	if err := acquireLock(second); !errors.Is(err, ErrSessionLocked) {
		t.Fatalf("second lock err = %v, want ErrSessionLocked", err)
	}
	if err := releaseLock(first); err != nil {
		t.Fatal(err)
	}
	if err := acquireLock(second); err != nil {
		t.Fatalf("lock did not release: %v", err)
	}
	if err := releaseLock(second); err != nil {
		t.Fatal(err)
	}
}
