package router

import (
	"strings"
	"testing"
)

// Staying is the default, because every switch abandons a warm prefix on the
// target it leaves and pays to write a new one where it lands.
func TestStickyStaysWithoutEvidence(t *testing.T) {
	s := NewSticky(Policy{}, 1)

	for range 10 {
		if move := s.AfterCall(3); move.Direction != 0 {
			t.Fatalf("moved with no signals: %+v", move)
		}
	}
	if s.Rank() != 1 {
		t.Errorf("rank = %d, want the one it started on", s.Rank())
	}
}

func TestStickyMovesOnEvidenceOnceTheDwellElapses(t *testing.T) {
	s := NewSticky(Policy{MinimumDwell: 2}, 1)
	s.Observe(RepeatedToolCall)

	if move := s.AfterCall(3); move.Direction != 0 || !move.Held {
		t.Fatalf("moved inside the dwell: %+v", move)
	}
	move := s.AfterCall(3)
	if move.Direction != 1 {
		t.Fatalf("did not escalate once the dwell elapsed: %+v", move)
	}
	if s.Rank() != 2 {
		t.Errorf("rank = %d, want 2", s.Rank())
	}
	// The signals that caused the move are spent, so the next call does not
	// escalate again on the same evidence.
	if again := s.AfterCall(3); again.Direction != 0 {
		t.Errorf("escalated twice on one piece of evidence: %+v", again)
	}
}

// The ladder has ends, and running off one is worth saying rather than silently
// staying put.
func TestStickyReportsHittingTheEndsOfTheLadder(t *testing.T) {
	top := NewSticky(Policy{MinimumDwell: 1}, 2)
	top.Observe(RepeatedToolCall)
	move := top.AfterCall(2)
	if move.Direction != 0 {
		t.Fatalf("escalated past the top: %+v", move)
	}
	if !strings.Contains(move.Rationale, "top of the ladder") {
		t.Errorf("rationale = %q", move.Rationale)
	}

	bottom := NewSticky(Policy{MinimumDwell: 1, DeescalateAfter: 1}, 0)
	bottom.Observe(PlanningComplete)
	move = bottom.AfterCall(2)
	if move.Direction != 0 {
		t.Fatalf("stepped below the bottom: %+v", move)
	}
	if !strings.Contains(move.Rationale, "bottom of the ladder") {
		t.Errorf("rationale = %q", move.Rationale)
	}
}

// A user who names a target has already answered the question the escalation
// policy asks.
func TestAPinnedPrimaryIsNotMovedByEvidence(t *testing.T) {
	s := NewSticky(Policy{MinimumDwell: 1}, 0)
	s.Pin(2)

	s.Observe(RepeatedToolCall)
	s.Observe(EditReverted)
	move := s.AfterCall(3)

	if move.Direction != 0 {
		t.Errorf("a pinned primary was moved: %+v", move)
	}
	if s.Rank() != 2 {
		t.Errorf("rank = %d, want the pinned one", s.Rank())
	}
	if !strings.Contains(move.Rationale, "pinned") {
		t.Errorf("rationale = %q; the user should see why evidence was ignored", move.Rationale)
	}
}

// §8.3's hysteresis is about not undoing a move on the next turn, and it ages
// out so one hard turn does not hold the ladder up for the rest of a session.
func TestEscalationMemoryLastsOneTurn(t *testing.T) {
	s := NewSticky(Policy{MinimumDwell: 1}, 0)

	s.Observe(RepeatedToolCall)
	s.AfterCall(3)
	if !s.EscalatedLastTurn() {
		t.Fatal("an escalation was not remembered for the next turn")
	}

	s.StartTurn()
	if s.EscalatedLastTurn() {
		t.Error("the escalation memory outlived the turn after it")
	}
}
