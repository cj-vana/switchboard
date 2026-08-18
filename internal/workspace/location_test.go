package workspace

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestLocationReadVerifyAndStaleGuard(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "src", "hello.go")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("package hello\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	w, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	doc, err := w.Read("src/hello.go", 0)
	if err != nil {
		t.Fatal(err)
	}
	if doc.Location.Path != "src/hello.go" || doc.Location.Revision.Size != int64(len(doc.Content)) || doc.Mode.Perm() != 0o640 {
		t.Fatalf("document = %+v mode %v", doc.Location, doc.Mode)
	}
	if err := w.Verify(doc.Location); err != nil {
		t.Fatalf("fresh location refused: %v", err)
	}
	if err := os.WriteFile(path, []byte("package changed\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := w.Verify(doc.Location); !errors.Is(err, ErrStaleLocation) {
		t.Fatalf("Verify after change = %v", err)
	}
}

func TestRootRefusesSymlinkEscapeAndBinary(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "secret"), []byte("nope"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "escape")); err != nil {
		if runtime.GOOS == "windows" {
			t.Skipf("symlink unavailable: %v", err)
		}
		t.Fatal(err)
	}
	w, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Read("escape/secret", 0); !errors.Is(err, ErrOutsideRoot) {
		t.Fatalf("escape read = %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "binary"), []byte{'x', 0, 'y'}, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Read("binary", 0); !errors.Is(err, ErrBinary) {
		t.Fatalf("binary read = %v", err)
	}
}
