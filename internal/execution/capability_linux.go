package execution

import (
	"os"
	"os/exec"
	"strings"
)

// Linux isolation is the best understood of the three targets: bubblewrap where
// it is installed, unprivileged user namespaces otherwise.
func detectPlatform() Capability {
	if path, err := exec.LookPath("bwrap"); err == nil {
		return Capability{
			Mechanism:        MechanismBubblewrap,
			MechanismPresent: true,
			Detail:           "bubblewrap found at " + path + " but its profile is untested",
		}
	}
	if userNamespacesEnabled() {
		return Capability{
			Mechanism:        MechanismNamespaces,
			MechanismPresent: true,
			Detail:           "unprivileged user namespaces are available but unconfigured",
		}
	}
	return Capability{
		Mechanism: MechanismNone,
		Detail:    "no bubblewrap and unprivileged user namespaces are disabled",
	}
}

func userNamespacesEnabled() bool {
	// Absent on kernels built without the sysctl, where the feature is governed
	// by build config instead. Treating absence as unavailable keeps the report
	// pessimistic, which is the direction that fails safe.
	raw, err := os.ReadFile("/proc/sys/kernel/unprivileged_userns_clone")
	if err != nil {
		return false
	}
	return strings.TrimSpace(string(raw)) == "1"
}
