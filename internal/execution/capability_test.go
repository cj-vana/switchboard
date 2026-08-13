package execution

import (
	"strings"
	"testing"
)

func TestCapabilityNeverClaimsUnverifiedContainment(t *testing.T) {
	c := Detect()
	if c.PolicyVerified {
		t.Fatal("no sandbox profile has passed the §11 spike; claiming otherwise would gate automatic execution on nothing")
	}
	if c.AutomaticExecutionAllowed() {
		t.Fatal("automatic execution must follow PolicyVerified, not MechanismPresent")
	}
	if !strings.Contains(c.Summary(), "no verified sandbox") {
		t.Errorf("the summary must say plainly that there is no sandbox, got %q", c.Summary())
	}
	if c.Platform == "" {
		t.Error("capability report did not name the platform")
	}
}
