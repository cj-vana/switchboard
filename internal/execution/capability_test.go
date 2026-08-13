package execution

import (
	"strings"
	"testing"
)

// The invariant the whole design rests on: whether automatic execution is
// allowed and whether a wrapper exists are one fact, not two that could drift.
func TestAutomaticExecutionAndTheWrapperAreOneFact(t *testing.T) {
	c := Detect()
	// Confinement is the only thing that may grant automatic execution, and it
	// is the wrapper itself, so there is no boolean that can disagree with it.
	if c.AutomaticExecutionAllowed() != (c.Confinement() != nil) {
		t.Fatal("automatic execution and the wrapper must be the same fact")
	}
	if !c.AutomaticExecutionAllowed() && !strings.Contains(c.Summary(), "no verified sandbox") {
		t.Errorf("an unverified host must say so plainly, got %q", c.Summary())
	}
	if c.AutomaticExecutionAllowed() && !strings.Contains(c.Summary(), "verified") {
		t.Errorf("a verified host should say so, got %q", c.Summary())
	}
	if c.Platform == "" {
		t.Error("capability report did not name the platform")
	}
}
