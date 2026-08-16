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
	"fmt"
	"io/fs"
	"os"
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
}

// Turn is one user turn's capture set.
type Turn struct {
	label   string
	files   map[string]fileState
	skipped []string // paths over the cap, named rather than half-covered
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
	mu    sync.Mutex
	turns []*Turn
	cur   *Turn
}

func NewRecorder() *Recorder { return &Recorder{} }

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
	r.commitLocked()
	r.cur = &Turn{label: label, files: map[string]fileState{}}
}

func (r *Recorder) commitLocked() {
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
	if _, seen := r.cur.files[abs]; seen {
		return
	}
	for _, s := range r.cur.skipped {
		if s == abs {
			return
		}
	}

	info, err := os.Lstat(abs)
	if err != nil {
		if os.IsNotExist(err) {
			r.cur.files[abs] = fileState{existed: false}
		}
		// Any other stat failure: leave uncaptured; the mutation itself will
		// surface the real error.
		return
	}
	if !info.Mode().IsRegular() {
		return
	}
	if info.Size() > maxFileBytes {
		r.cur.skipped = append(r.cur.skipped, abs)
		return
	}
	content, err := os.ReadFile(abs)
	if err != nil {
		return
	}
	r.cur.files[abs] = fileState{existed: true, mode: info.Mode().Perm(), content: content}
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

// Undo restores the most recent turn that changed files and reports the
// restored and removed paths, sorted, plus anything the cap kept it from
// covering. Restore-or-report is per file: one unwritable path does not
// abandon the rest, it gets named.
func (r *Recorder) Undo() (restored, removed, skipped, failed []string, label string, err error) {
	r.mu.Lock()
	r.commitLocked()
	if len(r.turns) == 0 {
		r.mu.Unlock()
		return nil, nil, nil, nil, "", fmt.Errorf("nothing to undo: no turn has changed files")
	}
	turn := r.turns[len(r.turns)-1]
	r.turns = r.turns[:len(r.turns)-1]
	r.mu.Unlock()

	paths := make([]string, 0, len(turn.files))
	for p := range turn.files {
		paths = append(paths, p)
	}
	sort.Strings(paths)

	for _, p := range paths {
		st := turn.files[p]
		if !st.existed {
			if rmErr := os.Remove(p); rmErr != nil && !os.IsNotExist(rmErr) {
				failed = append(failed, p+": "+rmErr.Error())
				continue
			}
			removed = append(removed, p)
			continue
		}
		if wrErr := os.WriteFile(p, st.content, st.mode); wrErr != nil {
			failed = append(failed, p+": "+wrErr.Error())
			continue
		}
		restored = append(restored, p)
	}
	return restored, removed, append([]string(nil), turn.skipped...), failed, turn.label, nil
}
