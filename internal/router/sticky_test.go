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

func TestProposalDoesNotMoveUntilCommitted(t *testing.T) {
	s := NewSticky(Policy{MinimumDwell: 1}, 0)
	s.Observe(RepeatedToolCall)
	s.CallServed()
	move := s.Assess(2)
	if move.Direction != 1 || move.ToRank != 1 {
		t.Fatalf("proposal = %+v", move)
	}
	if s.Rank() != 0 {
		t.Fatal("assessment changed the rank before the destination was bound")
	}
	if !s.Commit(move) || s.Rank() != 1 {
		t.Fatal("a successfully bound proposal did not commit")
	}
	if s.Commit(move) || s.Rank() != 1 {
		t.Fatal("a stale proposal committed twice")
	}
}

func TestSameRankRebasePreservesDwell(t *testing.T) {
	s := NewSticky(Policy{MinimumDwell: 2}, 0)
	s.CallServed()

	// Per-turn routing commonly selects the rung already serving the session.
	// That is not a switch and must not erase the call already served there.
	s.Rebase(0)
	s.Observe(RepeatedToolCall)
	s.CallServed()
	move := s.Assess(2)
	if move.Direction != 1 || move.Held {
		t.Fatalf("same-rank rebase reset dwell: %+v", move)
	}
}

func TestApplyNeverBindsAStaleProposal(t *testing.T) {
	s := NewSticky(Policy{MinimumDwell: 1}, 0)
	s.Observe(RepeatedToolCall)
	s.CallServed()
	move := s.Assess(2)

	// The user pin leaves the rank numerically unchanged, but invalidates the
	// automatic proposal. FromRank alone cannot detect this stale state.
	s.Pin(0)
	bound := false
	if s.Apply(move, func() { bound = true }) {
		t.Fatal("stale proposal committed")
	}
	if bound {
		t.Fatal("stale proposal mutated the live binding before it was rejected")
	}
	if s.Rank() != 0 {
		t.Fatalf("rank = %d, want unchanged", s.Rank())
	}
}

func TestApplyBindsAndCommitsInOneCriticalSection(t *testing.T) {
	s := NewSticky(Policy{MinimumDwell: 1}, 0)
	s.Observe(RepeatedToolCall)
	s.CallServed()
	move := s.Assess(2)
	bound := false
	if !s.Apply(move, func() { bound = true }) || !bound || s.Rank() != 1 {
		t.Fatalf("binding and policy did not land together: bound=%v rank=%d", bound, s.Rank())
	}
}

func TestApplyCheckedFailureLeavesBindingAndRankUncommitted(t *testing.T) {
	s := NewSticky(Policy{MinimumDwell: 1}, 0)
	s.Observe(RepeatedToolCall)
	s.CallServed()
	move := s.Assess(2)
	if s.ApplyChecked(move, func() bool { return false }) {
		t.Fatal("fallible precommit was reported as committed")
	}
	if s.Rank() != 0 {
		t.Fatalf("failed precommit advanced rank to %d", s.Rank())
	}
}

func TestApplyCheckedDoesNotRunPrecommitForStaleProposal(t *testing.T) {
	s := NewSticky(Policy{MinimumDwell: 1}, 0)
	s.Observe(RepeatedToolCall)
	s.CallServed()
	move := s.Assess(2)
	s.Pin(0)
	called := false
	if s.ApplyChecked(move, func() bool { called = true; return true }) {
		t.Fatal("stale proposal committed")
	}
	if called {
		t.Fatal("stale proposal ran its durable precommit")
	}
}

func TestSnapshotRestorePreservesPolicyStateAndInvalidatesMoves(t *testing.T) {
	s := NewSticky(Policy{MinimumDwell: 1}, 0)
	s.Observe(RepeatedToolCall)
	s.CallServed()
	move := s.Assess(2)
	before := s.Snapshot()
	revision := s.revision

	s.Pin(2)
	s.StartTurn()
	s.CallServed()
	s.Restore(before)

	after := s.Snapshot()
	if after.rank != before.rank || after.callsSinceSwitch != before.callsSinceSwitch ||
		after.escalatedLastTurn != before.escalatedLastTurn || after.pinned != before.pinned ||
		len(after.signals) != len(before.signals) {
		t.Fatalf("restored state = %#v, want %#v", after, before)
	}
	if s.revision <= revision {
		t.Fatalf("restore rolled revision back: got %d, before %d", s.revision, revision)
	}
	if s.Commit(move) {
		t.Fatal("move assessed before temporary override committed after restore")
	}
}

func TestBoundaryEvidenceIsReportedAndConsumed(t *testing.T) {
	s := NewSticky(Policy{MinimumDwell: 1}, 1)
	s.Observe(RepeatedToolCall)
	s.CallServed()
	move := s.Assess(1)
	if !move.Boundary || !strings.Contains(move.Rationale, "top of the ladder") {
		t.Fatalf("move = %+v", move)
	}
	if again := s.Assess(1); again.Boundary {
		t.Fatalf("boundary evidence was not consumed: %+v", again)
	}
}
