//go:build aix || darwin || dragonfly || freebsd || illumos || linux || netbsd || openbsd || solaris

package mcpnative

import (
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestFIFOAtNativeConfigPathCannotBlockDiscovery(t *testing.T) {
	root := t.TempDir()
	home := filepath.Join(root, "home")
	mustMkdir(t, home)
	path := filepath.Join(home, ".claude.json")
	if err := unix.Mkfifo(path, 0o600); err != nil {
		t.Fatal(err)
	}
	done := make(chan Result, 1)
	go func() {
		done <- Discover(Options{HomeDir: home})
	}()
	select {
	case result := <-done:
		if !hasDiagnostic(result, "non-regular-config") {
			t.Fatalf("FIFO was not refused: %+v", result.Diagnostics)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("discovery blocked while opening a FIFO")
	}
}
