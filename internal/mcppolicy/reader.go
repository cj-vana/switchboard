package mcppolicy

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

type policyFile struct {
	path     string
	realPath string
	data     []byte
}

// safeReadError deliberately carries only a stable code and generic message.
// Policy bytes and operating-system errors can contain credentials or other
// managed values, so neither is retained or rendered.
type safeReadError struct {
	code    string
	message string
}

func (e *safeReadError) Error() string { return e.message }

type readBudget struct {
	files    int
	bytes    int64
	attempts int
}

func (b *readBudget) document(document Document) ([]byte, error) {
	b.attempts++
	size := int64(len(document.contents))
	if b.attempts > MaxPolicyFiles*2 || b.files >= MaxPolicyFiles || size > MaxPolicyFileBytes || size > MaxPolicyBytes-b.bytes {
		return nil, newReadError("policy-budget-exceeded", "native MCP policy discovery exceeds its aggregate file or byte budget")
	}
	b.files++
	b.bytes += size
	return append([]byte(nil), document.contents...), nil
}

func (b *readBudget) read(path, allowedRoot string) (policyFile, bool, error) {
	if b.attempts >= MaxPolicyFiles*2 {
		return policyFile{}, false, newReadError("policy-budget-exceeded", "native MCP policy discovery exceeds its candidate budget")
	}
	b.attempts++
	if b.files >= MaxPolicyFiles || b.bytes >= MaxPolicyBytes {
		return policyFile{}, false, newReadError("policy-budget-exceeded", "native MCP policy discovery exceeds its aggregate file or byte budget")
	}
	file, found, err := readPolicyFile(path, allowedRoot, MaxPolicyBytes-b.bytes)
	if err != nil || !found {
		return file, found, err
	}
	b.files++
	b.bytes += int64(len(file.data))
	return file, true, nil
}

func newReadError(code, message string) *safeReadError {
	return &safeReadError{code: code, message: message}
}

func readPolicyFile(name, allowedRoot string, remaining int64) (policyFile, bool, error) {
	abs, err := filepath.Abs(name)
	if err != nil {
		return policyFile{}, false, newReadError("invalid-policy-path", "native MCP policy path cannot be made absolute")
	}
	abs = filepath.Clean(abs)
	if _, err := os.Lstat(abs); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return policyFile{}, false, nil
		}
		return policyFile{}, false, newReadError("unreadable-policy", "native MCP policy metadata could not be read")
	}
	if remaining < 0 {
		return policyFile{}, false, newReadError("policy-budget-exceeded", "native MCP policy discovery exceeds its aggregate byte budget")
	}

	real, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return policyFile{}, false, newReadError("unreadable-policy", "native MCP policy symlinks could not be resolved")
	}
	real = filepath.Clean(real)
	if allowedRoot != "" && !withinRoot(allowedRoot, real) {
		return policyFile{}, false, newReadError("policy-escapes-root", "native MCP policy resolves outside its authorized root")
	}
	before, err := os.Lstat(real)
	if err != nil {
		return policyFile{}, false, newReadError("unreadable-policy", "native MCP policy metadata could not be read")
	}
	if !before.Mode().IsRegular() {
		return policyFile{}, false, newReadError("non-regular-policy", "native MCP policy path is not a regular file")
	}
	if before.Size() > MaxPolicyFileBytes {
		return policyFile{}, false, newReadError("policy-too-large", "native MCP policy exceeds the bounded read limit")
	}
	if before.Size() > remaining {
		return policyFile{}, false, newReadError("policy-budget-exceeded", "native MCP policy discovery exceeds its aggregate byte budget")
	}

	file, err := openPolicyFile(real)
	if err != nil {
		return policyFile{}, false, newReadError("unreadable-policy", "native MCP policy could not be opened")
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil {
		return policyFile{}, false, newReadError("unreadable-policy", "native MCP policy metadata could not be read")
	}
	if !opened.Mode().IsRegular() {
		return policyFile{}, false, newReadError("non-regular-policy", "native MCP policy path is not a regular file")
	}
	if !os.SameFile(before, opened) {
		return policyFile{}, false, newReadError("policy-changed-during-read", "native MCP policy changed while it was opened")
	}
	if _, err := verifyPolicyPath(abs, allowedRoot, opened); err != nil {
		return policyFile{}, false, err
	}

	limit := MaxPolicyFileBytes
	if remaining < limit {
		limit = remaining
	}
	data, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil {
		return policyFile{}, false, newReadError("unreadable-policy", "native MCP policy could not be read")
	}
	if int64(len(data)) > MaxPolicyFileBytes {
		return policyFile{}, false, newReadError("policy-too-large", "native MCP policy exceeds the bounded read limit")
	}
	if int64(len(data)) > remaining {
		return policyFile{}, false, newReadError("policy-budget-exceeded", "native MCP policy discovery exceeds its aggregate byte budget")
	}
	after, err := file.Stat()
	if err != nil || !os.SameFile(opened, after) || opened.Size() != after.Size() || !opened.ModTime().Equal(after.ModTime()) {
		return policyFile{}, false, newReadError("policy-changed-during-read", "native MCP policy changed while it was read")
	}
	finalReal, err := verifyPolicyPath(abs, allowedRoot, after)
	if err != nil {
		return policyFile{}, false, err
	}
	return policyFile{path: abs, realPath: finalReal, data: data}, true, nil
}

func verifyPolicyPath(name, allowedRoot string, opened os.FileInfo) (string, error) {
	real, err := filepath.EvalSymlinks(name)
	if err != nil {
		return "", newReadError("policy-changed-during-read", "native MCP policy path changed while it was read")
	}
	real = filepath.Clean(real)
	info, err := os.Stat(real)
	if err != nil || !os.SameFile(opened, info) {
		return "", newReadError("policy-changed-during-read", "native MCP policy path changed while it was read")
	}
	if allowedRoot != "" && !withinRoot(allowedRoot, real) {
		return "", newReadError("policy-escapes-root", "native MCP policy resolves outside its authorized root")
	}
	return real, nil
}

// readDropIns opens and lists the managed drop-in directory through a stable
// descriptor, then reads each visible *.json file with the same bounded,
// no-follow, identity-checked path used for single policy files.
func (b *readBudget) readDropIns(directory, allowedRoot string) ([]policyFile, bool, error) {
	abs, err := filepath.Abs(directory)
	if err != nil {
		return nil, false, newReadError("invalid-policy-path", "managed settings directory cannot be made absolute")
	}
	abs = filepath.Clean(abs)
	if _, err := os.Lstat(abs); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, false, nil
		}
		return nil, false, newReadError("unreadable-policy", "managed settings directory metadata could not be read")
	}
	real, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return nil, false, newReadError("unreadable-policy", "managed settings directory symlinks could not be resolved")
	}
	real = filepath.Clean(real)
	if allowedRoot != "" && !withinRoot(allowedRoot, real) {
		return nil, false, newReadError("policy-escapes-root", "managed settings directory resolves outside its authorized root")
	}
	directoryFile, err := openPolicyDirectory(real)
	if err != nil {
		return nil, false, newReadError("unreadable-policy", "managed settings directory could not be opened")
	}
	defer directoryFile.Close()
	opened, err := directoryFile.Stat()
	if err != nil || !opened.IsDir() {
		return nil, false, newReadError("non-directory-policy", "managed settings drop-in path is not a directory")
	}
	if err := verifyDirectoryPath(abs, allowedRoot, opened); err != nil {
		return nil, false, err
	}
	entries, err := directoryFile.ReadDir(MaxPolicyFiles*2 + 1)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, false, newReadError("unreadable-policy", "managed settings directory could not be listed")
	}
	if len(entries) > MaxPolicyFiles*2 {
		return nil, false, newReadError("policy-budget-exceeded", "managed settings directory exceeds its candidate budget")
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		name := entry.Name()
		if strings.HasPrefix(name, ".") || !strings.HasSuffix(name, ".json") {
			continue
		}
		names = append(names, name)
	}
	sort.Strings(names)
	files := make([]policyFile, 0, len(names))
	for _, name := range names {
		file, found, readErr := b.read(filepath.Join(abs, name), real)
		if readErr != nil {
			return nil, true, readErr
		}
		if !found {
			return nil, true, newReadError("policy-changed-during-read", "managed settings drop-in changed while it was read")
		}
		files = append(files, file)
	}
	if err := verifyDirectoryPath(abs, allowedRoot, opened); err != nil {
		return nil, true, err
	}
	return files, true, nil
}

func verifyDirectoryPath(name, allowedRoot string, opened os.FileInfo) error {
	real, err := filepath.EvalSymlinks(name)
	if err != nil {
		return newReadError("policy-changed-during-read", "managed settings directory changed while it was read")
	}
	real = filepath.Clean(real)
	info, err := os.Stat(real)
	if err != nil || !info.IsDir() || !os.SameFile(opened, info) {
		return newReadError("policy-changed-during-read", "managed settings directory changed while it was read")
	}
	if allowedRoot != "" && !withinRoot(allowedRoot, real) {
		return newReadError("policy-escapes-root", "managed settings directory resolves outside its authorized root")
	}
	return nil
}

func withinRoot(root, candidate string) bool {
	rootAbs, err := filepath.Abs(root)
	if err != nil {
		return false
	}
	rel, err := filepath.Rel(filepath.Clean(rootAbs), filepath.Clean(candidate))
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}

func policyRoot(name string) string {
	if strings.TrimSpace(name) == "" {
		return ""
	}
	return filepath.Dir(filepath.Clean(name))
}

func surfacePresent(name string) (bool, error) {
	if strings.TrimSpace(name) == "" {
		return false, nil
	}
	_, err := os.Lstat(name)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	return true, fmt.Errorf("managed policy surface cannot be inspected")
}
