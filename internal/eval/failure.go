package eval

import "strings"

// FailureKind separates model-quality evidence from failures of the harness,
// provider, or serving surface. The empty value means the attempt solved.
type FailureKind string

const (
	FailureWorkspace    FailureKind = "workspace"
	FailureSetup        FailureKind = "setup"
	FailureRouting      FailureKind = "routing"
	FailureTimeout      FailureKind = "timeout"
	FailureCancelled    FailureKind = "cancelled"
	FailureRoundLimit   FailureKind = "round_limit"
	FailureTurn         FailureKind = "turn_error"
	FailureVerification FailureKind = "verification"
	FailureVerifier     FailureKind = "verifier_error"
	FailureFidelity     FailureKind = "routing_fidelity"
	FailureUnknown      FailureKind = "unknown"
)

// FailureKind returns the recorded category or derives it from an older
// journal's Detail field. Derivation is deliberately narrow: an unknown error
// stays unknown rather than being guessed into a model-quality label.
func (r Run) FailureKind() FailureKind {
	if r.Failure != "" {
		return r.Failure
	}
	if r.Solved {
		return ""
	}
	switch {
	case strings.HasPrefix(r.Detail, "could not create a workspace:"):
		return FailureWorkspace
	case strings.HasPrefix(r.Detail, "setup failed:"):
		return FailureSetup
	case strings.HasPrefix(r.Detail, "routing failed:"):
		return FailureRouting
	case strings.HasPrefix(r.Detail, "routed evaluation fidelity failed:"):
		return FailureFidelity
	case strings.HasPrefix(r.Detail, "the verifier failed to run:"):
		return FailureVerifier
	case strings.HasPrefix(r.Detail, "the turn failed:"):
		turn := strings.TrimPrefix(r.Detail, "the turn failed:")
		switch {
		case strings.Contains(turn, "context deadline exceeded"):
			return FailureTimeout
		case strings.Contains(turn, "context canceled"):
			return FailureCancelled
		case strings.Contains(turn, "tool-round limit"):
			return FailureRoundLimit
		default:
			return FailureTurn
		}
	default:
		return FailureUnknown
	}
}

// modelQualityOutcome is the only population from which quality rates may be
// computed: a cleanly solved attempt, or a cleanly completed attempt whose
// independent verifier explicitly said the solution was wrong. Everything
// else measures infrastructure, serving, or harness reliability instead.
func modelQualityOutcome(run Run) bool {
	kind := run.FailureKind()
	return (run.Solved && kind == "") || (!run.Solved && kind == FailureVerification)
}
