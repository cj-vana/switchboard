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
	"bytes"
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
	parents []ancestorIdentity

	// committed distinguishes a successful mutation from a capture that is
	// only prepared. activeKind distinguishes legacy one-call Record captures
	// from two-phase RecordState captures that Begin and undo must wait for.
	// This also lets a later mutation update the expected post-image without
	// replacing the turn's first pre-image or its original parent identity.
	committed  bool
	parentSet  bool
	activeKind captureKind
	active     int
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
	size    int64
	digest  [sha256.Size]byte
}

type ancestorIdentity struct {
	path string
	info fs.FileInfo
}

type skippedState struct {
	committed  bool
	activeKind captureKind
	active     int
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
// committed guard that makes a later current-state read stale-safe.
type MutationSnapshot struct {
	Path   string
	Before FileState
	After  FileFingerprint

	// turn and state bind the otherwise cloned value to live recorder evidence.
	// They are deliberately private: callers can ask Recorder to read the
	// matching current post-image, but cannot mint authority for an arbitrary
	// path.
	turn  *Turn
	state *fileState
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

// ReviewCursor is opaque authority for one exact recorder turn at one exact
// checkpoint revision. It lets an asynchronous read-only surface bind its
// selection before leaving the UI goroutine without cloning any pre-images.
type ReviewCursor struct {
	turn     *Turn
	revision uint64
	index    int
	open     bool
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

// UndoFileOutcome separates the point-of-no-return from the checks that run
// after it. Published means the target was removed or replaced, so callers
// must invalidate any read authority for the path even when UndoFile also
// returns a durability or final-state verification error. Removed identifies
// which inverse operation was published.
type UndoFileOutcome struct {
	Published bool
	Removed   bool
}

// Recorder is safe for concurrent use: parallel-safe tools do not mutate,
// but the loop and a surface may inspect while a turn runs.
type Recorder struct {
	mu                 sync.Mutex
	restoreMu          sync.Mutex
	idle               *sync.Cond
	activeTransactions int
	activeRestores     int
	transitionWaiters  int // deterministic concurrency tests; guarded by mu
	turns              []*Turn
	cur                *Turn
	revision           uint64

	// restoreHook is deterministic fault injection for tests that prove a
	// mutation cannot enter RecordState while a restore is in flight.
	restoreHook func()

	// snapshotAfterOpenHook is deterministic fault injection for tests that
	// prove snapshot I/O does not hold the recorder lifecycle lock.
	snapshotAfterOpenHook func()

	// These hooks inject failures immediately after the irreversible filesystem
	// operation. They pin the distinction between publication and the later
	// durability/verification checks without relying on filesystem quirks.
	afterRemoveHook  func() error
	afterReplaceHook func() error
}

func NewRecorder() *Recorder {
	r := &Recorder{}
	r.idle = sync.NewCond(&r.mu)
	return r
}

// ErrStale means an undo target no longer matches the successful mutation's
// post-image. Refusing is intentional: restoring over an editor, formatter,
// shell command, or later overlapping agent edit would turn undo into data
// loss. The capture remains available when the refusal happens before
// publication. If a final check finds staleness after remove/replace succeeded,
// UndoFileOutcome.Published reports that point-of-no-return and the capture is
// consumed.
var ErrStale = errors.New("checkpoint post-image no longer matches")

// ErrSnapshotTooLarge means a current regular file has the committed
// existence, mode, and size, but its digest cannot be reverified within the
// review I/O bound. A review surface must render an explicit unverified marker
// instead of a text diff.
var ErrSnapshotTooLarge = errors.New("checkpoint post-image is over the review byte limit")

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
	r.waitForRestoresLocked()
	r.commitLocked()
	r.cur = newTurn(label)
	r.revision++
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

func (r *Recorder) waitForRestoresLocked() {
	for r.activeRestores > 0 {
		r.transitionWaiters++
		r.conditionLocked().Wait()
		r.transitionWaiters--
	}
}

func (r *Recorder) startRestoreLocked() func() {
	r.activeRestores++
	return r.restoreHook
}

func (r *Recorder) finishRestore() {
	r.mu.Lock()
	if r.activeRestores > 0 {
		r.activeRestores--
	}
	if r.activeRestores == 0 {
		r.conditionLocked().Broadcast()
	}
	r.mu.Unlock()
}

func (r *Recorder) startTransactionLocked(kind *captureKind, active *int) {
	*kind = captureTwoPhase
	(*active)++
	r.activeTransactions++
}

func (r *Recorder) finishTransactionLocked(kind *captureKind, active *int) {
	if *kind != captureTwoPhase || *active <= 0 {
		*kind = captureIdle
		return
	}
	*active--
	if *active == 0 {
		*kind = captureIdle
	}
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
			st.parent, st.parents, st.parentSet = parentIdentity(path)
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
	r.waitForRestoresLocked()
	if r.cur == nil {
		return
	}
	r.revision++
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
			parent, parents, parentSet := parentIdentity(abs)
			r.cur.files[abs] = &fileState{
				existed: false, parent: parent, parents: parents, parentSet: parentSet,
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
	parent, parents, parentSet := parentIdentity(abs)
	r.cur.files[abs] = &fileState{
		existed:    true,
		mode:       restorableMode(info.Mode()),
		content:    content,
		parent:     parent,
		parents:    parents,
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
	r.waitForRestoresLocked()
	if r.cur == nil {
		return
	}
	r.revision++
	if st, seen := r.cur.files[abs]; seen {
		r.startTransactionLocked(&st.activeKind, &st.active)
		return
	}
	if st, seen := r.cur.skipped[abs]; seen {
		r.startTransactionLocked(&st.activeKind, &st.active)
		return
	}
	if existed && len(content) > maxFileBytes {
		st := &skippedState{}
		r.startTransactionLocked(&st.activeKind, &st.active)
		r.cur.skipped[abs] = st
		return
	}
	parent, parents, parentSet := parentIdentity(abs)
	st := &fileState{
		existed:   existed,
		mode:      restorableMode(mode),
		content:   append([]byte(nil), content...),
		parent:    parent,
		parents:   parents,
		parentSet: parentSet,
	}
	r.startTransactionLocked(&st.activeKind, &st.active)
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
	r.revision++
	after := fingerprint{existed: existed, mode: restorableMode(mode), size: committedSize(abs, existed), digest: digest}
	if st, ok := r.cur.files[abs]; ok {
		if !st.parentSet {
			st.parent, st.parents, st.parentSet = parentIdentity(abs)
		}
		st.after = after
		st.committed = true
		r.finishTransactionLocked(&st.activeKind, &st.active)
		return
	}
	if st, ok := r.cur.skipped[abs]; ok {
		st.committed = true
		r.finishTransactionLocked(&st.activeKind, &st.active)
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
	r.revision++
	if st, ok := r.cur.files[abs]; ok {
		if st.activeKind == captureTwoPhase {
			r.finishTransactionLocked(&st.activeKind, &st.active)
		}
		if !st.committed && st.active == 0 {
			delete(r.cur.files, abs)
		}
		return
	}
	if st, ok := r.cur.skipped[abs]; ok {
		if st.activeKind == captureTwoPhase {
			r.finishTransactionLocked(&st.activeKind, &st.active)
		}
		if !st.committed && st.active == 0 {
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

	out := make([]TurnSnapshot, 0, len(r.turns)+1)
	for _, turn := range r.turns {
		out = append(out, snapshotTurnLocked(turn, false))
	}
	if r.cur != nil {
		snapshot := snapshotTurnLocked(r.cur, true)
		if len(snapshot.Files) > 0 || len(snapshot.Skipped) > 0 {
			out = append(out, snapshot)
		}
	}
	return out
}

// CurrentSnapshot reports the open turn even when it has no committed
// mutations. This lets a read-only current-turn surface distinguish a no-op
// turn from the previous closed mutating turn without changing Snapshots'
// historical filtering semantics.
func (r *Recorder) CurrentSnapshot() (TurnSnapshot, int, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.cur == nil {
		return TurnSnapshot{}, 0, false
	}
	return snapshotTurnLocked(r.cur, true), len(r.turns) + 1, true
}

// CurrentReviewCursor binds the currently open turn without cloning its file
// bytes. hasMutations reports committed write/edit evidence; ok is false when
// no turn scope is open. The cursor becomes stale on any checkpoint mutation.
func (r *Recorder) CurrentReviewCursor() (cursor ReviewCursor, index int, hasMutations, ok bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.cur == nil {
		return ReviewCursor{}, 0, false, false
	}
	index = len(r.turns) + 1
	return ReviewCursor{turn: r.cur, revision: r.revision, index: index, open: true}, index,
		hasReviewEvidenceLocked(r.cur), true
}

// ReviewCursorAt binds one one-based recorded mutation turn without cloning
// the other retained turns. total is the number of mutation turns addressable
// at that instant, including a mutating open turn.
func (r *Recorder) ReviewCursorAt(turn int) (cursor ReviewCursor, total int, ok bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	total = len(r.turns)
	currentIncluded := r.cur != nil && hasReviewEvidenceLocked(r.cur)
	if currentIncluded {
		total++
	}
	if turn < 1 || turn > total {
		return ReviewCursor{}, total, false
	}
	if turn <= len(r.turns) {
		return ReviewCursor{turn: r.turns[turn-1], revision: r.revision, index: turn}, total, true
	}
	return ReviewCursor{turn: r.cur, revision: r.revision, index: turn, open: true}, total, true
}

// ReviewSnapshot clones only a bounded prefix of the exact turn selected by
// cursor. Paths are considered in bytewise order; omitted reports additional
// committed paths excluded by the file or aggregate pre-image byte limit.
func (r *Recorder) ReviewSnapshot(cursor ReviewCursor, maxFiles, maxBytes int) (TurnSnapshot, int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if maxFiles < 1 || maxBytes < 0 || !r.reviewCursorCurrentLocked(cursor) {
		return TurnSnapshot{}, 0, fmt.Errorf("%w: review turn changed before it was loaded", ErrStale)
	}
	return snapshotTurnBoundedLocked(cursor.turn, cursor.open, maxFiles, maxBytes)
}

// ReviewCursorValid reports whether cursor still names the same idle recorder
// revision. It is the final guard after a bounded loader performs file I/O.
func (r *Recorder) ReviewCursorValid(cursor ReviewCursor) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.reviewCursorCurrentLocked(cursor)
}

func (r *Recorder) reviewCursorCurrentLocked(cursor ReviewCursor) bool {
	if cursor.turn == nil || cursor.revision != r.revision || r.activeTransactions != 0 || r.activeRestores != 0 {
		return false
	}
	if cursor.open {
		return r.cur == cursor.turn && cursor.index == len(r.turns)+1
	}
	return cursor.index >= 1 && cursor.index <= len(r.turns) && r.turns[cursor.index-1] == cursor.turn
}

func hasReviewEvidenceLocked(turn *Turn) bool {
	if turn == nil {
		return false
	}
	for _, state := range turn.files {
		if state.committed {
			return true
		}
	}
	for _, state := range turn.skipped {
		if state.committed {
			return true
		}
	}
	return false
}

type reviewSnapshotCandidate struct {
	path    string
	skipped bool
}

func snapshotTurnBoundedLocked(turn *Turn, open bool, maxFiles, maxBytes int) (TurnSnapshot, int, error) {
	candidates := make([]reviewSnapshotCandidate, 0, maxFiles)
	total := 0
	consider := func(candidate reviewSnapshotCandidate) {
		total++
		at := sort.Search(len(candidates), func(i int) bool {
			if candidates[i].path == candidate.path {
				return !candidates[i].skipped || candidate.skipped
			}
			return candidates[i].path >= candidate.path
		})
		if len(candidates) == maxFiles && at == len(candidates) {
			return
		}
		candidates = append(candidates, reviewSnapshotCandidate{})
		copy(candidates[at+1:], candidates[at:])
		candidates[at] = candidate
		if len(candidates) > maxFiles {
			candidates = candidates[:maxFiles]
		}
	}
	for path, state := range turn.files {
		if state.committed {
			consider(reviewSnapshotCandidate{path: path})
		}
	}
	for path, state := range turn.skipped {
		if state.committed {
			consider(reviewSnapshotCandidate{path: path, skipped: true})
		}
	}

	out := TurnSnapshot{Label: turn.label, Open: open}
	omitted := total - len(candidates)
	remaining := maxBytes
	for _, candidate := range candidates {
		if candidate.skipped {
			out.Skipped = append(out.Skipped, candidate.path)
			continue
		}
		state := turn.files[candidate.path]
		if len(state.content) > remaining {
			omitted++
			continue
		}
		remaining -= len(state.content)
		out.Files = append(out.Files, MutationSnapshot{
			Path: candidate.path,
			Before: FileState{
				Existed: state.existed,
				Mode:    state.mode,
				Content: append([]byte(nil), state.content...),
			},
			After: FileFingerprint{
				Existed: state.after.existed,
				Mode:    state.after.mode,
				Digest:  state.after.digest,
			},
			turn:  turn,
			state: state,
		})
	}
	out.Partial = len(out.Skipped) > 0 || omitted > 0
	return out, omitted, nil
}

func snapshotTurnLocked(t *Turn, open bool) TurnSnapshot {
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
			turn:  t,
			state: st,
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

// ReadSnapshotCurrent returns the exact current post-image for snapshot after
// proving that the snapshot still names live recorder evidence, the captured
// parent directory has not changed identity, and existence, mode, and complete
// content digest still match the committed After fingerprint. It never follows
// a target symlink. Callers must not fall back to reading snapshot.Path when
// this method refuses: doing so would turn stale or redirected bytes into a
// review of the recorded mutation.
//
// File I/O runs without the recorder mutex and is capped at maxFileBytes+1;
// the live token is revalidated after the read. If the expected file exceeds
// that bound, the method returns its stable existence and mode with
// ErrSnapshotTooLarge, omits Content, and does not claim its digest was checked.
func (r *Recorder) ReadSnapshotCurrent(snapshot MutationSnapshot) (FileState, error) {
	return r.readSnapshotCurrentBounded(snapshot, maxFileBytes)
}

// ReadSnapshotCurrentBounded is ReadSnapshotCurrent with a caller-supplied
// content ceiling. A matching file above the ceiling returns
// ErrSnapshotTooLarge without reading or returning its bytes.
func (r *Recorder) ReadSnapshotCurrentBounded(snapshot MutationSnapshot, maxBytes int) (FileState, error) {
	if maxBytes < 0 {
		maxBytes = 0
	}
	if maxBytes > maxFileBytes {
		maxBytes = maxFileBytes
	}
	return r.readSnapshotCurrentBounded(snapshot, maxBytes)
}

func (r *Recorder) readSnapshotCurrentBounded(snapshot MutationSnapshot, maxBytes int) (FileState, error) {
	r.mu.Lock()
	r.waitForRestoresLocked()

	st, ok := r.snapshotStateLocked(snapshot)
	if !ok {
		r.mu.Unlock()
		return FileState{}, fmt.Errorf("%w: review snapshot is no longer current", ErrStale)
	}
	if st.activeKind != captureIdle || st.active != 0 {
		r.mu.Unlock()
		return FileState{}, fmt.Errorf("%w: %s has another mutation in progress", ErrStale, snapshot.Path)
	}
	expected := st.after
	readState := &fileState{
		parent:    st.parent,
		parents:   append([]ancestorIdentity(nil), st.parents...),
		parentSet: st.parentSet,
	}
	afterOpen := r.snapshotAfterOpenHook
	r.mu.Unlock()

	current, readErr := readSnapshotCurrent(snapshot.Path, expected, readState, afterOpen, int64(maxBytes))

	r.mu.Lock()
	live, stillCurrent := r.snapshotStateLocked(snapshot)
	valid := stillCurrent && live == st && live.activeKind == captureIdle && live.active == 0 &&
		r.activeRestores == 0 && live.after == expected
	r.mu.Unlock()
	if !valid {
		return FileState{}, fmt.Errorf("%w: review snapshot changed while its post-image was read", ErrStale)
	}
	return current, readErr
}

func (r *Recorder) snapshotStateLocked(snapshot MutationSnapshot) (*fileState, bool) {
	if snapshot.turn == nil || snapshot.state == nil || snapshot.Path == "" {
		return nil, false
	}
	knownTurn := snapshot.turn == r.cur
	if !knownTurn {
		for _, turn := range r.turns {
			if turn == snapshot.turn {
				knownTurn = true
				break
			}
		}
	}
	if !knownTurn {
		return nil, false
	}
	st, ok := snapshot.turn.files[snapshot.Path]
	if !ok || st != snapshot.state || !st.committed {
		return nil, false
	}
	if snapshot.Before.Existed != st.existed ||
		snapshot.Before.Mode != st.mode ||
		!bytes.Equal(snapshot.Before.Content, st.content) ||
		snapshot.After.Existed != st.after.existed ||
		snapshot.After.Mode != st.after.mode ||
		snapshot.After.Digest != st.after.digest {
		return nil, false
	}
	return st, true
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
// Outcome.Published can be true alongside a non-nil error: remove/replace
// succeeded, but a later durability or final-state check did not. Such a
// capture is consumed because retrying it would compare against stale evidence.
func (r *Recorder) UndoFile(abs string) (outcome UndoFileOutcome, label string, err error) {
	r.restoreMu.Lock()
	defer r.restoreMu.Unlock()
	r.mu.Lock()
	r.waitForTransactionsLocked()
	r.revision++
	resumeLabel, hadOpenScope := "", r.cur != nil
	if hadOpenScope {
		resumeLabel = r.cur.label
	}
	r.commitLocked()
	var turn *Turn
	for i := len(r.turns) - 1; i >= 0; i-- {
		if _, ok := r.turns[i].files[abs]; ok {
			turn = r.turns[i]
			break
		}
	}
	if turn == nil {
		if hadOpenScope {
			r.cur = newTurn(resumeLabel)
		}
		r.mu.Unlock()
		return UndoFileOutcome{}, "", fmt.Errorf("no turn captured %s, as far as write and edit saw", abs)
	}
	st := turn.files[abs]
	if !hadOpenScope {
		resumeLabel = turn.label
	}
	// Keep an empty capture scope available before waking any RecordState
	// waiter. A mutation that was blocked behind this restore must never wake
	// to nil and then publish without checkpoint evidence.
	r.cur = newTurn(resumeLabel)
	hook := r.startRestoreLocked()
	hooks := restoreHooks{
		afterRemove:  r.afterRemoveHook,
		afterReplace: r.afterReplaceHook,
	}
	label = turn.label
	r.mu.Unlock()
	defer r.finishRestore()
	if hook != nil {
		hook()
	}

	// A pre-publication failure leaves the one copy of the old content available
	// for inspection or retry. Once remove/replace succeeds, consume the capture
	// even if a later durability or verification check reports an error.
	restored := restore(abs, st, hooks)
	if !restored.published {
		return UndoFileOutcome{}, label, restored.err
	}
	outcome = UndoFileOutcome{Published: true, Removed: !st.existed}

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
	return outcome, label, restored.err
}

// Undo restores the most recent turn that changed files and reports the
// restored and removed paths, sorted, plus anything the cap kept it from
// covering. Restore-or-report is per file: one unwritable path does not
// abandon the rest, it gets named. A path whose remove/replace was published
// before a later durability or verification error appears in both its changed
// list and failed; callers must invalidate every path in the changed lists.
func (r *Recorder) Undo() (restored, removed, skipped, failed []string, label string, err error) {
	r.restoreMu.Lock()
	defer r.restoreMu.Unlock()
	r.mu.Lock()
	r.waitForTransactionsLocked()
	r.revision++
	resumeLabel, hadOpenScope := "", r.cur != nil
	if hadOpenScope {
		resumeLabel = r.cur.label
	}
	r.commitLocked()
	if len(r.turns) == 0 {
		if hadOpenScope {
			r.cur = newTurn(resumeLabel)
		}
		r.mu.Unlock()
		return nil, nil, nil, nil, "", fmt.Errorf("nothing to undo: no turn has changed files")
	}
	turn := r.turns[len(r.turns)-1]
	if !hadOpenScope {
		resumeLabel = turn.label
	}
	r.cur = newTurn(resumeLabel)
	label = turn.label
	for p := range turn.skipped {
		skipped = append(skipped, p)
	}
	// Oversize captures were never restorable. Report and consume those
	// markers now; failed file restores stay on this turn and make a later
	// /undo a retry rather than silently advancing to an older turn.
	turn.skipped = map[string]*skippedState{}
	hook := r.startRestoreLocked()
	hooks := restoreHooks{
		afterRemove:  r.afterRemoveHook,
		afterReplace: r.afterReplaceHook,
	}
	r.mu.Unlock()
	defer r.finishRestore()
	if hook != nil {
		hook()
	}

	paths := make([]string, 0, len(turn.files))
	for p := range turn.files {
		paths = append(paths, p)
	}
	sort.Strings(paths)

	for _, p := range paths {
		st := turn.files[p]
		outcome := restore(p, st, hooks)
		if outcome.err != nil {
			failed = append(failed, p+": "+outcome.err.Error())
		}
		if !outcome.published {
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
	fp.size = int64(len(content))
	fp.digest = sha256.Sum256(content)
	return fp
}

func committedSize(path string, existed bool) int64 {
	if !existed {
		return 0
	}
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() {
		return -1
	}
	return info.Size()
}

func readSnapshotCurrent(path string, expected fingerprint, st *fileState, afterOpen func(), maxBytes int64) (FileState, error) {
	if err := validateParentIdentity(path, st); err != nil {
		return FileState{}, err
	}

	linfo, err := os.Lstat(path)
	if err != nil {
		if os.IsNotExist(err) && !expected.existed {
			if err := validateParentIdentity(path, st); err != nil {
				return FileState{}, err
			}
			return FileState{}, nil
		}
		if os.IsNotExist(err) {
			return FileState{}, fmt.Errorf("%w: %s no longer exists", ErrStale, path)
		}
		return FileState{}, err
	}
	if !expected.existed {
		return FileState{}, fmt.Errorf("%w: %s exists after a recorded deletion", ErrStale, path)
	}
	if !linfo.Mode().IsRegular() {
		return FileState{}, fmt.Errorf("%w: %s is not a regular file", ErrStale, path)
	}
	if restorableMode(linfo.Mode()) != expected.mode || (expected.size >= 0 && linfo.Size() != expected.size) {
		return FileState{}, fmt.Errorf("%w: %s size or mode changed after the recorded mutation", ErrStale, path)
	}

	f, err := os.Open(path)
	if err != nil {
		return FileState{}, err
	}
	defer f.Close()
	opened, err := f.Stat()
	if err != nil {
		return FileState{}, err
	}
	if !os.SameFile(linfo, opened) {
		return FileState{}, fmt.Errorf("%w: %s changed identity while it was opened", ErrStale, path)
	}
	if afterOpen != nil {
		afterOpen()
	}
	if restorableMode(opened.Mode()) != expected.mode || (expected.size >= 0 && opened.Size() != expected.size) {
		return FileState{}, fmt.Errorf("%w: %s size or mode changed while it was opened", ErrStale, path)
	}

	if opened.Size() > maxBytes {
		if err := validateSnapshotFileObservation(path, f, opened); err != nil {
			return FileState{}, err
		}
		if err := validateParentIdentity(path, st); err != nil {
			return FileState{}, err
		}
		return FileState{Existed: true, Mode: restorableMode(opened.Mode())},
			fmt.Errorf("%w: %s is larger than %d bytes; digest was not reverified", ErrSnapshotTooLarge, path, maxBytes)
	}

	h := sha256.New()
	var content bytes.Buffer
	if opened.Size() > 0 {
		content.Grow(int(opened.Size()))
	}
	n, err := io.Copy(io.MultiWriter(h, &content), io.LimitReader(f, maxBytes+1))
	if err != nil {
		return FileState{}, err
	}
	if n != opened.Size() || n > maxBytes {
		return FileState{}, fmt.Errorf("%w: %s changed size while it was read", ErrStale, path)
	}
	if err := validateSnapshotFileObservation(path, f, opened); err != nil {
		return FileState{}, err
	}
	actual := fingerprint{existed: true, mode: restorableMode(opened.Mode()), size: n}
	copy(actual.digest[:], h.Sum(nil))
	if !sameFingerprint(actual, expected) {
		return FileState{}, fmt.Errorf("%w: %s changed after the recorded mutation", ErrStale, path)
	}
	if err := validateParentIdentity(path, st); err != nil {
		return FileState{}, err
	}

	current := FileState{Existed: true, Mode: actual.mode}
	current.Content = append([]byte(nil), content.Bytes()...)
	return current, nil
}

func validateSnapshotFileObservation(path string, f *os.File, opened fs.FileInfo) error {
	finished, err := f.Stat()
	if err != nil {
		return err
	}
	if !os.SameFile(opened, finished) || opened.Size() != finished.Size() ||
		!opened.ModTime().Equal(finished.ModTime()) || restorableMode(opened.Mode()) != restorableMode(finished.Mode()) {
		return fmt.Errorf("%w: %s changed while it was read", ErrStale, path)
	}
	linked, err := os.Lstat(path)
	if err != nil || !linked.Mode().IsRegular() || !os.SameFile(finished, linked) ||
		linked.Size() != finished.Size() || !linked.ModTime().Equal(finished.ModTime()) ||
		restorableMode(linked.Mode()) != restorableMode(finished.Mode()) {
		return fmt.Errorf("%w: %s changed identity while it was read", ErrStale, path)
	}
	return nil
}

func fingerprintPath(path string) (fingerprint, error) {
	return fingerprintPathWithHook(path, nil)
}

func fingerprintPathWithHook(path string, afterHash func()) (fingerprint, error) {
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
	if afterHash != nil {
		afterHash()
	}
	linked, err := os.Lstat(path)
	if err != nil || !linked.Mode().IsRegular() || !os.SameFile(finished, linked) ||
		linked.Size() != finished.Size() || !linked.ModTime().Equal(finished.ModTime()) ||
		restorableMode(linked.Mode()) != restorableMode(finished.Mode()) {
		return fingerprint{}, fmt.Errorf("%w: %s changed identity while it was fingerprinted", ErrStale, path)
	}
	fp := fingerprint{existed: true, mode: restorableMode(finished.Mode()), size: finished.Size()}
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
	if a.mode != b.mode || a.digest != b.digest {
		return false
	}
	return a.size < 0 || b.size < 0 || a.size == b.size
}

func parentIdentity(path string) (fs.FileInfo, []ancestorIdentity, bool) {
	if !filepath.IsAbs(path) {
		return nil, nil, false
	}
	parent := filepath.Clean(filepath.Dir(path))
	var reverse []string
	for current := parent; ; current = filepath.Dir(current) {
		reverse = append(reverse, current)
		next := filepath.Dir(current)
		if next == current {
			break
		}
	}
	ancestors := make([]ancestorIdentity, 0, len(reverse))
	for i := len(reverse) - 1; i >= 0; i-- {
		info, err := os.Lstat(reverse[i])
		if err != nil || (!info.IsDir() && info.Mode()&fs.ModeSymlink == 0) {
			return nil, nil, false
		}
		ancestors = append(ancestors, ancestorIdentity{path: reverse[i], info: info})
	}
	if len(ancestors) == 0 {
		return nil, nil, false
	}
	immediate := ancestors[len(ancestors)-1].info
	if !immediate.IsDir() || immediate.Mode()&fs.ModeSymlink != 0 {
		return nil, nil, false
	}
	return immediate, ancestors, true
}

func validateParentIdentity(path string, st *fileState) error {
	if !st.parentSet || st.parent == nil || len(st.parents) == 0 {
		return fmt.Errorf("%w: no trustworthy parent identity was captured for %s", ErrStale, path)
	}
	for i, captured := range st.parents {
		role := "ancestor"
		if i == len(st.parents)-1 {
			role = "parent"
		}
		current, err := os.Lstat(captured.path)
		if err != nil {
			return fmt.Errorf("%w: cannot verify %s %s of %s: %v", ErrStale, role, captured.path, path, err)
		}
		capturedSymlink := captured.info.Mode()&fs.ModeSymlink != 0
		currentSymlink := current.Mode()&fs.ModeSymlink != 0
		if capturedSymlink != currentSymlink || captured.info.IsDir() != current.IsDir() ||
			!os.SameFile(captured.info, current) {
			return fmt.Errorf("%w: %s %s of %s changed identity", ErrStale, role, captured.path, path)
		}
		if i == len(st.parents)-1 && (!current.IsDir() || currentSymlink) {
			return fmt.Errorf("%w: parent of %s is no longer a real directory", ErrStale, path)
		}
	}
	return nil
}

type restoreHooks struct {
	afterRemove  func() error
	afterReplace func() error
}

type restoreOutcome struct {
	published bool
	err       error
}

func unpublishedRestore(err error) restoreOutcome {
	return restoreOutcome{err: err}
}

func publishedRestoreError(path string, err error) restoreOutcome {
	return restoreOutcome{
		published: true,
		err:       fmt.Errorf("restore was published for %s, but durability or final-state verification failed: %w", path, err),
	}
}

func restore(path string, st *fileState, hooks restoreHooks) restoreOutcome {
	// Existing /undo remains a cooperative compare-and-swap: the captured
	// parent and post-image are checked immediately before publication, but the
	// portable filesystem APIs cannot make an external pathname writer part of
	// that transaction. Read-only turn review never calls this function.
	if err := validateParentIdentity(path, st); err != nil {
		return unpublishedRestore(err)
	}
	current, err := fingerprintPath(path)
	if err != nil {
		return unpublishedRestore(err)
	}
	if !sameFingerprint(current, st.after) {
		return unpublishedRestore(fmt.Errorf("%w: %s changed after the recorded mutation; refusing to overwrite it", ErrStale, path))
	}
	if !st.existed {
		if err := validateParentIdentity(path, st); err != nil {
			return unpublishedRestore(err)
		}
		if err := os.Remove(path); err != nil {
			return unpublishedRestore(err)
		}
		if hooks.afterRemove != nil {
			if err := hooks.afterRemove(); err != nil {
				return publishedRestoreError(path, err)
			}
		}
		if err := syncDirectory(filepath.Dir(path)); err != nil {
			return publishedRestoreError(path, fmt.Errorf("syncing parent directory: %w", err))
		}
		check, err := fingerprintPath(path)
		if err != nil {
			return publishedRestoreError(path, fmt.Errorf("verifying removal: %w", err))
		}
		if check.existed {
			return publishedRestoreError(path, errors.New("file still exists after removal"))
		}
		return restoreOutcome{published: true}
	}
	return atomicRestore(path, st.content, st.mode, st.after, st, hooks)
}

func atomicRestore(path string, content []byte, mode fs.FileMode, expected fingerprint, st *fileState, hooks restoreHooks) restoreOutcome {
	parent := filepath.Dir(path)
	if err := validateParentIdentity(path, st); err != nil {
		return unpublishedRestore(err)
	}
	tmp, err := os.CreateTemp(parent, ".switchboard-undo-*")
	if err != nil {
		return unpublishedRestore(err)
	}
	tmpPath := tmp.Name()
	defer func() {
		_ = tmp.Close()
		_ = os.Remove(tmpPath)
	}()
	if _, err := tmp.Write(content); err != nil {
		return unpublishedRestore(err)
	}
	if err := tmp.Chmod(mode); err != nil {
		return unpublishedRestore(err)
	}
	if err := tmp.Sync(); err != nil {
		return unpublishedRestore(err)
	}
	tmpInfo, err := tmp.Stat()
	if err != nil {
		return unpublishedRestore(err)
	}
	if err := tmp.Close(); err != nil {
		return unpublishedRestore(err)
	}

	if err := validateParentIdentity(path, st); err != nil {
		return unpublishedRestore(err)
	}
	currentTmp, err := os.Lstat(tmpPath)
	if err != nil || !currentTmp.Mode().IsRegular() || !os.SameFile(tmpInfo, currentTmp) {
		return unpublishedRestore(fmt.Errorf("%w: undo temporary file changed identity", ErrStale))
	}
	current, err := fingerprintPath(path)
	if err != nil {
		return unpublishedRestore(err)
	}
	if !sameFingerprint(current, expected) {
		return unpublishedRestore(fmt.Errorf("%w: %s changed while undo was being prepared", ErrStale, path))
	}
	if err := validateParentIdentity(path, st); err != nil {
		return unpublishedRestore(err)
	}
	currentTmp, err = os.Lstat(tmpPath)
	if err != nil || !currentTmp.Mode().IsRegular() || !os.SameFile(tmpInfo, currentTmp) {
		return unpublishedRestore(fmt.Errorf("%w: undo temporary file changed before publication", ErrStale))
	}
	if err := replacePath(tmpPath, path); err != nil {
		return unpublishedRestore(err)
	}
	if hooks.afterReplace != nil {
		if err := hooks.afterReplace(); err != nil {
			return publishedRestoreError(path, err)
		}
	}
	if err := syncDirectory(parent); err != nil {
		return publishedRestoreError(path, fmt.Errorf("syncing parent directory: %w", err))
	}
	want := fingerprintBytes(true, mode, content)
	got, err := fingerprintPath(path)
	if err != nil {
		return publishedRestoreError(path, fmt.Errorf("verifying restored file: %w", err))
	}
	if !sameFingerprint(got, want) {
		return publishedRestoreError(path, errors.New("restored file post-image mismatch"))
	}
	if err := validateParentIdentity(path, st); err != nil {
		return publishedRestoreError(path, err)
	}
	return restoreOutcome{published: true}
}
