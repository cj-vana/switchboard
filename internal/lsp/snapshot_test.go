package lsp

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReadDocumentSnapshotBoundsTypeSizeEncodingAndContext(t *testing.T) {
	root := t.TempDir()

	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := readDocumentSnapshot(canceled, filepath.Join(root, "missing")); !errors.Is(err, context.Canceled) {
		t.Fatalf("pre-canceled snapshot error = %v", err)
	}

	if _, err := readDocumentSnapshot(context.Background(), root); err == nil || !strings.Contains(err.Error(), "not a regular file") {
		t.Fatalf("directory snapshot error = %v", err)
	}

	invalid := filepath.Join(root, "invalid.go")
	if err := os.WriteFile(invalid, []byte{0xff, 0xfe}, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := readDocumentSnapshot(context.Background(), invalid); err == nil || !strings.Contains(err.Error(), "not valid UTF-8") {
		t.Fatalf("invalid UTF-8 snapshot error = %v", err)
	}

	huge := filepath.Join(root, "huge.go")
	file, err := os.Create(huge)
	if err != nil {
		t.Fatal(err)
	}
	if err := file.Truncate(maxDocumentBytes + 1); err != nil {
		file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := readDocumentSnapshot(context.Background(), huge); err == nil || !strings.Contains(err.Error(), "document limit") {
		t.Fatalf("oversized snapshot error = %v", err)
	}
}

func TestWriteRejectsJSONExpandedFramesBeforeWriting(t *testing.T) {
	writer := &failFirstWriteCloser{}
	c := &Client{in: writer}
	payload := bytes.Repeat([]byte{'x'}, maxLSPMessageBytes+1)
	if err := c.write(payload); err == nil || !strings.Contains(err.Error(), "request is") {
		t.Fatalf("oversized write error = %v", err)
	}
	if writer.Len() != 0 {
		t.Fatalf("oversized payload wrote %d bytes before rejection", writer.Len())
	}
}

func TestReadDocumentSnapshotFeedsOneImmutableByteSlice(t *testing.T) {
	path := filepath.Join(t.TempDir(), "a.go")
	want := []byte("package a\nvar 😀Thing = 1\n")
	if err := os.WriteFile(path, want, 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := readDocumentSnapshot(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("snapshot = %q, want %q", got, want)
	}
	if err := os.WriteFile(path, []byte("changed later"), 0o644); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("returned snapshot aliased later disk content: %q", got)
	}
}
