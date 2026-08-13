// Package execution runs commands on behalf of the agent and reports what
// isolation the host can actually provide.
//
// The report is deliberately pessimistic. A permission prompt is not a sandbox,
// so anything this package cannot demonstrate on this machine, it denies
// (design principle 4).
package execution

import (
	"errors"
	"runtime"
)

// Mechanism names an OS isolation primitive.
type Mechanism string

const (
	MechanismNone       Mechanism = "none"
	MechanismSeatbelt   Mechanism = "seatbelt"
	MechanismBubblewrap Mechanism = "bubblewrap"
	MechanismNamespaces Mechanism = "namespaces"
)

// Capability separates two questions that are easy to conflate and expensive to
// confuse.
//
// MechanismPresent asks whether the OS primitive exists on this machine.
// The confinement asks whether a profile has been demonstrated, here, now, to
// confine a command: writes stay in the workspace, credential stores are
// unreadable, and egress is refused.
//
// Only the second may gate automatic execution, and it is not a boolean. It is
// the wrapper itself, so "we verified containment" and "we applied containment"
// cannot come apart.
type Capability struct {
	Platform         string
	Mechanism        Mechanism
	MechanismPresent bool
	Detail           string

	confinement *Confinement
}

// AutomaticExecutionAllowed is the only question the permission engine asks of
// this type.
func (c Capability) AutomaticExecutionAllowed() bool { return c.confinement != nil }

// Confinement returns the wrapper to hand to Run, or nil when this host has
// none. Callers pass it through rather than consulting the boolean separately.
func (c Capability) Confinement() *Confinement { return c.confinement }

// Summary is one line for the status display and the first-run notice. On a
// host without verified containment it has to be plain, because a user who
// discovers the limitation by hitting it will reasonably read it as a bug
// (§19.3).
func (c Capability) Summary() string {
	if c.confinement != nil {
		return string(c.Mechanism) + " sandbox verified on this host"
	}
	return "no verified sandbox: every command needs approval (" + c.Detail + ")"
}

func Detect() Capability {
	c := detectPlatform()
	c.Platform = runtime.GOOS
	return c
}

// TestingVerifiedCapability builds a Capability that reports containment, so
// tests in other packages can exercise the path where execution is allowed.
// Production capability comes only from Detect, which runs the self-test.
//
// Its wrapper refuses rather than passing the command through. Tests of policy
// never execute anything, and if this ever leaked into a real build the result
// is a refusal to run instead of a command running unconfined while the
// interface reports a sandbox.
func TestingVerifiedCapability() Capability {
	return Capability{
		Platform:         runtime.GOOS,
		Mechanism:        MechanismNone,
		MechanismPresent: true,
		Detail:           "test double",
		confinement: &Confinement{
			mechanism: MechanismNone,
			wrap: func(Policy, []string) ([]string, error) {
				return nil, errors.New("test-double capability confines nothing and must not run commands")
			},
		},
	}
}
