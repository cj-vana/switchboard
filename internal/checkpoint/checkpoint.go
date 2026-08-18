// Package checkpoint records what files looked like before each turn
// changed them, so a turn's edits can be taken back.
//
// The scope is deliberately files, not conversation. Rewinding messages
// would rewrite the append-only prefix and invalidate the provider cache
// from that point (§6.1); restoring a file invalidates nothing, because the
// model is required to re-read before it may write again — undo leans on
// the same read-before-write contract the tools already enforce.
//
// Capture is before-first-mutation, per turn: the first time a turn touches
// a file, its prior bytes are kept; later touches in the same turn are the
// turn's own churn and restore to the pre-turn state. Only the write and
// edit tools capture. A shell command that mutates the workspace is outside
// the boundary, and saying so plainly beats a checkpoint that sometimes
// covers it.
package checkpoint

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

const (
	// maxTurns bounds memory across a long session; the oldest checkpoint
	// falls off. Fifty user turns of undo depth is a session's working past.
	maxTurns = 50

	// maxFileBytes bounds one captured file. A file over the cap is not
	// captured, and the turn is marked partial so an undo can say what it
	// could not restore instead of silently restoring half a turn.
	maxFileBytes = 4 << 20
)

type fileState struct {
	existed bool
	mode    fs.FileMode
	content []byte
	after   fingerprint
	parent  fs.FileInfo

	// committed distinguishes a successful mutation from a capture that is
	// only prepared. activeKind distinguishes legacy one-call Record captures
	// from two-phase RecordState captures that Begin and undo must wait for.
	// This also lets a later mutation update the expected post-image without
	// replacing the turn's first pre-image or its original parent identity.
	committed  bool
	parentSet  bool
	activeKind captureKind
}

type captureKind uint8

const (
	captureIdle captureKind = iota
	captureLegacy
	captureTwoPhase
)

type fingerprint struct {
	existed bool
	mode    fs.FileMode
	digest  [sha256.Size]byte
}

type skippedState struct {
	committed  bool
	activeKind captureKind
}

// FileState is one captured file's bytes-and-existence, exported for the
// surfaces that reconstruct past states rather than pop them — /bisect
// above all. Existed false means the file was not there: restoring that
// state is deleting the file.
type FileState struct {
	Existed bool
	Mode    fs.FileMode
	Content []byte
}

// FileFingerprint is the exact, bounded identity an undo compare-and-swap
// expects at the target. Digest is SHA-256 of the complete file bytes. A
// non-existent state has a zero Mode and Digest.
type FileFingerprint struct {
	Existed bool
	Mode    fs.FileMode
	Digest  [sha256.Size]byte
}

// MutationSnapshot is one successful path mutation shaped for read-only
// review. Before is cloned and cannot mutate recorder state; After is the
// committed guard that makes a later restore stale-safe.
type MutationSnapshot struct {
	Path   string
	Before FileState
	After  FileFingerprint
}

// TurnSnapshot is the recorder's non-consuming review surface. Files contains
// successful mutations only. Skipped names successful mutations whose
// pre-images exceeded the memory bound and therefore cannot be reviewed or
// restored exactly.
type TurnSnapshot struct {
	Label   string
	Files   []MutationSnapshot
	Skipped []string
	Partial bool
	Open    bool
}

// Turn is one user turn's capture set.
type Turn struct {
	label   string
	files   map[string]*fileState
	skipped map[string]*skippedState // paths over the cap, named rather than half-covered
}

// Info describes a turn for display.
type Info struct {
	Label   string
	Files   int
	Partial bool
}

// Recorder is safe for concurrent use: parallel-safe tools do not mutate,
// but the loop and a surface may inspect while a turn runs.
type Recorder struct {
	mu                 sync.Mutex
	restoreMu          sync.Mutex
	idle               *sync.Cond
	activeTransactions int
	transitionWaiters  int // deterministic concurrency tests; guarded by mu
	turns              []*Turn
	cur                *Turn
}

func NewRecorder() *Recorder {
	r := &Recorder{}
	r.idle = sync.NewCond(&r.mu)
	return r
}

// ErrStale means an undo target no longer matches the successful mutation's
// post-image. Refusing is intentional: restoring over an editor, formatter,
// shell command, or later overlapping agent edit would turn undo into data
// loss. The capture remains available after this error.
var ErrStale = errors.New("checkpoint post-image no longer matches")

// Begin opens a new turn scope. An open scope with no captures is
// discarded rather than stacked, so /undo never pops a turn that changed
// nothing.
func (r *Recorder) Begin(label string) {
	label = strings.TrimSpace(label)
	if len(label) > 60 {
		label = label[:60] + "…"
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.waitForTransactionsLocked()
	r.commitLocked()
	r.cur = newTurn(label)
}

func (r *Recorder) conditionLocked() *sync.Cond {
	// Preserve the useful zero value for embedders that construct Recorder
	// directly instead of calling NewRecorder.
	if r.idle == nil {
		r.idle = sync.NewCond(&r.mu)
	}
	return r.idle
}

func (r *Recorder) waitForTransactionsLocked() {
	for r.activeTransactions > 0 {
		r.transitionWaiters++
		r.conditionLocked().Wait()
		r.transitionWaiters--
	}
}

func (r *Recorder) startTransactionLocked(kind *captureKind) {
	if *kind == captureTwoPhase {
		return
	}
	*kind = captureTwoPhase
	r.activeTransactions++
}

func (r *Recorder) finishTransactionLocked(kind *captureKind) {
	if *kind != captureTwoPhase {
		*kind = captureIdle
		return
	}
	*kind = captureIdle
	if r.activeTransactions > 0 {
		r.activeTransactions--
	}
	if r.activeTransactions == 0 {
		r.conditionLocked().Broadcast()
	}
}

func newTurn(label string) *Turn {
	return &Turn{
		label:   label,
		files:   map[string]*fileState{},
		skipped: map[string]*skippedState{},
	}
}

func (r *Recorder) commitLocked() {
	r.finalizeLegacyLocked()
	if r.cur == nil || (len(r.cur.files) == 0 && len(r.cur.skipped) == 0) {
		r.cur = nil
		return
	}
	r.turns = append(r.turns, r.cur)
	if len(r.turns) > maxTurns {
		r.turns = r.turns[len(r.turns)-maxTurns:]
	}
	r.cur = nil
}

// finalizeLegacyLocked preserves Record's original one-call contract for
// embedders that have not adopted Commit/Abort yet. First-party mutations use
// the two-phase API and therefore record the expected post-image immediately;
// this compatibility path samples it only when the scope is closed or undone.
func (r *Recorder) finalizeLegacyLocked() {
	if r.cur == nil {
		return
	}
	for path, st := range r.cur.files {
		if st.activeKind == captureTwoPhase {
			// Begin and undo wait before reaching this function. Keeping this
			// guard makes the invariant fail closed if another caller is added.
			continue
		}
		if st.committed && st.activeKind == captureIdle {
			continue
		}
		fp, err := fingerprintPath(path)
		if err != nil {
			if !st.committed {
				delete(r.cur.files, path)
			} else {
				st.activeKind = captureIdle
			}
			continue
		}
		if !st.parentSet {
			st.parent, st.parentSet = parentIdentity(path)
		}
		st.after = fp
		st.committed = true
		st.activeKind = captureIdle
	}
	for _, st := range r.cur.skipped {
		if st.activeKind == captureTwoPhase {
			continue
		}
		st.committed = true
		st.activeKind = captureIdle
	}
}

// Record captures a file's current state, once per turn, before a mutation.
// Outside any turn scope it does nothing: a mutation with no turn is a
// caller bug, and inventing an anonymous checkpoint would file the capture
// where no undo can honestly describe it.
func (r *Recorder) Record(abs string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.cur == nil {
		return
	}
	if st, seen := r.cur.files[abs]; seen {
		if st.activeKind != captureTwoPhase {
			st.activeKind = captureLegacy
		}
		return
	}
	if st, seen := r.cur.skipped[abs]; seen {
		if st.activeKind != captureTwoPhase {
			st.activeKind = captureLegacy
		}
		return
	}

	info, err := os.Lstat(abs)
	if err != nil {
		if os.IsNotExist(err) {
			parent, parentSet := parentIdentity(abs)
			r.cur.files[abs] = &fileState{
				existed: false, parent: parent, parentSet: parentSet,
				activeKind: captureLegacy,
			}
		}
		// Any other stat failure: leave uncaptured; the mutation itself will
		// surface the real error.
		return
	}
	if !info.Mode().IsRegular() {
		return
	}
	if info.Size() > maxFileBytes {
		r.cur.skipped[abs] = &skippedState{activeKind: captureLegacy}
		return
	}
	content, err := os.ReadFile(abs)
	if err != nil {
		return
	}
	parent, parentSet := parentIdentity(abs)
	r.cur.files[abs] = &fileState{
		existed:    true,
		mode:       restorableMode(info.Mode()),
		content:    content,
		parent:     parent,
		parentSet:  parentSet,
		activeKind: captureLegacy,
	}
}

// RecordState is Record with the exact bytes already read by the mutation
// transaction. It avoids a second read and makes the capture and the
// transaction's source identity the same observation. Record remains for API
// compatibility; new mutation callers should prefer RecordState.
func (r *Recorder) RecordState(abs string, existed bool, mode fs.FileMode, content []byte) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.cur == nil {
		return
	}
	if st, seen := r.cur.files[abs]; seen {
		r.startTransactionLocked(&st.activeKind)
		return
	}
	if st, seen := r.cur.skipped[abs]; seen {
		r.startTransactionLocked(&st.activeKind)
		return
	}
	if existed && len(content) > maxFileBytes {
		st := &skippedState{}
		r.startTransactionLocked(&st.activeKind)
		r.cur.skipped[abs] = st
		return
	}
	parent, parentSet := parentIdentity(abs)
	st := &fileState{
		existed:   existed,
		mode:      restorableMode(mode),
		content:   append([]byte(nil), content...),
		parent:    parent,
		parentSet: parentSet,
	}
	r.startTransactionLocked(&st.activeKind)
	r.cur.files[abs] = st
}

// Commit records the post-image of a successful mutation. The first
// pre-image remains unchanged across repeated edits in a turn; each commit
// only advances the compare-and-swap guard to the newest successful bytes.
// digest must be SHA-256 of the complete post-image when existed is true.
func (r *Recorder) Commit(abs string, existed bool, mode fs.FileMode, digest [sha256.Size]byte) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.cur == nil {
		return
	}
	after := fingerprint{existed: existed, mode: restorableMode(mode), digest: digest}
	if st, ok := r.cur.files[abs]; ok {
		if !st.parentSet {
			st.parent, st.parentSet = parentIdentity(abs)
		}
		st.after = after
		st.committed = true
		r.finishTransactionLocked(&st.activeKind)
		return
	}
	if st, ok := r.cur.skipped[abs]; ok {
		st.committed = true
		r.finishTransactionLocked(&st.activeKind)
	}
}

// Abort discards a prepared capture when no mutation was published. If the
// file was changed successfully earlier in the same turn, that committed
// capture and its original pre-image remain intact.
func (r *Recorder) Abort(abs string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.cur == nil {
		return
	}
	if st, ok := r.cur.files[abs]; ok {
		wasTwoPhase := st.activeKind == captureTwoPhase
		if st.committed {
			r.finishTransactionLocked(&st.activeKind)
		} else {
			if wasTwoPhase {
				r.finishTransactionLocked(&st.activeKind)
			}
			delete(r.cur.files, abs)
		}
		return
	}
	if st, ok := r.cur.skipped[abs]; ok {
		wasTwoPhase := st.activeKind == captureTwoPhase
		if st.committed {
			r.finishTransactionLocked(&st.activeKind)
		} else {
			if wasTwoPhase {
				r.finishTransactionLocked(&st.activeKind)
			}
			delete(r.cur.skipped, abs)
		}
	}
}

// PendingFiles counts what the open turn scope has captured so far,
// paths over the snapshot cap included: those were mutations too, just ones
// undo cannot cover. It is the loop's own evidence that the current turn has
// changed files — the same evidence /undo restores from — which is what lets
// a surface ask "has anything changed since I last looked" without the loop
// keeping an edit history it does not have.
func (r *Recorder) PendingFiles() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.cur == nil {
		return 0
	}
	return len(r.cur.files) + len(r.cur.skipped)
}

// Turns lists checkpoints oldest first, including the still-open scope if
// it has captures.
func (r *Recorder) Turns() []Info {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]Info, 0, len(r.turns)+1)
	for _, t := range r.turns {
		out = append(out, Info{Label: t.label, Files: len(t.files), Partial: len(t.skipped) > 0})
	}
	if r.cur != nil && (len(r.cur.files) > 0 || len(r.cur.skipped) > 0) {
		out = append(out, Info{Label: r.cur.label, Files: len(r.cur.files), Partial: len(r.cur.skipped) > 0})
	}
	return out
}

// TurnDetail is one turn's capture set for display: which files, not just
// how many. Paths are absolute and sorted; Skipped names what the snapshot
// cap kept uncovered.
type TurnDetail struct {
	Label   string
	Paths   []string
	Skipped []string
}

// Details lists checkpoints oldest first with their captured paths,
// including the still-open scope when it has captures. It is the same
// evidence Undo restores from, shaped for a surface that wants to say what
// a session touched rather than take it back.
func (r *Recorder) Details() []TurnDetail {
	r.mu.Lock()
	defer r.mu.Unlock()
	detail := func(t *Turn) TurnDetail {
		d := TurnDetail{Label: t.label}
		for path := range t.files {
			d.Paths = append(d.Paths, path)
		}
		for path := range t.skipped {
			d.Skipped = append(d.Skipped, path)
		}
		sort.Strings(d.Paths)
		sort.Strings(d.Skipped)
		return d
	}
	out := make([]TurnDetail, 0, len(r.turns)+1)
	for _, t := range r.turns {
		out = append(out, detail(t))
	}
	if r.cur != nil && (len(r.cur.files) > 0 || len(r.cur.skipped) > 0) {
		out = append(out, detail(r.cur))
	}
	return out
}

// Snapshots returns exact cloned evidence without finalizing legacy pending
// captures, consuming an undo entry, or otherwise changing recorder state.
// First-party transactions Commit synchronously, so their successful edits
// appear immediately even while the turn remains open.
func (r *Recorder) Snapshots() []TurnSnapshot {
	r.mu.Lock()
	defer r.mu.Unlock()

	clone := func(t *Turn, open bool) TurnSnapshot {
		out := TurnSnapshot{Label: t.label, Open: open}
		for path, st := range t.files {
			if !st.committed {
				continue
			}
			out.Files = append(out.Files, MutationSnapshot{
				Path: path,
				Before: FileState{
					Existed: st.existed,
					Mode:    st.mode,
					Content: append([]byte(nil), st.content...),
				},
				After: FileFingerprint{
					Existed: st.after.existed,
					Mode:    st.after.mode,
					Digest:  st.after.digest,
				},
			})
		}
		for path, st := range t.skipped {
			if st.committed {
				out.Skipped = append(out.Skipped, path)
			}
		}
		sort.Slice(out.Files, func(i, j int) bool { return out.Files[i].Path < out.Files[j].Path })
		sort.Strings(out.Skipped)
		out.Partial = len(out.Skipped) > 0
		return out
	}

	out := make([]TurnSnapshot, 0, len(r.turns)+1)
	for _, turn := range r.turns {
		out = append(out, clone(turn, false))
	}
	if r.cur != nil {
		snapshot := clone(r.cur, true)
		if len(snapshot.Files) > 0 || len(snapshot.Skipped) > 0 {
			out = append(out, snapshot)
		}
	}
	return out
}

// StateBefore returns, for every file any turn from index turn onward
// captured, its state just before that turn ran: the oldest pre-image at
// or after it. The index is into Turns(), the still-open scope included.
// Files no turn in that range captured are absent from the map — their
// state before the turn is whatever they hold now, and the caller already
// has that. Paths a partial turn skipped are absent too, which is why a
// reconstruction over a partial turn must be refused, not attempted.
func (r *Recorder) StateBefore(turn int) map[string]FileState {
	r.mu.Lock()
	defer r.mu.Unlock()
	scopes := r.turns
	if r.cur != nil && (len(r.cur.files) > 0 || len(r.cur.skipped) > 0) {
		scopes = append(append([]*Turn(nil), r.turns...), r.cur)
	}
	out := map[string]FileState{}
	for i := len(scopes) - 1; i >= turn && i >= 0; i-- {
		for path, st := range scopes[i].files {
			out[path] = FileState{Existed: st.existed, Mode: st.mode, Content: append([]byte(nil), st.content...)}
		}
	}
	return out
}

// UndoFile restores one file to what it was before the newest turn that
// captured it, and consumes that capture, so a later whole-turn /undo does
// not restore it twice. The turn's other files stay on the stack: taking
// back one file is not taking back the turn. A turn left with nothing is
// dropped, the same rule Begin applies to a scope that captured nothing.
// removed reports the inverse restore: the turn created the file, so
// taking it back deletes it.
func (r *Recorder) UndoFile(abs string) (removed bool, label string, err error) {
	r.restoreMu.Lock()
	defer r.restoreMu.Unlock()
	r.mu.Lock()
	r.waitForTransactionsLocked()
	r.commitLocked()
	var turn *Turn
	for i := len(r.turns) - 1; i >= 0; i-- {
		if _, ok := r.turns[i].files[abs]; ok {
			turn = r.turns[i]
			break
		}
	}
	if turn == nil {
		r.mu.Unlock()
		return false, "", fmt.Errorf("no turn captured %s, as far as write and edit saw", abs)
	}
	st := turn.files[abs]
	label = turn.label
	r.mu.Unlock()

	// Restore first, consume after: a failed or stale compare-and-swap must
	// leave the one copy of the old content available for inspection/retry.
	if restoreErr := restore(abs, st); restoreErr != nil {
		return false, label, restoreErr
	}
	removed = !st.existed

	r.mu.Lock()
	delete(turn.files, abs)
	if len(turn.files) == 0 && len(turn.skipped) == 0 {
		for i, t := range r.turns {
			if t == turn {
				r.turns = append(r.turns[:i], r.turns[i+1:]...)
				break
			}
		}
	}
	r.mu.Unlock()
	return removed, label, nil
}

// Undo restores the most recent turn that changed files and reports the
// restored and removed paths, sorted, plus anything the cap kept it from
// covering. Restore-or-report is per file: one unwritable path does not
// abandon the rest, it gets named.
func (r *Recorder) Undo() (restored, removed, skipped, failed []string, label string, err error) {
	r.restoreMu.Lock()
	defer r.restoreMu.Unlock()
	r.mu.Lock()
	r.waitForTransactionsLocked()
	r.commitLocked()
	if len(r.turns) == 0 {
		r.mu.Unlock()
		return nil, nil, nil, nil, "", fmt.Errorf("nothing to undo: no turn has changed files")
	}
	turn := r.turns[len(r.turns)-1]
	label = turn.label
	for p := range turn.skipped {
		skipped = append(skipped, p)
	}
	// Oversize captures were never restorable. Report and consume those
	// markers now; failed file restores stay on this turn and make a later
	// /undo a retry rather than silently advancing to an older turn.
	turn.skipped = map[string]*skippedState{}
	r.mu.Unlock()

	paths := make([]string, 0, len(turn.files))
	for p := range turn.files {
		paths = append(paths, p)
	}
	sort.Strings(paths)

	for _, p := range paths {
		st := turn.files[p]
		if restoreErr := restore(p, st); restoreErr != nil {
			failed = append(failed, p+": "+restoreErr.Error())
			continue
		}
		if st.existed {
			restored = append(restored, p)
		} else {
			removed = append(removed, p)
		}
		r.mu.Lock()
		delete(turn.files, p)
		r.mu.Unlock()
	}

	sort.Strings(skipped)
	r.mu.Lock()
	if len(turn.files) == 0 && len(turn.skipped) == 0 {
		for i, candidate := range r.turns {
			if candidate == turn {
				r.turns = append(r.turns[:i], r.turns[i+1:]...)
				break
			}
		}
	}
	r.mu.Unlock()
	return restored, removed, skipped, failed, label, nil
}

func fingerprintBytes(existed bool, mode fs.FileMode, content []byte) fingerprint {
	fp := fingerprint{existed: existed}
	if !existed {
		return fp
	}
	fp.mode = restorableMode(mode)
	fp.digest = sha256.Sum256(content)
	return fp
}

func fingerprintPath(path string) (fingerprint, error) {
	linfo, err := os.Lstat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return fingerprint{}, nil
		}
		return fingerprint{}, err
	}
	if !linfo.Mode().IsRegular() {
		return fingerprint{}, fmt.Errorf("%s is not a regular file", path)
	}
	f, err := os.Open(path)
	if err != nil {
		return fingerprint{}, err
	}
	defer f.Close()
	opened, err := f.Stat()
	if err != nil {
		return fingerprint{}, err
	}
	if !os.SameFile(linfo, opened) {
		return fingerprint{}, fmt.Errorf("%s changed identity while it was opened", path)
	}
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return fingerprint{}, err
	}
	finished, err := f.Stat()
	if err != nil {
		return fingerprint{}, err
	}
	if !os.SameFile(opened, finished) || opened.Size() != finished.Size() ||
		!opened.ModTime().Equal(finished.ModTime()) || restorableMode(opened.Mode()) != restorableMode(finished.Mode()) {
		return fingerprint{}, fmt.Errorf("%s changed while it was fingerprinted", path)
	}
	fp := fingerprint{existed: true, mode: restorableMode(finished.Mode())}
	copy(fp.digest[:], h.Sum(nil))
	return fp, nil
}

func restorableMode(mode fs.FileMode) fs.FileMode {
	return mode & (fs.ModePerm | fs.ModeSetuid | fs.ModeSetgid | fs.ModeSticky)
}

func sameFingerprint(a, b fingerprint) bool {
	if a.existed != b.existed {
		return false
	}
	if !a.existed {
		return true
	}
	return a.mode == b.mode && a.digest == b.digest
}

func parentIdentity(path string) (fs.FileInfo, bool) {
	info, err := os.Lstat(filepath.Dir(path))
	if err != nil || !info.IsDir() || info.Mode()&fs.ModeSymlink != 0 {
		return nil, false
	}
	return info, true
}

func validateParentIdentity(path string, st *fileState) error {
	if !st.parentSet || st.parent == nil {
		return fmt.Errorf("%w: no trustworthy parent identity was captured for %s", ErrStale, path)
	}
	current, err := os.Lstat(filepath.Dir(path))
	if err != nil {
		return fmt.Errorf("%w: cannot verify parent of %s: %v", ErrStale, path, err)
	}
	if !current.IsDir() || current.Mode()&fs.ModeSymlink != 0 {
		return fmt.Errorf("%w: parent of %s is no longer a real directory", ErrStale, path)
	}
	if !os.SameFile(st.parent, current) {
		return fmt.Errorf("%w: parent directory of %s changed identity", ErrStale, path)
	}
	return nil
}

func restore(path string, st *fileState) error {
	// Validate the captured directory before even fingerprinting the path.
	// Otherwise a replaced parent symlink could make the read—and the later
	// restore—land outside the original workspace.
	if err := validateParentIdentity(path, st); err != nil {
		return err
	}
	current, err := fingerprintPath(path)
	if err != nil {
		return err
	}
	if !sameFingerprint(current, st.after) {
		return fmt.Errorf("%w: %s changed after the recorded mutation; refusing to overwrite it", ErrStale, path)
	}
	if !st.existed {
		if err := validateParentIdentity(path, st); err != nil {
			return err
		}
		if err := os.Remove(path); err != nil {
			return err
		}
		if err := syncDirectory(filepath.Dir(path)); err != nil {
			return err
		}
		check, err := fingerprintPath(path)
		if err != nil {
			return err
		}
		if check.existed {
			return fmt.Errorf("verifying removal of %s: file still exists", path)
		}
		return nil
	}
	return atomicRestore(path, st.content, st.mode, st.after, st)
}

func atomicRestore(path string, content []byte, mode fs.FileMode, expected fingerprint, st *fileState) (retErr error) {
	parent := filepath.Dir(path)
	if err := validateParentIdentity(path, st); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(parent, ".switchboard-undo-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer func() {
		_ = tmp.Close()
		if retErr != nil {
			_ = os.Remove(tmpPath)
		}
	}()
	if _, err := tmp.Write(content); err != nil {
		return err
	}
	if err := tmp.Chmod(mode); err != nil {
		return err
	}
	if err := tmp.Sync(); err != nil {
		return err
	}
	tmpInfo, err := tmp.Stat()
	if err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}

	if err := validateParentIdentity(path, st); err != nil {
		return err
	}
	currentTmp, err := os.Lstat(tmpPath)
	if err != nil || !currentTmp.Mode().IsRegular() || !os.SameFile(tmpInfo, currentTmp) {
		return fmt.Errorf("%w: undo temporary file changed identity", ErrStale)
	}
	current, err := fingerprintPath(path)
	if err != nil {
		return err
	}
	if !sameFingerprint(current, expected) {
		return fmt.Errorf("%w: %s changed while undo was being prepared", ErrStale, path)
	}
	if err := validateParentIdentity(path, st); err != nil {
		return err
	}
	currentTmp, err = os.Lstat(tmpPath)
	if err != nil || !currentTmp.Mode().IsRegular() || !os.SameFile(tmpInfo, currentTmp) {
		return fmt.Errorf("%w: undo temporary file changed before publication", ErrStale)
	}
	if err := replacePath(tmpPath, path); err != nil {
		return err
	}
	if err := syncDirectory(parent); err != nil {
		return err
	}
	want := fingerprintBytes(true, mode, content)
	got, err := fingerprintPath(path)
	if err != nil {
		return err
	}
	if !sameFingerprint(got, want) {
		return fmt.Errorf("verifying restored file %s: post-image mismatch", path)
	}
	if err := validateParentIdentity(path, st); err != nil {
		return err
	}
	return nil
}
