package router

import (
	"fmt"
	"sync"
)

// Sticky holds one primary target for a session and moves it only on evidence.
//
// §9.1 calls this the sticky primary, and the reason is cache economics rather
// than taste. Every switch leaves a warm prefix behind on the target it left and
// pays to write a new one where it lands, so a router that re-decides freely
// spends real money to answer a question it already answered. The default is
// therefore to stay, and moving is what needs justification.
//
// It is safe for concurrent use, for the same reason the detector is: a turn's
// tool calls run in parallel goroutines and every signal arrives from one of
// them.
type Sticky struct {
	Policy Policy

	mu sync.Mutex

	rank              int
	callsSinceSwitch  int
	signals           []Signal
	escalatedLastTurn bool
	pinned            bool
	revision          uint64
}

// StickySnapshot is an opaque checkpoint for a temporary surface override.
// Its fields intentionally remain private: callers may restore policy state,
// but cannot manufacture it.
type StickySnapshot struct {
	rank              int
	callsSinceSwitch  int
	signals           []Signal
	escalatedLastTurn bool
	pinned            bool
}

func NewSticky(policy Policy, startRank int) *Sticky {
	return &Sticky{Policy: policy, rank: startRank}
}

// Snapshot captures the complete behavioral state needed to make a temporary
// one-turn pin invisible to the surrounding automatic-routing policy.
func (s *Sticky) Snapshot() StickySnapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	return StickySnapshot{
		rank: s.rank, callsSinceSwitch: s.callsSinceSwitch,
		signals:           append([]Signal(nil), s.signals...),
		escalatedLastTurn: s.escalatedLastTurn, pinned: s.pinned,
	}
}

// Restore reinstates a Snapshot while advancing revision, so a Move assessed
// before the temporary override cannot commit against the restored state.
func (s *Sticky) Restore(snapshot StickySnapshot) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rank = snapshot.rank
	s.callsSinceSwitch = snapshot.callsSinceSwitch
	s.signals = append([]Signal(nil), snapshot.signals...)
	s.escalatedLastTurn = snapshot.escalatedLastTurn
	s.pinned = snapshot.pinned
	s.revision++
}

// Rank is the ladder position currently being served.
func (s *Sticky) Rank() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.rank
}

// EscalatedLastTurn feeds the router's hysteresis, so a turn that needed more
// does not drop straight back down on the next one.
func (s *Sticky) EscalatedLastTurn() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.escalatedLastTurn
}

// Pin fixes the primary to what the user asked for. §8.1 lets a pin
// short-circuit selection after the hard checks, and that includes not being
// moved by mid-turn evidence: a user who names a target has answered the
// question the escalation policy is asking.
func (s *Sticky) Pin(rank int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.rank != rank {
		s.callsSinceSwitch = 0
	}
	s.rank = rank
	s.pinned = true
	s.signals = nil
	s.revision++
}

func (s *Sticky) Unpin() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.pinned {
		return
	}
	s.pinned = false
	s.revision++
}

func (s *Sticky) Pinned() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.pinned
}

// Rebase synchronizes policy state with a target selected at a user-turn
// boundary or by an explicit surface action. It does not pin the rank: future
// evidence may still move an automatically routed session.
func (s *Sticky) Rebase(rank int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.rank == rank {
		s.revision++
		return
	}
	s.rank = rank
	s.callsSinceSwitch = 0
	s.signals = nil
	s.revision++
}

// Observe records a signal seen inside the current turn.
func (s *Sticky) Observe(sig Signal) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.pinned {
		return
	}
	s.signals = append(s.signals, sig)
	s.revision++
}

// CallServed advances the dwell clock exactly once per completed model call.
// Tool results are deliberately not calls: a single model response may request
// many tools in parallel, and counting each result would make dwell depend on
// batch width instead of model work.
func (s *Sticky) CallServed() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.callsSinceSwitch++
	s.revision++
}

// Assess proposes a move without mutating the current rank. Runtime callers
// first prove that the destination can serve the next request, then Apply its
// prepared binding. This two-phase shape keeps the sticky rank and live target
// identical when a budget, capability check, or provider probe rejects it.
func (s *Sticky) Assess(maxRank int) Move {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.pinned {
		return Move{Rationale: "pinned by you, so mid-turn evidence does not move it"}
	}

	move := s.Policy.Assess(s.signals, s.callsSinceSwitch)
	move.revision = s.revision
	if move.Direction == 0 {
		return move
	}

	next := s.rank + move.Direction
	switch {
	case next < 0:
		s.signals = nil
		s.revision++
		return Move{Boundary: true, FromRank: s.rank, ToRank: s.rank, revision: s.revision,
			Rationale: fmt.Sprintf("would step down (%s) but this is the bottom of the ladder", move.Rationale)}
	case next > maxRank:
		s.signals = nil
		s.revision++
		return Move{Boundary: true, FromRank: s.rank, ToRank: s.rank, revision: s.revision,
			Rationale: fmt.Sprintf("would escalate (%s) but this is the top of the ladder", move.Rationale)}
	}
	move.FromRank = s.rank
	move.ToRank = next
	return move
}

// Commit applies a proposal that has no external binding step. Runtime callers
// use Apply so validation, live binding, and policy advancement stay atomic.
func (s *Sticky) Commit(move Move) bool {
	return s.Apply(move, nil)
}

// Apply validates a proposal, applies the already-checked live binding, and
// advances policy state as one critical section. The callback must do no slow
// discovery and must not call back into Sticky. Keeping the final bind here is
// important: binding first and then discovering that Commit rejected a stale
// proposal leaves the live model and the policy on different rungs.
//
// Callers must perform capability, budget, and provider probes before Apply.
// bind is the infallible, in-memory installation of that prepared result; it
// is invoked only after the proposal has been validated while holding the
// Sticky lock, so a stale proposal can never touch the live target.
func (s *Sticky) Apply(move Move, bind func()) bool {
	return s.ApplyChecked(move, func() bool {
		if bind != nil {
			bind()
		}
		return true
	})
}

// ApplyChecked is Apply with a fallible precommit. The callback runs only
// after the proposal is proven current, while the policy lock is held; false
// leaves both rank and policy state unchanged. Runtime callers use this to
// sync their binding WAL before installing an infallible in-memory binding.
func (s *Sticky) ApplyChecked(move Move, bind func() bool) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.pinned || move.Direction == 0 || move.revision != s.revision ||
		move.FromRank != s.rank || move.ToRank != s.rank+move.Direction {
		return false
	}
	if bind != nil && !bind() {
		return false
	}
	s.rank = move.ToRank
	s.callsSinceSwitch = 0
	s.signals = nil
	s.revision++
	if move.Direction > 0 {
		s.escalatedLastTurn = true
	}
	return true
}

// AfterCall preserves the original single-step API for non-runtime callers.
// Production routing uses CallServed, Assess, and Commit so destination checks
// can happen transactionally.
func (s *Sticky) AfterCall(maxRank int) Move {
	s.CallServed()
	move := s.Assess(maxRank)
	if move.Direction != 0 {
		s.Commit(move)
	}
	return move
}

// EndTurn clears the per-turn signals. The escalation memory survives, because
// §8.3's hysteresis is about not undoing a move on the next turn.
func (s *Sticky) EndTurn() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.signals = nil
	s.revision++
}

// StartTurn is called before a new user message. It ages out the escalation
// memory, so a single hard turn does not hold the ladder up forever.
func (s *Sticky) StartTurn() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.escalatedLastTurn = false
	s.signals = nil
	s.revision++
}
