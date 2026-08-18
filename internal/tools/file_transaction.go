package tools

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// pathLocks serialize first-party mutations of one resolved path across every
// registry in the process. A registry-local lock would still allow a delegate
// or another session rooted at the same workspace to interleave a stale check
// and publication.
var pathLocks = struct {
	sync.Mutex
	locks map[string]*pathLock
}{locks: map[string]*pathLock{}}

type pathLock struct {
	mu   sync.Mutex
	refs int
}

func lockPath(path string) func() {
	pathLocks.Lock()
	l := pathLocks.locks[path]
	if l == nil {
		l = &pathLock{}
		pathLocks.locks[path] = l
	}
	l.refs++
	pathLocks.Unlock()

	l.mu.Lock()
	return func() {
		l.mu.Unlock()
		pathLocks.Lock()
		l.refs--
		if l.refs == 0 {
			delete(pathLocks.locks, path)
		}
		pathLocks.Unlock()
	}
}

type diskFile struct {
	existed bool
	mode    fs.FileMode
	content []byte
	digest  [sha256.Size]byte
	info    fs.FileInfo
}

func (f diskFile) sameContent(other diskFile) bool {
	if f.existed != other.existed {
		return false
	}
	if !f.existed {
		return true
	}
	return f.mode == other.mode && f.digest == other.digest
}

func readDiskFile(path string) (diskFile, error) {
	linfo, err := os.Lstat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return diskFile{}, nil
		}
		return diskFile{}, err
	}
	if !linfo.Mode().IsRegular() {
		return diskFile{}, fmt.Errorf("%s is not a regular file", path)
	}

	f, err := os.Open(path)
	if err != nil {
		return diskFile{}, err
	}
	defer f.Close()
	opened, err := f.Stat()
	if err != nil {
		return diskFile{}, err
	}
	if !os.SameFile(linfo, opened) {
		return diskFile{}, fmt.Errorf("%s changed identity while it was opened", path)
	}
	content, err := io.ReadAll(f)
	if err != nil {
		return diskFile{}, err
	}
	finished, err := f.Stat()
	if err != nil {
		return diskFile{}, err
	}
	if !os.SameFile(opened, finished) || opened.Size() != finished.Size() ||
		!opened.ModTime().Equal(finished.ModTime()) || restorableFileMode(opened.Mode()) != restorableFileMode(finished.Mode()) {
		return diskFile{}, fmt.Errorf("%s changed while it was being read", path)
	}
	return diskFile{
		existed: true,
		mode:    restorableFileMode(finished.Mode()),
		content: content,
		digest:  sha256.Sum256(content),
		info:    finished,
	}, nil
}

func restorableFileMode(mode fs.FileMode) fs.FileMode {
	return mode & (fs.ModePerm | fs.ModeSetuid | fs.ModeSetgid | fs.ModeSticky)
}

type fileMutation struct {
	r            *Registry
	abs          string
	before       diskFile
	readToken    string
	readTokenSet bool
	unlock       func()
	closed       bool
}

// prepareFileMutation takes the per-path lease, reads one exact source image,
// and enforces read-before-write against that same image. Validation such as
// exact edit matching happens while the lease remains held and before a
// checkpoint is prepared.
func (r *Registry) prepareFileMutation(abs string, allowMissing bool) (*fileMutation, Result, bool) {
	// Capture the caller's read token before waiting for another mutation of
	// this path. Two calls launched from the same source image must not let
	// the first call's success silently authorize the second call to overwrite
	// it. A genuinely sequential follow-up observes the updated token here.
	recorded, versionKnown := r.versions.get(abs)
	unlock := lockPath(abs)
	fail := func(format string, args ...any) (*fileMutation, Result, bool) {
		unlock()
		res, _ := errorf(format, args...)
		return nil, res, false
	}

	if err := validateResolvedTarget(r.root, abs); err != nil {
		return fail("cannot safely access %s: %v", r.display(abs), err)
	}
	before, err := readDiskFile(abs)
	if err != nil {
		return fail("cannot read %s: %v", r.display(abs), err)
	}
	if !before.existed {
		if !allowMissing {
			return fail("cannot read %s: file does not exist", r.display(abs))
		}
		return &fileMutation{
			r: r, abs: abs, before: before,
			readToken: recorded, readTokenSet: versionKnown, unlock: unlock,
		}, Result{}, true
	}

	if !versionKnown {
		return fail("%s exists but has not been read in this session. Read it first so "+
			"the change is made against its current contents.", r.display(abs))
	}
	if recorded != hashContent(before.content) {
		r.versions.forgetIf(abs, recorded)
		return fail("%s changed since it was read. Read it again before writing.", r.display(abs))
	}
	return &fileMutation{
		r: r, abs: abs, before: before,
		readToken: recorded, readTokenSet: versionKnown, unlock: unlock,
	}, Result{}, true
}

// forgetIf removes only the stale token this call actually relied on. A
// concurrent read may already have refreshed the path while this mutation was
// waiting for its lease; erasing that newer evidence would be safe but wrong.
func (v *fileVersions) forgetIf(path, stale string) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.seen[path] != stale {
		return
	}
	delete(v.seen, path)
	delete(v.whole, path)
}

func (m *fileMutation) close() {
	if m == nil || m.closed {
		return
	}
	m.closed = true
	m.unlock()
}

type publishResult struct {
	published bool
	after     diskFile
}

// publish atomically replaces the target after preparing its checkpoint.
// hook is test-only fault injection run after the durable temporary file is
// ready and before the source compare-and-swap check.
func (m *fileMutation) publish(ctx context.Context, content []byte, mode fs.FileMode, hook func()) error {
	mode = restorableFileMode(mode)
	if m.before.existed && bytes.Equal(m.before.content, content) && m.before.mode == mode {
		return nil
	}

	m.prepareCheckpoint()
	result, err := publishFile(ctx, m.r.root, m.abs, m.before, content, mode, hook)
	if result.published {
		m.commitCheckpoint(true, mode, sha256.Sum256(content))
	} else {
		m.abortCheckpoint()
	}
	if err != nil {
		// Publication errors can mean an external writer won the source CAS or
		// that verification after rename failed. In both cases retaining a
		// read token would make the next attempt less safe.
		if m.readTokenSet {
			m.r.versions.forgetIf(m.abs, m.readToken)
		}
		return err
	}
	m.r.versions.record(m.abs, hashContent(content))
	return nil
}

type exactStateCheckpointer interface {
	RecordState(abs string, existed bool, mode fs.FileMode, content []byte)
}

type lifecycleCheckpointer interface {
	Commit(abs string, existed bool, mode fs.FileMode, digest [sha256.Size]byte)
	Abort(abs string)
}

func (m *fileMutation) prepareCheckpoint() {
	if m.r.checkpoints == nil {
		return
	}
	if exact, ok := m.r.checkpoints.(exactStateCheckpointer); ok {
		exact.RecordState(m.abs, m.before.existed, m.before.mode, m.before.content)
		return
	}
	m.r.checkpoints.Record(m.abs)
}

func (m *fileMutation) commitCheckpoint(existed bool, mode fs.FileMode, digest [sha256.Size]byte) {
	if lifecycle, ok := m.r.checkpoints.(lifecycleCheckpointer); ok {
		lifecycle.Commit(m.abs, existed, mode, digest)
	}
}

func (m *fileMutation) abortCheckpoint() {
	if lifecycle, ok := m.r.checkpoints.(lifecycleCheckpointer); ok {
		lifecycle.Abort(m.abs)
	}
}

func publishFile(ctx context.Context, root, path string, before diskFile, content []byte, mode fs.FileMode, hook func()) (out publishResult, retErr error) {
	parentInfo, err := ensureSafeParent(root, filepath.Dir(path))
	if err != nil {
		return out, err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".switchboard-write-*")
	if err != nil {
		return out, err
	}
	tmpPath := tmp.Name()
	defer func() {
		_ = tmp.Close()
		if !out.published {
			_ = os.Remove(tmpPath)
		}
	}()
	if _, err := io.Copy(tmp, bytes.NewReader(content)); err != nil {
		return out, err
	}
	// Write first, chmod second: on Unix, writing may clear setuid/setgid
	// bits. Applying the captured mode after the bytes preserves it exactly.
	if err := tmp.Chmod(mode); err != nil {
		return out, err
	}
	if err := tmp.Sync(); err != nil {
		return out, err
	}
	if err := tmp.Close(); err != nil {
		return out, err
	}

	if hook != nil {
		hook()
	}
	if err := ctx.Err(); err != nil {
		return out, err
	}
	currentParent, err := ensureSafeParent(root, filepath.Dir(path))
	if err != nil {
		return out, err
	}
	if !os.SameFile(parentInfo, currentParent) {
		return out, fmt.Errorf("parent directory for %s changed identity before commit", path)
	}
	current, err := readDiskFile(path)
	if err != nil {
		return out, err
	}
	if !sameSource(before, current) {
		return out, fmt.Errorf("%s changed before commit; refusing to overwrite it", path)
	}
	if err := replaceMutationPath(tmpPath, path); err != nil {
		return out, err
	}
	out.published = true
	if err := syncMutationDirectory(filepath.Dir(path)); err != nil {
		return out, err
	}
	after, err := readDiskFile(path)
	if err != nil {
		return out, err
	}
	want := diskFile{existed: true, mode: mode, content: content, digest: sha256.Sum256(content)}
	if !want.sameContent(after) {
		return out, fmt.Errorf("verifying %s after atomic replace: post-image mismatch", path)
	}
	out.after = after
	return out, nil
}

func sameSource(before, current diskFile) bool {
	if !before.sameContent(current) {
		return false
	}
	if !before.existed {
		return true
	}
	return before.info != nil && current.info != nil && os.SameFile(before.info, current.info)
}

// ensureSafeParent creates missing directories one component at a time from
// the already-resolved workspace root and refuses symlinks. It then resolves
// the finished path again, so a path swapped between Plan and Run cannot turn
// a creation into a write outside the workspace.
func ensureSafeParent(root, parent string) (fs.FileInfo, error) {
	rel, err := filepath.Rel(root, parent)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return nil, fmt.Errorf("parent is outside the workspace")
	}
	cur := root
	if rel != "." {
		for _, component := range strings.Split(rel, string(filepath.Separator)) {
			cur = filepath.Join(cur, component)
			info, statErr := os.Lstat(cur)
			if os.IsNotExist(statErr) {
				if err := os.Mkdir(cur, 0o755); err != nil && !os.IsExist(err) {
					return nil, err
				}
				info, statErr = os.Lstat(cur)
			}
			if statErr != nil {
				return nil, statErr
			}
			if info.Mode()&fs.ModeSymlink != 0 || !info.IsDir() {
				return nil, fmt.Errorf("%s is not a real directory", cur)
			}
		}
	}
	resolved, err := filepath.EvalSymlinks(parent)
	if err != nil {
		return nil, err
	}
	if filepath.Clean(resolved) != filepath.Clean(parent) {
		return nil, fmt.Errorf("parent directory changed through a symlink")
	}
	info, err := os.Lstat(parent)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() || info.Mode()&fs.ModeSymlink != 0 {
		return nil, fmt.Errorf("parent is not a real directory")
	}
	return info, nil
}

func validateResolvedTarget(root, path string) error {
	rel, err := filepath.Rel(root, path)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return fmt.Errorf("path is outside the workspace")
	}
	resolved, err := resolveExistingPrefix(filepath.Clean(path))
	if err != nil {
		return err
	}
	if filepath.Clean(resolved) != filepath.Clean(path) {
		return fmt.Errorf("path changed through a symlink after it was resolved")
	}
	return nil
}
