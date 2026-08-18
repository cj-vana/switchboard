package execution

import (
	"strings"
	"testing"
)

func TestSandboxOffIsHostDirectByDefault(t *testing.T) {
	c := NewDefaultController(TestingVerifiedCapability())
	got := c.CommandPolicy(false)
	if got.SandboxMode != SandboxOff || got.SandboxActive || got.Confinement != nil {
		t.Fatalf("default policy = %+v, want unconfined", got)
	}
	if got.Network != NetworkFull {
		t.Errorf("unconfined network = %s, want full", got.Network)
	}
	if !strings.Contains(c.Summary(), "SANDBOX OFF") || !strings.Contains(c.Summary(), "full host") {
		t.Errorf("summary does not make host reach visible: %q", c.Summary())
	}
}

func TestSandboxOnRequiresVerifiedConfinement(t *testing.T) {
	missing := Capability{Platform: "darwin", Mechanism: MechanismSeatbelt, MechanismPresent: true, Detail: "self-test failed"}
	if _, err := NewController(missing, SandboxOn); err == nil || !strings.Contains(err.Error(), "unavailable") {
		t.Fatalf("SandboxOn error = %v, want an unavailable refusal", err)
	}

	c, err := NewController(TestingVerifiedCapability(), SandboxOn)
	if err != nil {
		t.Fatal(err)
	}
	got := c.CommandPolicy(false)
	if !got.SandboxActive || got.Confinement == nil || got.Network != NetworkLoopback {
		t.Fatalf("verified policy = %+v, want confined loopback", got)
	}
	online := c.CommandPolicy(true)
	if online.Network != NetworkFull || online.Confinement == nil {
		t.Fatalf("network-granted policy = %+v", online)
	}
}

func TestSandboxAutoUsesOnlyVerifiedConfinement(t *testing.T) {
	missing := Capability{Platform: "linux", Mechanism: MechanismBubblewrap, MechanismPresent: true, Detail: "self-test failed"}
	c, err := NewController(missing, SandboxAuto)
	if err != nil {
		t.Fatal(err)
	}
	if got := c.CommandPolicy(false); got.SandboxActive || got.Confinement != nil || got.Network != NetworkFull {
		t.Fatalf("unverified auto policy = %+v, want visibly unconfined", got)
	}
	if !strings.Contains(c.Summary(), "UNAVAILABLE") {
		t.Errorf("auto fallback hidden by summary %q", c.Summary())
	}

	verified, err := NewController(TestingVerifiedCapability(), SandboxAuto)
	if err != nil {
		t.Fatal(err)
	}
	if got := verified.CommandPolicy(false); !got.SandboxActive || got.Confinement == nil {
		t.Fatalf("verified auto policy = %+v, want confined", got)
	}
}

func TestFullAccessOverridesAndThenRestoresSandbox(t *testing.T) {
	c, err := NewController(TestingVerifiedCapability(), SandboxOn)
	if err != nil {
		t.Fatal(err)
	}
	c.SetFullAccess(true)
	got := c.CommandPolicy(false)
	if !got.FullAccess || got.SandboxActive || got.Confinement != nil || got.Network != NetworkFull {
		t.Fatalf("full-access policy = %+v", got)
	}
	if !strings.Contains(c.Summary(), "FULL HOST ACCESS") {
		t.Errorf("full access hidden by summary %q", c.Summary())
	}

	c.SetFullAccess(false)
	restored := c.CommandPolicy(false)
	if restored.SandboxMode != SandboxOn || !restored.SandboxActive {
		t.Fatalf("restored policy = %+v, want sandbox on", restored)
	}
}

func TestFailedSandboxChangeLeavesPriorMode(t *testing.T) {
	c := NewDefaultController(Capability{Detail: "not available"})
	if err := c.SetSandbox(SandboxOn); err == nil {
		t.Fatal("expected sandbox-on failure")
	}
	if got := c.SandboxMode(); got != SandboxOff {
		t.Errorf("sandbox mode after failure = %s, want off", got)
	}
}

func TestCommandPolicyRevisionRejectsReachChanges(t *testing.T) {
	c := NewDefaultController(TestingVerifiedCapability())
	hostDirect := c.CommandPolicy(false)
	if err := c.Validate(hostDirect, false); err != nil {
		t.Fatalf("unchanged policy: %v", err)
	}
	if err := c.SetSandbox(SandboxOn); err != nil {
		t.Fatal(err)
	}
	if err := c.Validate(hostDirect, false); err == nil || !strings.Contains(err.Error(), "changed after permission") {
		t.Fatalf("off-to-on validation = %v", err)
	}

	confined := c.CommandPolicy(false)
	c.SetFullAccess(true)
	if err := c.Validate(confined, false); err == nil {
		t.Fatal("on-to-yolo change accepted a stale confined policy")
	}
	c.SetFullAccess(false)
	restored := c.CommandPolicy(false)
	if err := c.Validate(restored, false); err != nil {
		t.Fatalf("fresh restored policy: %v", err)
	}
}

func TestCommandPolicyValidationRejectsTamperedFields(t *testing.T) {
	c := NewDefaultController(TestingVerifiedCapability())
	for name, tamper := range map[string]func(*CommandPolicy){
		"network":              func(p *CommandPolicy) { p.Network = NetworkLoopback },
		"active":               func(p *CommandPolicy) { p.SandboxActive = true },
		"full access":          func(p *CommandPolicy) { p.FullAccess = true },
		"host loopback shared": func(p *CommandPolicy) { p.HostLoopbackShared = true },
		"host IPC shared":      func(p *CommandPolicy) { p.HostIPCShared = !p.HostIPCShared },
		"mode":                 func(p *CommandPolicy) { p.SandboxMode = SandboxOn },
	} {
		t.Run(name, func(t *testing.T) {
			policy := c.CommandPolicy(false)
			tamper(&policy)
			if err := c.Validate(policy, false); err == nil {
				t.Fatal("tampered policy validated")
			}
		})
	}
}

func TestCommandPolicyHoldPinsPostureThroughExecutionWindow(t *testing.T) {
	c := NewDefaultController(TestingVerifiedCapability())
	policy := c.CommandPolicy(false)
	release, err := c.Hold(policy, false)
	if err != nil {
		t.Fatal(err)
	}
	started := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		close(started)
		done <- c.SetSandbox(SandboxOn)
	}()
	<-started
	select {
	case err := <-done:
		t.Fatalf("sandbox changed inside held execution window: %v", err)
	default:
	}
	release()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if err := c.Validate(policy, false); err == nil {
		t.Fatal("old host-direct policy remained valid after held change completed")
	}
}

func TestParseSandboxMode(t *testing.T) {
	for input, want := range map[string]SandboxMode{
		"": SandboxOff, "off": SandboxOff, "false": SandboxOff,
		"on": SandboxOn, "true": SandboxOn, "auto": SandboxAuto,
	} {
		got, err := ParseSandboxMode(input)
		if err != nil || got != want {
			t.Errorf("ParseSandboxMode(%q) = %q, %v; want %q", input, got, err, want)
		}
	}
	if _, err := ParseSandboxMode("maybe"); err == nil {
		t.Error("unknown sandbox mode accepted")
	}
}
