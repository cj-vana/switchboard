package router

import (
	"strings"
	"testing"
)

// §8.3 says uncertainty language is a weak signal and never sufficient alone.
// That is arithmetic here rather than a special case, so a later edit cannot
// quietly lose it.
func TestUncertaintyAloneNeverEscalates(t *testing.T) {
	p := Policy{}

	alone := p.Assess([]Signal{UncertaintyLanguage, UncertaintyLanguage, UncertaintyLanguage}, 10)
	if alone.Direction != 0 {
		t.Errorf("hedging alone escalated: %s", alone.Rationale)
	}

	// With something real alongside it, it contributes.
	together := p.Assess([]Signal{UncertaintyLanguage, RepeatedToolCall}, 10)
	if together.Direction != 1 {
		t.Errorf("a real signal with hedging did not escalate: %+v", together)
	}
}

// Moving back down abandons a warm prefix and risks redoing work, so it takes
// more evidence than moving up. That asymmetry is the hysteresis.
func TestDeescalationNeedsMoreEvidenceThanEscalation(t *testing.T) {
	p := Policy{}

	up := p.Assess([]Signal{EditReverted}, 10)
	if up.Direction != 1 {
		t.Errorf("one strong escalating signal did not move up: %+v", up)
	}

	down := p.Assess([]Signal{PlanningComplete}, 10)
	if down.Direction != 0 {
		t.Errorf("one de-escalating signal moved down; that is the oscillation §8.3 warns about: %+v", down)
	}

	moreDown := p.Assess([]Signal{PlanningComplete, ScopeReduced}, 10)
	if moreDown.Direction != -1 {
		t.Errorf("two de-escalating signals did not move down: %+v", moreDown)
	}
}

// The dwell stops a switch every other call inside one long turn. Each switch
// abandons a warm prefix on the target it leaves, so oscillation costs money
// twice over.
func TestMinimumDwellHoldsAWarrantedSwitch(t *testing.T) {
	p := Policy{MinimumDwell: 3}

	early := p.Assess([]Signal{EditReverted, RepeatedToolCall}, 1)
	if early.Direction != 0 {
		t.Error("a switch happened inside the dwell")
	}
	if !early.Held {
		t.Error("the held switch was not reported, so it would look like nothing was warranted")
	}
	if !strings.Contains(early.Rationale, "1 of 3") {
		t.Errorf("rationale = %q; it has to say why it was held", early.Rationale)
	}

	later := p.Assess([]Signal{EditReverted, RepeatedToolCall}, 3)
	if later.Direction != 1 {
		t.Errorf("the switch did not happen once the dwell elapsed: %+v", later)
	}
}

func TestNoSignalsMeansStay(t *testing.T) {
	got := Policy{}.Assess(nil, 10)
	if got.Direction != 0 || got.Held {
		t.Errorf("move = %+v, want no change", got)
	}
}

// §8.4's labelling rules, each of which prevents a specific way a router learns
// the wrong thing.
func TestOutcomeLabellingFollowsTheEvidence(t *testing.T) {
	// A clean completion is weak evidence of sufficiency and none of necessity.
	// Treating it as necessity is how a naive router learns to over-provision.
	unverified := LabelFor(Completed, false)
	verified := LabelFor(Completed, true)
	if !unverified.Positive || !verified.Positive {
		t.Error("a completion is not positive evidence")
	}
	if unverified.Weight >= verified.Weight {
		t.Errorf("an unverified completion (%.2f) weighs as much as a verified one (%.2f)",
			unverified.Weight, verified.Weight)
	}
	if !strings.Contains(unverified.Reason, "necessity") {
		t.Errorf("the reason does not distinguish sufficiency from necessity: %q", unverified.Reason)
	}

	// An escalation is not automatically negative: provider failure, a phase
	// change, and a bad rule produce the same event.
	esc := LabelFor(Escalated, false)
	if esc.Negative {
		t.Error("an escalation was treated as a negative label")
	}
	if !esc.Censored {
		t.Error("an escalation was counted as usable signal")
	}

	// Abandonment is censored, not negative.
	ab := LabelFor(Abandoned, false)
	if ab.Negative || !ab.Censored {
		t.Errorf("abandonment = %+v, want censored", ab)
	}
	failed := LabelFor(Failed, false)
	if failed.Positive || failed.Negative || !failed.Censored || failed.Weight != 0 {
		t.Errorf("failed route = %+v, want unavailable/censored evidence", failed)
	}

	// A reviewer rejection against a rubric is the strongest negative.
	rej := LabelFor(ReviewerRejected, false)
	corrected := LabelFor(UserCorrected, false)
	if !rej.Negative || !corrected.Negative {
		t.Error("a rejection or a correction is not negative evidence")
	}
	if rej.Weight <= corrected.Weight {
		t.Errorf("a rubric rejection (%.2f) does not outweigh a user correction (%.2f)",
			rej.Weight, corrected.Weight)
	}
}

// Censored outcomes must be excluded rather than counted as neutral, or the
// majority of a real session's turns quietly become training evidence.
func TestCensoredOutcomesCarryNoWeight(t *testing.T) {
	for _, o := range []Outcome{Escalated, Abandoned, Failed} {
		if got := LabelFor(o, false); got.Weight != 0 {
			t.Errorf("%s carries weight %.2f while censored", o, got.Weight)
		}
	}
}
