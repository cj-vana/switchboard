package lsp

import (
	"context"
	"fmt"
	"io"
	"os"
	"unicode/utf8"
)

// Keeping the raw document below half the frame cap leaves room for JSON
// escaping and request metadata. In particular, a hostile or accidental huge
// file cannot turn one semantic lookup into unbounded allocation.
const maxDocumentBytes = maxLSPMessageBytes / 2

// readDocumentSnapshot is the only disk-reader used by document sync. The one
// returned byte slice feeds both didOpen/didChange and position resolution.
func readDocumentSnapshot(ctx context.Context, path string) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%s is not a regular file", path)
	}
	if info.Size() > maxDocumentBytes {
		return nil, fmt.Errorf("%s is %d bytes; language-server document limit is %d", path, info.Size(), maxDocumentBytes)
	}

	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if !opened.Mode().IsRegular() {
		return nil, fmt.Errorf("%s stopped being a regular file before it was read", path)
	}
	if !os.SameFile(info, opened) {
		return nil, fmt.Errorf("%s was replaced before its language-server snapshot was read", path)
	}
	if opened.Size() > maxDocumentBytes {
		return nil, fmt.Errorf("%s is %d bytes; language-server document limit is %d", path, opened.Size(), maxDocumentBytes)
	}

	data, err := io.ReadAll(io.LimitReader(file, maxDocumentBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxDocumentBytes {
		return nil, fmt.Errorf("%s exceeds the %d-byte language-server document limit", path, maxDocumentBytes)
	}
	after, err := file.Stat()
	if err != nil {
		return nil, err
	}
	current, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if !os.SameFile(opened, current) || after.Size() != opened.Size() || !after.ModTime().Equal(opened.ModTime()) {
		return nil, fmt.Errorf("%s changed while its language-server snapshot was read", path)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !utf8.Valid(data) {
		return nil, fmt.Errorf("%s is not valid UTF-8", path)
	}
	return data, nil
}
