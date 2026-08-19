package router

import (
	"fmt"
	"strings"
)

// §8.3 says the initial routing decision is worth less than the mid-task
// adjustments, because one user message produces dozens of model calls. This is
// that policy.
//
// Two mechanisms keep it from thrashing. Hysteresis means the evidence to move
// up is not the evidence to move back down, and a minimum dwell means a target
// is kept for a few calls whatever the signals say. Without them a run of tool
// errors and a run of clean calls alternate the target every other call, and
// every switch abandons a warm prefix on the target it left.

// Signal is one observation from inside a turn.
type Signal string

const (
	// NewTestFailure counts only a failure signature not seen before in this
	// turn. The same failure repeating is one problem observed twice, and
	// counting it twice escalates for persistence rather than for difficulty.
	NewTestFailure Signal = "a new test failure"

	// EditReverted is the same edit rejected or reverted again.
	EditReverted Signal = "an edit was reverted again"

	// DiffGrew is the diff crossing a breadth threshold.
	DiffGrew Signal = "the diff crossed the breadth threshold"

	// RepeatedToolCall is loop detection: an identical call with identical
	// arguments.
	RepeatedToolCall Signal = "the same tool call repeated with the same arguments"

	// ToolErrorSpike is a rise in failed tool calls within one turn.
	ToolErrorSpike Signal = "tool calls started failing"

	// UncertaintyLanguage is explicitly a weak signal. §8.3 says never on its
	// own, because a model hedging is not evidence it is stuck.
	UncertaintyLanguage Signal = "the model sounded unsure"

	// EmptyResultRun is a run of tool calls that succeeded and returned
	// nothing. Every other signal here is a lagging one: it reports something
	// that has already gone wrong, and a turn can spend its whole budget
	// without tripping any of them, because searching the wrong place
	// succeeds every time. This is what that looks like while it is still
	// happening.
	EmptyResultRun Signal = "tool calls are succeeding and returning nothing"

	// PlanningComplete means the remaining work is mechanical.
	PlanningComplete Signal = "planning finished and the rest is mechanical"

	// ScopeReduced means the task turned out smaller than it looked.
	ScopeReduced Signal = "the task is smaller than it first appeared"
)

// Policy decides mid-task moves.
type Policy struct {
	// EscalateAfter is how much weighted evidence justifies moving up. Weights
	// and threshold are both guesses until the eval corpus exists, which is why
	// they are fields.
	EscalateAfter float64

	// DeescalateAfter is deliberately larger. Moving back down abandons a warm
	// prefix and risks redoing work, so it needs more evidence than moving up:
	// that asymmetry is the hysteresis.
	DeescalateAfter float64

	// MinimumDwell is how many model calls a target is kept for regardless of
	// signals.
	MinimumDwell int
}

const (
	DefaultEscalateAfter   = 1.0
	DefaultDeescalateAfter = 2.0
	DefaultMinimumDwell    = 3
)

func (p Policy) escalateAfter() float64 {
	if p.EscalateAfter > 0 {
		return p.EscalateAfter
	}
	return DefaultEscalateAfter
}

func (p Policy) deescalateAfter() float64 {
	if p.DeescalateAfter > 0 {
		return p.DeescalateAfter
	}
	return DefaultDeescalateAfter
}

func (p Policy) dwell() int {
	if p.MinimumDwell > 0 {
		return p.MinimumDwell
	}
	return DefaultMinimumDwell
}

// weights are how much each signal counts.
//
// A signal whose §8.3 description already contains its own repetition -- an edit
// reverted *twice*, an identical call *repeated* -- is worth the whole threshold
// on its own, because by the time it is reported the trigger has fired. One that
// counts occurrences, like consecutive new test failures, is worth half, so two
// are needed.
var weights = map[Signal]float64{
	NewTestFailure:   0.5,
	EditReverted:     1.0,
	DiffGrew:         1.0,
	RepeatedToolCall: 1.0,
	ToolErrorSpike:   1.0,

	PlanningComplete: 1.0,
	ScopeReduced:     1.0,

	// Half, despite naming its own repetition, because an empty result is
	// often the correct answer: a grep that matches nothing has done its job
	// and said so. It is evidence that wants corroboration, not a move on its
	// own, which is the same posture §8.3 takes toward hedging.
	EmptyResultRun: 0.5,
}

// weakWeight and weakCap keep hedging from ever escalating by itself.
//
// §8.3 says model uncertainty is a weak signal and never sufficient alone. A
// per-occurrence weight would satisfy that for two occurrences and fail for
// five, so the total contribution is capped below the threshold instead. That
// makes "never by itself" arithmetic rather than a rule someone has to remember.
const (
	weakWeight = 0.3
	weakCap    = 0.3
)

func escalating(s Signal) bool {
	return s != PlanningComplete && s != ScopeReduced
}

// Move is what the policy concluded.
type Move struct {
	Direction int // +1 up, -1 down, 0 stay
	Rationale string
	Held      bool // a move was warranted and the dwell held it back

	// FromRank and ToRank make a proposed move transactional. The policy can
	// ask the surface to prove and bind ToRank first; Sticky commits only when
	// the proposal is still current. A failed probe or budget check therefore
	// cannot leave policy state ahead of the target actually serving the loop.
	FromRank int
	ToRank   int
	Boundary bool

	// revision binds a proposal to the exact Sticky state that produced it.
	// It is deliberately private: only Sticky may mint or validate proposals.
	// A pin, rebase, or concurrent observation can otherwise leave FromRank
	// unchanged while making the proposal stale.
	revision uint64
}

// Assess weighs the signals seen since the last switch.
//
// callsSinceSwitch is model calls, not turns: the dwell exists to stop a
// switch every other call inside one long turn.
func (p Policy) Assess(signals []Signal, callsSinceSwitch int) Move {
	var up, down, weak float64
	seen := map[Signal]bool{}
	var upReasons, downReasons []string

	for _, s := range signals {
		if s == UncertaintyLanguage {
			weak = min(weak+weakWeight, weakCap)
			if !seen[s] {
				upReasons = append(upReasons, string(s))
			}
			seen[s] = true
			continue
		}
		w, ok := weights[s]
		if !ok {
			continue
		}
		if escalating(s) {
			up += w
			if !seen[s] {
				upReasons = append(upReasons, string(s))
			}
		} else {
			down += w
			if !seen[s] {
				downReasons = append(downReasons, string(s))
			}
		}
		seen[s] = true
	}

	// The cap is added only alongside something real. On its own it stays below
	// the threshold by construction, but adding it unconditionally would make
	// the arithmetic read as though hedging counted toward escalation.
	if up > 0 {
		up += weak
	}

	switch {
	case up >= p.escalateAfter():
		if callsSinceSwitch < p.dwell() {
			return Move{
				Held: true,
				Rationale: fmt.Sprintf("would escalate (%s) but the target has served %d of %d calls",
					strings.Join(upReasons, ", "), callsSinceSwitch, p.dwell()),
			}
		}
		return Move{Direction: 1, Rationale: strings.Join(upReasons, ", ")}

	case down >= p.deescalateAfter():
		if callsSinceSwitch < p.dwell() {
			return Move{
				Held: true,
				Rationale: fmt.Sprintf("would step down (%s) but the target has served %d of %d calls",
					strings.Join(downReasons, ", "), callsSinceSwitch, p.dwell()),
			}
		}
		return Move{Direction: -1, Rationale: strings.Join(downReasons, ", ")}
	}

	return Move{Rationale: "nothing in this turn argues for a different target"}
}

// Outcome is how a turn ended, for the §8.4 training signal.
type Outcome string

const (
	Completed        Outcome = "completed"
	Escalated        Outcome = "escalated"
	UserCorrected    Outcome = "user_corrected"
	ReviewerRejected Outcome = "reviewer_rejected"
	Abandoned        Outcome = "abandoned"
	Failed           Outcome = "failed"
)

// Label is what an outcome is worth as evidence, and mostly the answer is "less
// than it looks".
//
// §8.4 is precise about this and each rule prevents a specific way a router
// learns the wrong thing:
//
//   - A clean completion is a weak positive for sufficiency and says nothing
//     about necessity. Treating it as necessity is named as the main way a
//     naive router learns to over-provision, because every successful expensive
//     turn becomes evidence that expense was required.
//   - An escalation is not automatically negative. Provider failure, a planned
//     phase change, or a bad escalation rule produce the same event.
//   - Abandonment is censored rather than negative. A user who walked away told
//     you nothing about the target.
type Label struct {
	Positive bool
	Negative bool

	// Censored means this outcome carries no usable signal and must be excluded
	// from training rather than counted as neutral.
	Censored bool

	// Weight is how much the label is worth relative to a verified outcome,
	// which is 1.
	Weight float64

	Reason string
}

// LabelFor reports what an outcome is worth. verified is whether a task-specific
// check confirmed the result, which §8.4 calls stronger evidence than the
// harness's own completion signal.
func LabelFor(o Outcome, verified bool) Label {
	switch o {
	case Completed:
		if verified {
			return Label{Positive: true, Weight: 1,
				Reason: "a verified completion is evidence the target sufficed"}
		}
		return Label{Positive: true, Weight: 0.3,
			Reason: "an unverified completion is weak evidence of sufficiency and none of necessity"}

	case Escalated:
		return Label{Censored: true, Weight: 0,
			Reason: "an escalation can come from provider failure, a phase change, or a bad rule, " +
				"so it is not evidence the first choice was wrong"}

	case UserCorrected:
		return Label{Negative: true, Weight: 0.7,
			Reason: "the user corrected the result, which is evidence the target fell short"}

	case ReviewerRejected:
		return Label{Negative: true, Weight: 1,
			Reason: "a reviewer rejected the result against a rubric"}

	case Abandoned:
		return Label{Censored: true, Weight: 0,
			Reason: "abandonment says nothing about the target unless the user gave a reason"}

	case Failed:
		return Label{Censored: true, Weight: 0,
			Reason: "provider, budget, round-limit, or internal failure is unavailable model-quality evidence"}
	}
	return Label{Censored: true, Weight: 0, Reason: "unrecognized outcome"}
}
