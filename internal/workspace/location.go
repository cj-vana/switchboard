// Package workspace provides the revision-aware, workspace-contained file
// identity used by human-facing editor features. A Location is useful only
// together with the exact bytes it was derived from: callers must Verify it
// before turning a viewed range into an attachment or mutation.
package workspace

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"
)

const DefaultDocumentLimit int64 = 4 << 20

var (
	ErrOutsideRoot   = errors.New("path is outside the workspace")
	ErrStaleLocation = errors.New("source location is stale")
	ErrBinary        = errors.New("file is binary")
	ErrTooLarge      = errors.New("file is too large")
)

// Position is a human-facing, one-based line and column. LSP wire positions
// intentionally use their own zero-based UTF-16 type in internal/lsp.
type Position struct {
	Line   int `json:"line"`
	Column int `json:"column"`
}

// Range is half-open. A zero End means the location names the whole line or
// file, depending on the surface presenting it.
type Range struct {
	Start Position `json:"start"`
	End   Position `json:"end,omitempty"`
}

// Revision is the exact regular-file snapshot behind a view. Size is carried
// for useful diagnostics; SHA256 is the authority.
type Revision struct {
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
}

// Location is workspace-relative and slash-normalized. It never carries an
// absolute host path into a transcript or provider request.
type Location struct {
	Path     string   `json:"path"`
	Range    Range    `json:"range,omitempty"`
	Revision Revision `json:"revision"`
}

func (l Location) String() string {
	if l.Range.Start.Line <= 0 {
		return l.Path
	}
	if l.Range.Start.Column <= 0 {
		return fmt.Sprintf("%s:%d", l.Path, l.Range.Start.Line)
	}
	return fmt.Sprintf("%s:%d:%d", l.Path, l.Range.Start.Line, l.Range.Start.Column)
}

// Document is one immutable source snapshot.
type Document struct {
	Location Location `json:"location"`
	Content  []byte   `json:"-"`
	Mode     fs.FileMode
}

// Root is a canonical workspace boundary.
type Root struct {
	path string
}

func Open(root string) (*Root, error) {
	if strings.TrimSpace(root) == "" {
		return nil, errors.New("workspace root is required")
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	real, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return nil, fmt.Errorf("resolving workspace root: %w", err)
	}
	info, err := os.Stat(real)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("workspace root %s is not a directory", root)
	}
	return &Root{path: filepath.Clean(real)}, nil
}

func (r *Root) Path() string { return r.path }

// Resolve follows existing symlinks before applying the workspace boundary.
// The target must exist; editor views never create paths.
func (r *Root) Resolve(name string) (string, error) {
	if strings.TrimSpace(name) == "" {
		return "", errors.New("path is required")
	}
	candidate := name
	if !filepath.IsAbs(candidate) {
		candidate = filepath.Join(r.path, filepath.FromSlash(candidate))
	}
	real, err := filepath.EvalSymlinks(filepath.Clean(candidate))
	if err != nil {
		return "", err
	}
	if !within(r.path, real) {
		return "", fmt.Errorf("%w: %s", ErrOutsideRoot, name)
	}
	return real, nil
}

func (r *Root) Relative(abs string) (string, error) {
	real, err := filepath.EvalSymlinks(filepath.Clean(abs))
	if err != nil {
		return "", err
	}
	if !within(r.path, real) {
		return "", fmt.Errorf("%w: %s", ErrOutsideRoot, abs)
	}
	rel, err := filepath.Rel(r.path, real)
	if err != nil {
		return "", err
	}
	return filepath.ToSlash(rel), nil
}

func within(root, candidate string) bool {
	rel, err := filepath.Rel(root, candidate)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// Read returns an exact, bounded regular-file snapshot. Binary data is
// refused instead of being painted as source text.
func (r *Root) Read(name string, limit int64) (Document, error) {
	if limit <= 0 {
		limit = DefaultDocumentLimit
	}
	abs, err := r.Resolve(name)
	if err != nil {
		return Document{}, err
	}
	file, err := os.Open(abs)
	if err != nil {
		return Document{}, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return Document{}, err
	}
	if !info.Mode().IsRegular() {
		return Document{}, fmt.Errorf("%s is not a regular file", name)
	}
	if info.Size() > limit {
		return Document{}, fmt.Errorf("%w: %s is %d bytes (limit %d)", ErrTooLarge, name, info.Size(), limit)
	}
	data, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil {
		return Document{}, err
	}
	if int64(len(data)) > limit {
		return Document{}, fmt.Errorf("%w: %s exceeds %d bytes", ErrTooLarge, name, limit)
	}
	afterFD, err := file.Stat()
	if err != nil {
		return Document{}, err
	}
	afterPath, err := os.Stat(abs)
	if err != nil {
		return Document{}, err
	}
	if int64(len(data)) != info.Size() || info.Size() != afterFD.Size() ||
		!info.ModTime().Equal(afterFD.ModTime()) || !os.SameFile(afterFD, afterPath) {
		return Document{}, fmt.Errorf("%w: %s changed while it was read", ErrStaleLocation, name)
	}
	if bytes.IndexByte(data, 0) >= 0 || !utf8.Valid(data) {
		return Document{}, fmt.Errorf("%w: %s", ErrBinary, name)
	}
	rel, err := r.Relative(abs)
	if err != nil {
		return Document{}, err
	}
	revision := revision(data)
	return Document{
		Location: Location{Path: rel, Revision: revision},
		Content:  data,
		Mode:     info.Mode(),
	}, nil
}

// Verify proves that a location still names the bytes that were viewed.
func (r *Root) Verify(location Location) error {
	doc, err := r.Read(location.Path, max(DefaultDocumentLimit, location.Revision.Size))
	if err != nil {
		return err
	}
	if doc.Location.Revision != location.Revision {
		return fmt.Errorf("%w: %s changed from %s to %s", ErrStaleLocation, location.Path,
			location.Revision.SHA256, doc.Location.Revision.SHA256)
	}
	return nil
}

func revision(data []byte) Revision {
	sum := sha256.Sum256(data)
	return Revision{SHA256: hex.EncodeToString(sum[:]), Size: int64(len(data))}
}
