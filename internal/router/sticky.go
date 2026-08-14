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
}

func NewSticky(policy Policy, startRank int) *Sticky {
	return &Sticky{Policy: policy, rank: startRank}
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
	s.rank = rank
	s.pinned = true
	s.signals = nil
}

func (s *Sticky) Unpin() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pinned = false
}

// Observe records a signal seen inside the current turn.
func (s *Sticky) Observe(sig Signal) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.pinned {
		return
	}
	s.signals = append(s.signals, sig)
}

// AfterCall is consulted once per model call. It returns the move that was made,
// which is zero when nothing changed.
func (s *Sticky) AfterCall(maxRank int) Move {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.callsSinceSwitch++
	if s.pinned {
		return Move{Rationale: "pinned by you, so mid-turn evidence does not move it"}
	}

	move := s.Policy.Assess(s.signals, s.callsSinceSwitch)
	if move.Direction == 0 {
		return move
	}

	next := s.rank + move.Direction
	switch {
	case next < 0:
		return Move{Rationale: fmt.Sprintf("would step down (%s) but this is the bottom of the ladder", move.Rationale)}
	case next > maxRank:
		return Move{Rationale: fmt.Sprintf("would escalate (%s) but this is the top of the ladder", move.Rationale)}
	}

	s.rank = next
	s.callsSinceSwitch = 0
	s.signals = nil
	if move.Direction > 0 {
		s.escalatedLastTurn = true
	}
	return move
}

// EndTurn clears the per-turn signals. The escalation memory survives, because
// §8.3's hysteresis is about not undoing a move on the next turn.
func (s *Sticky) EndTurn() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.signals = nil
}

// StartTurn is called before a new user message. It ages out the escalation
// memory, so a single hard turn does not hold the ladder up forever.
func (s *Sticky) StartTurn() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.escalatedLastTurn = false
	s.signals = nil
}
