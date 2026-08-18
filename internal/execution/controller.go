package execution

import (
	"errors"
	"fmt"
	"strings"
	"sync"
)

// SandboxMode is the user's command-confinement preference. It is independent
// of the permission mode: approval answers whether a command may run, while
// this value answers where it runs after approval.
type SandboxMode string

const (
	// SandboxOff is deliberately the zero value and the product default.
	// Commands approved by policy run directly on the host, with the account's
	// normal filesystem and network reach.
	SandboxOff SandboxMode = "off"

	// SandboxOn requires a confinement profile that Detect verified on this
	// host. Selecting it fails rather than pretending a prompt is isolation.
	SandboxOn SandboxMode = "on"

	// SandboxAuto uses verified confinement when it is available and otherwise
	// stays visibly unconfined. It never trusts mechanism presence alone.
	SandboxAuto SandboxMode = "auto"
)

func ParseSandboxMode(value string) (SandboxMode, error) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "", "off", "false", "0", "no":
		return SandboxOff, nil
	case "on", "true", "1", "yes":
		return SandboxOn, nil
	case "auto":
		return SandboxAuto, nil
	default:
		return "", fmt.Errorf("unknown sandbox mode %q: want off, on, or auto", value)
	}
}

// CommandPolicy is an atomic snapshot of the reach a command will receive.
// A caller takes it immediately before execution, so a /mode or /sandbox
// change cannot leave the UI describing one posture while a stale closure
// applies another.
type CommandPolicy struct {
	Revision           uint64
	SandboxMode        SandboxMode
	SandboxActive      bool
	FullAccess         bool
	HostLoopbackShared bool
	HostIPCShared      bool
	Confinement        *Confinement
	Network            NetworkAccess
}

// Controller is shared by every registry built for a session, including
// delegates. Keeping one mutable source of truth prevents a mode change from
// widening the primary while leaving a branch under a different reach policy.
type Controller struct {
	mu         sync.RWMutex
	capability Capability
	sandbox    SandboxMode
	fullAccess bool
	revision   uint64
}

// NewController validates an explicit sandbox requirement before returning.
// SandboxAuto is intentionally non-failing: its point is to use confinement
// where this host can prove it and remain visibly off everywhere else.
func NewController(capability Capability, sandbox SandboxMode) (*Controller, error) {
	c := &Controller{capability: capability}
	if err := c.SetSandbox(sandbox); err != nil {
		return nil, err
	}
	return c, nil
}

// NewDefaultController creates the product-default host-direct posture.
func NewDefaultController(capability Capability) *Controller {
	c, _ := NewController(capability, SandboxOff)
	return c
}

func (c *Controller) Capability() Capability {
	if c == nil {
		return Capability{}
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.capability
}

func (c *Controller) SandboxMode() SandboxMode {
	if c == nil {
		return SandboxOff
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.sandbox
}

// SetSandbox leaves the prior selection intact when SandboxOn cannot be
// honored. A failed interactive change therefore cannot silently disable a
// sandbox that was already active.
func (c *Controller) SetSandbox(mode SandboxMode) error {
	if c == nil {
		return fmt.Errorf("execution controller is nil")
	}
	switch mode {
	case SandboxOff, SandboxAuto:
	case SandboxOn:
		if c.capability.Confinement() == nil {
			return fmt.Errorf("sandbox requested but unavailable: %s", c.capability.Summary())
		}
	default:
		return fmt.Errorf("unknown sandbox mode %q: want off, on, or auto", mode)
	}

	c.mu.Lock()
	if c.sandbox != mode {
		c.sandbox = mode
		c.revision++
	}
	c.mu.Unlock()
	return nil
}

// SetFullAccess forces host-direct execution. The requested sandbox setting is
// retained so leaving yolo can restore it, but CommandPolicy and Summary never
// claim it is active while full access is selected.
func (c *Controller) SetFullAccess(enabled bool) {
	if c == nil {
		return
	}
	c.mu.Lock()
	if c.fullAccess != enabled {
		c.fullAccess = enabled
		c.revision++
	}
	c.mu.Unlock()
}

func (c *Controller) FullAccess() bool {
	if c == nil {
		return false
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.fullAccess
}

// SandboxActive reports effective verified confinement, not merely a user
// preference or an installed mechanism.
func (c *Controller) SandboxActive() bool {
	return c.CommandPolicy(false).SandboxActive
}

// CommandPolicy returns the effective command posture. An unconfined process
// cannot have loopback-only networking imposed by this package, so NetworkFull
// is reported even when the model omitted its network hint.
func (c *Controller) CommandPolicy(requestedNetwork bool) CommandPolicy {
	if c == nil {
		return CommandPolicy{SandboxMode: SandboxOff, Network: NetworkFull}
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.commandPolicyLocked(requestedNetwork)
}

func (c *Controller) commandPolicyLocked(requestedNetwork bool) CommandPolicy {
	policy := CommandPolicy{
		Revision:    c.revision,
		SandboxMode: c.sandbox,
		FullAccess:  c.fullAccess,
		Network:     NetworkFull,
	}
	if c.fullAccess || c.sandbox == SandboxOff {
		return policy
	}
	if confinement := c.capability.Confinement(); confinement != nil {
		policy.SandboxActive = true
		// Both current profiles leave some host Unix-domain services visible.
		// Those services retain their own filesystem/network authority, so this
		// is modeled separately from direct socket egress and permission modes
		// can require a human instead of treating confinement as complete.
		policy.HostIPCShared = confinement.Mechanism() == MechanismSeatbelt || confinement.Mechanism() == MechanismBubblewrap
		policy.Confinement = confinement
		if !requestedNetwork {
			policy.Network = NetworkLoopback
			policy.HostLoopbackShared = confinement.Mechanism() == MechanismSeatbelt
		}
	}
	return policy
}

// Validate rejects a command whose reach changed after its permission request
// was evaluated. Refusing is safer than either alternative: applying the new
// posture would run with reach nobody approved, while applying a stale posture
// would make the live /sandbox display false for a process still starting.
func (c *Controller) Validate(policy CommandPolicy, requestedNetwork bool) error {
	if c == nil {
		if policy != (CommandPolicy{SandboxMode: SandboxOff, Network: NetworkFull}) {
			return errors.New("execution posture changed after permission was evaluated; review the command again")
		}
		return nil
	}
	current := c.CommandPolicy(requestedNetwork)
	return validateCommandPolicy(policy, current)
}

func validateCommandPolicy(policy, current CommandPolicy) error {
	if current.Revision != policy.Revision ||
		current.SandboxMode != policy.SandboxMode ||
		current.SandboxActive != policy.SandboxActive ||
		current.FullAccess != policy.FullAccess ||
		current.HostLoopbackShared != policy.HostLoopbackShared ||
		current.HostIPCShared != policy.HostIPCShared ||
		current.Confinement != policy.Confinement ||
		current.Network != policy.Network {
		return fmt.Errorf("execution posture changed after permission was evaluated (revision %d to %d); review the command again", policy.Revision, current.Revision)
	}
	return nil
}

// Hold validates and pins one exact execution posture until release. SetMode
// and /sandbox updates wait, so a controller cannot change after Validate but
// before the child process starts (or while the old posture is still running).
func (c *Controller) Hold(policy CommandPolicy, requestedNetwork bool) (release func(), err error) {
	if c == nil {
		if err := validateCommandPolicy(policy, CommandPolicy{SandboxMode: SandboxOff, Network: NetworkFull}); err != nil {
			return nil, err
		}
		return func() {}, nil
	}
	c.mu.RLock()
	if err := validateCommandPolicy(policy, c.commandPolicyLocked(requestedNetwork)); err != nil {
		c.mu.RUnlock()
		return nil, err
	}
	return c.mu.RUnlock, nil
}

// Summary is intentionally blunt because it is shown in banners and approval
// dialogs. Availability belongs to Capability.Summary; this reports the
// posture commands actually receive now.
func (c *Controller) Summary() string {
	if c == nil {
		return "SANDBOX OFF: approved commands have full host filesystem and network access"
	}
	policy := c.CommandPolicy(false)
	capability := c.Capability()
	switch {
	case policy.FullAccess:
		summary := "FULL HOST ACCESS: sandbox off; commands can access files outside the workspace and the network"
		if capability.Platform == "windows" {
			summary += "; descendant processes may survive cancellation"
		}
		return summary
	case policy.HostLoopbackShared:
		return string(capability.Mechanism) + " sandbox active; direct writes limited to workspace/temp/build caches, broad outside-home reads remain; host loopback and IPC services remain trusted"
	case policy.SandboxActive:
		return string(capability.Mechanism) + " sandbox active; direct writes limited to workspace/temp/build caches, broad outside-home reads remain; private loopback, but host IPC services remain trusted"
	case policy.SandboxMode == SandboxAuto:
		return "SANDBOX AUTO, UNAVAILABLE: commands run with full host filesystem and network access after approval (" + capability.Detail + ")"
	default:
		return "SANDBOX OFF: approved commands have full host filesystem and network access"
	}
}
