//go:build !windows

package checkpoint

import (
	"crypto/sha256"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestUndoRefusesReplacedParentSymlink(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "dir")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "file.txt")
	write(t, path, "before")

	r := NewRecorder()
	r.Begin("edit")
	r.RecordState(path, true, 0o644, []byte("before"))
	write(t, path, "after")
	r.Commit(path, true, 0o644, sha256.Sum256([]byte("after")))

	// Move the captured directory away, then make the original pathname
	// resolve to an outside directory holding an identical post-image. A
	// content-only CAS would accept it and overwrite outside/file.txt.
	moved := filepath.Join(root, "moved")
	if err := os.Rename(dir, moved); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	outsideFile := filepath.Join(outside, "file.txt")
	write(t, outsideFile, "after")
	if err := os.Symlink(outside, dir); err != nil {
		t.Fatal(err)
	}

	restored, removed, _, failed, _, err := r.Undo()
	if err != nil {
		t.Fatal(err)
	}
	if len(restored) != 0 || len(removed) != 0 || len(failed) != 1 || !strings.Contains(failed[0], "parent") {
		t.Fatalf("restored=%v removed=%v failed=%v", restored, removed, failed)
	}
	if got := readBack(t, outsideFile); got != "after" {
		t.Fatalf("undo escaped through the replacement symlink: outside=%q", got)
	}
	if got := readBack(t, filepath.Join(moved, "file.txt")); got != "after" {
		t.Fatalf("the moved original directory was unexpectedly changed: %q", got)
	}
	if details := r.Details(); len(details) != 1 || len(details[0].Paths) != 1 || details[0].Paths[0] != path {
		t.Fatalf("refused capture was consumed: %+v", details)
	}
}
