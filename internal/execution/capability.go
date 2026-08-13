// Package execution runs commands on behalf of the agent and reports what
// isolation the host can actually provide.
//
// The report is deliberately pessimistic. A permission prompt is not a sandbox,
// so anything this package cannot demonstrate, it denies (design principle 4).
package execution

import "runtime"

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
// PolicyVerified asks whether Switchboard has a profile, tested against real
// toolchains, that confines a build to the workspace while denying credential
// stores, agent sockets, and network egress.
//
// Only the second may gate automatic execution. A present-but-unverified
// mechanism is worth exactly as much as no mechanism: shipping automatic
// execution on isolation nobody has confirmed would present a prompt as
// containment, which §11 forbids.
type Capability struct {
	Platform         string
	Mechanism        Mechanism
	MechanismPresent bool
	PolicyVerified   bool
	Detail           string
}

// AutomaticExecutionAllowed is the only question the permission engine asks of
// this type.
func (c Capability) AutomaticExecutionAllowed() bool { return c.PolicyVerified }

// Summary is one line for the status display and the first-run notice. On a
// platform without verified containment it has to be plain, because a user who
// discovers the limitation by hitting it will reasonably read it as a bug
// (§19.3).
func (c Capability) Summary() string {
	if c.PolicyVerified {
		return string(c.Mechanism) + " sandbox active"
	}
	return "no verified sandbox: every command needs approval (" + c.Detail + ")"
}

func Detect() Capability {
	c := detectPlatform()
	c.Platform = runtime.GOOS
	// No profile has passed the §11 spike on any platform yet, so nothing here
	// may claim verification. This is the single line that changes when the
	// macOS and Linux spikes land.
	c.PolicyVerified = false
	return c
}
