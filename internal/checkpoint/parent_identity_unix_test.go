//go:build !windows

package checkpoint

import (
	"crypto/sha256"
	"errors"
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
	snapshot := r.Snapshots()[0].Files[0]
	if _, readErr := r.ReadSnapshotCurrent(snapshot); !errors.Is(readErr, ErrStale) {
		t.Fatalf("review read through replaced parent error=%v, want ErrStale", readErr)
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

func TestSnapshotReadRefusesReplacedRegularParent(t *testing.T) {
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
	snapshot := r.Snapshots()[0].Files[0]

	if err := os.Rename(dir, filepath.Join(root, "moved")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	write(t, path, "after")
	if _, err := r.ReadSnapshotCurrent(snapshot); !errors.Is(err, ErrStale) {
		t.Fatalf("replacement parent read error=%v, want ErrStale", err)
	}
	if got := readBack(t, path); got != "after" {
		t.Fatalf("review changed replacement file to %q", got)
	}
}

func TestUndoRefusesReplacedAncestorSymlinkToCapturedParent(t *testing.T) {
	workspace := t.TempDir()
	ancestor := filepath.Join(workspace, "ancestor")
	parent := filepath.Join(ancestor, "parent")
	if err := os.MkdirAll(parent, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(parent, "file.txt")
	write(t, path, "before")

	r := NewRecorder()
	r.Begin("edit")
	r.RecordState(path, true, 0o644, []byte("before"))
	write(t, path, "after")
	r.Commit(path, true, 0o644, sha256.Sum256([]byte("after")))
	snapshot := r.Snapshots()[0].Files[0]

	outside := t.TempDir()
	moved := filepath.Join(outside, "moved")
	if err := os.Rename(ancestor, moved); err != nil {
		t.Skipf("cannot move test ancestor on this filesystem: %v", err)
	}
	if err := os.Symlink(moved, ancestor); err != nil {
		t.Fatal(err)
	}
	outsidePath := filepath.Join(moved, "parent", "file.txt")
	if _, err := r.ReadSnapshotCurrent(snapshot); !errors.Is(err, ErrStale) {
		t.Fatalf("ancestor-symlink review error=%v, want ErrStale", err)
	}
	if _, _, err := r.UndoFile(path); !errors.Is(err, ErrStale) {
		t.Fatalf("ancestor-symlink undo error=%v, want ErrStale", err)
	}
	if got := readBack(t, outsidePath); got != "after" {
		t.Fatalf("undo escaped through ancestor symlink: outside=%q", got)
	}
}

func TestFingerprintPathRebindsNameAfterHash(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "target.txt")
	moved := filepath.Join(dir, "opened-inode.txt")
	write(t, path, "recorded")

	_, err := fingerprintPathWithHook(path, func() {
		if renameErr := os.Rename(path, moved); renameErr != nil {
			t.Fatal(renameErr)
		}
		write(t, path, "replacement")
	})
	if !errors.Is(err, ErrStale) {
		t.Fatalf("fingerprint replacement error=%v, want ErrStale", err)
	}
	if got := readBack(t, path); got != "replacement" {
		t.Fatalf("fingerprint changed replacement to %q", got)
	}
}
