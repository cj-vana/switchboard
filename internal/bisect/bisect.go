// Package bisect finds the recorded turn that turned a verifier red, by
// binary-searching the per-turn states the checkpoint recorder holds.
//
// git bisect walks commits; this walks turns, in place, with the same
// contract: whatever it does to the working tree, it puts back. The
// states probed are reconstructions from the recorder's pre-images, so
// the boundary is the recorder's own — what write and edit captured;
// a shell command's side effects and hand edits ride along at their
// current state through every probe, and the report says so rather than
// pretending the reconstruction is a checkout.
//
// The verifier is the caller's, declared the way /watch declares one:
// the user's own command, never inferred from the workspace.
package bisect

import (
	"context"
	"fmt"
	"io/fs"
	"os"

	"github.com/cj-vana/switchboard/internal/checkpoint"
)

// Verdict is one verifier run's answer.
type Verdict struct {
	Passed    bool
	FirstFail string // the first failing line, for the report
	Err       error  // the command could not run at all
}

// Outcome says how a run ended.
type Outcome int

const (
	// Found: Culprit is the index of the turn whose completion turned the
	// verifier red — green just before it, red ever since.
	Found Outcome = iota
	// AlreadyGreen: the verifier passes as things stand; nothing to find.
	AlreadyGreen
	// RedBeforeRecord: the oldest reconstructable state already fails, so
	// the break predates what the recorder holds.
	RedBeforeRecord
)

// Result is a completed bisect. The working tree is back to its current
// state whenever Run returns, whatever the outcome.
type Result struct {
	Outcome Outcome
	Culprit int
	Fail    Verdict // the failing verdict at the earliest red state
	Probes  int
}

// Runner bisects the workspace in place.
type Runner struct {
	// States is the reconstruction before each recorded turn, oldest
	// first: States[i] holds every file captured from turn i onward, at
	// its bytes just before turn i ran.
	States []map[string]checkpoint.FileState

	// Verify runs the declared verifier against the tree as it stands.
	Verify func(context.Context) Verdict

	// OnProbe, when set, hears each probe before its verifier runs:
	// which turn boundary is being reconstructed (len(States) means the
	// current state) and how many probes have run.
	OnProbe func(turn, probes int)
}

// Run bisects. Every exit path — a verdict, a verifier error, an
// unwritable file, cancellation — restores the current state first;
// restore failures are the one error that outranks the answer, because a
// bisect that leaves the tree in the past has done damage no verdict
// repays.
func (r *Runner) Run(ctx context.Context) (result Result, err error) {
	if len(r.States) == 0 {
		return Result{}, fmt.Errorf("no recorded turns to bisect")
	}

	// States[0] covers every file any recorded turn captured; that union
	// is the whole set of paths any probe will touch.
	current := map[string]checkpoint.FileState{}
	for path := range r.States[0] {
		st, captureErr := captureFile(path)
		if captureErr != nil {
			return Result{}, fmt.Errorf("cannot capture %s before probing: %w", path, captureErr)
		}
		current[path] = st
	}
	defer func() {
		if restoreErr := restoreAll(current); restoreErr != nil && err == nil {
			err = restoreErr
		}
	}()

	probes := 0
	verifyAt := func(state int) (Verdict, error) {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return Verdict{}, ctxErr
		}
		if r.OnProbe != nil {
			r.OnProbe(state, probes)
		}
		desired := current
		if state < len(r.States) {
			desired = overlay(current, r.States[state])
		}
		if applyErr := restoreAll(desired); applyErr != nil {
			return Verdict{}, applyErr
		}
		probes++
		v := r.Verify(ctx)
		if v.Err != nil {
			return Verdict{}, fmt.Errorf("the verifier could not run: %w", v.Err)
		}
		return v, nil
	}

	now, err := verifyAt(len(r.States))
	if err != nil {
		return Result{}, err
	}
	if now.Passed {
		return Result{Outcome: AlreadyGreen, Probes: probes}, nil
	}
	oldest, err := verifyAt(0)
	if err != nil {
		return Result{}, err
	}
	if !oldest.Passed {
		return Result{Outcome: RedBeforeRecord, Fail: oldest, Probes: probes}, nil
	}

	// Invariant: the state before turn lo is green, the state before turn
	// hi (or the current state, at len(States)) is red. The turn between
	// the final pair is the culprit.
	lo, hi, fail := 0, len(r.States), now
	for hi-lo > 1 {
		mid := (lo + hi) / 2
		v, verifyErr := verifyAt(mid)
		if verifyErr != nil {
			return Result{}, verifyErr
		}
		if v.Passed {
			lo = mid
		} else {
			hi, fail = mid, v
		}
	}
	return Result{Outcome: Found, Culprit: lo, Fail: fail, Probes: probes}, nil
}

// overlay is base with states laid over it: the probe's desired tree.
func overlay(base, states map[string]checkpoint.FileState) map[string]checkpoint.FileState {
	out := make(map[string]checkpoint.FileState, len(base))
	for path, st := range base {
		if past, ok := states[path]; ok {
			st = past
		}
		out[path] = st
	}
	return out
}

func captureFile(path string) (checkpoint.FileState, error) {
	info, err := os.Lstat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return checkpoint.FileState{}, nil
		}
		return checkpoint.FileState{}, err
	}
	if !info.Mode().IsRegular() {
		return checkpoint.FileState{}, fmt.Errorf("%s is no longer a regular file", path)
	}
	content, err := os.ReadFile(path)
	if err != nil {
		return checkpoint.FileState{}, err
	}
	return checkpoint.FileState{Existed: true, Mode: info.Mode().Perm(), Content: content}, nil
}

// restoreAll writes a state map to disk, every file, and reports the
// failures together rather than stopping at the first: a partial restore
// must name everything it left wrong.
func restoreAll(states map[string]checkpoint.FileState) error {
	var failed []string
	for path, st := range states {
		if !st.Existed {
			if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
				failed = append(failed, path+": "+err.Error())
			}
			continue
		}
		mode := st.Mode
		if mode == 0 {
			mode = fs.FileMode(0o644)
		}
		if err := os.WriteFile(path, st.Content, mode); err != nil {
			failed = append(failed, path+": "+err.Error())
		}
	}
	if len(failed) > 0 {
		return fmt.Errorf("restore left %d files wrong: %v", len(failed), failed)
	}
	return nil
}
