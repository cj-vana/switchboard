//go:build !darwin && !linux

package execution

// Windows has no native containment that meets the same release bar as the
// macOS and Linux mechanisms, which is open question §21.7. Until it does,
// v0.1 there is a plan-and-approve product: the agent reads, proposes, and
// edits under approval, and every command execution requires per-action
// confirmation.
func detectPlatform() Capability {
	return Capability{
		Mechanism: MechanismNone,
		Detail:    "no containment strategy has met the automatic-execution gate on this platform",
	}
}
