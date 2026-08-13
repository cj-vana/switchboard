package execution

import "os"

// sandbox-exec is the front end to Seatbelt. It has carried a deprecation
// warning for years while remaining functional and widely relied upon, and
// Apple has never committed to its stability for third parties. Its presence is
// therefore evidence the mechanism exists, and nothing more.
func detectPlatform() Capability {
	const frontEnd = "/usr/bin/sandbox-exec"

	if _, err := os.Stat(frontEnd); err != nil {
		return Capability{
			Mechanism: MechanismNone,
			Detail:    "sandbox-exec is not present on this system",
		}
	}
	return Capability{
		Mechanism:        MechanismSeatbelt,
		MechanismPresent: true,
		Detail:           "sandbox-exec is present but no profile has passed the containment spike",
	}
}
