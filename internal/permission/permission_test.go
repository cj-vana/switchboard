package permission

import (
	"context"
	"errors"
	"testing"

	"github.com/cjvana/switchboard/internal/execution"
)

var (
	noSandbox = execution.Capability{
		Platform:         "darwin",
		Mechanism:        execution.MechanismSeatbelt,
		MechanismPresent: true,
		PolicyVerified:   false,
	}
	verifiedSandbox = execution.Capability{
		Platform:         "linux",
		Mechanism:        execution.MechanismBubblewrap,
		MechanismPresent: true,
		PolicyVerified:   true,
	}
)

func read() Request  { return Request{Tool: "read", Effect: EffectRead, Path: "main.go"} }
func write() Request { return Request{Tool: "edit", Effect: EffectWrite, Path: "main.go"} }
func exec() Request {
	return Request{Tool: "exec", Effect: EffectExecute, Argv: []string{"go", "test", "./..."}}
}

func TestModeDefaults(t *testing.T) {
	cases := []struct {
		mode              Mode
		read, write, exec Decision
	}{
		{ModePlan, Allow, Deny, Deny},
		{ModeDefault, Allow, Ask, Ask},
		{ModeAcceptEdits, Allow, Allow, Ask},
		// bypass would allow execution, but there is no sandbox to bypass into.
		{ModeBypass, Allow, Allow, Ask},
	}

	for _, tc := range cases {
		t.Run(string(tc.mode), func(t *testing.T) {
			e := NewEngine(tc.mode, noSandbox)
			if got := e.Check(read()).Decision; got != tc.read {
				t.Errorf("read = %s, want %s", got, tc.read)
			}
			if got := e.Check(write()).Decision; got != tc.write {
				t.Errorf("write = %s, want %s", got, tc.write)
			}
			if got := e.Check(exec()).Decision; got != tc.exec {
				t.Errorf("exec = %s, want %s", got, tc.exec)
			}
		})
	}
}

// The whole point of design principle 4: without verified containment, no mode
// grants automatic execution, and the reason has to say why so the UI cannot
// render the prompt as if it were a sandbox.
func TestBypassDoesNotGrantExecutionWithoutASandbox(t *testing.T) {
	e := NewEngine(ModeBypass, noSandbox)

	out := e.Check(exec())
	if out.Decision != Ask {
		t.Fatalf("decision = %s, want ask", out.Decision)
	}
	if !out.SandboxAbsent {
		t.Error("the outcome must mark that this prompt stands in for missing isolation")
	}
	if out.Reason == "" {
		t.Error("a downgraded decision needs a reason the user can read")
	}

	verified := NewEngine(ModeBypass, verifiedSandbox)
	if got := verified.Check(exec()); got.Decision != Allow {
		t.Errorf("with a verified sandbox, bypass should allow execution, got %s", got.Decision)
	} else if got.SandboxAbsent {
		t.Error("SandboxAbsent must be false when containment is verified")
	}
}

func TestAllowRuleForExecutionIsStillGatedOnTheSandbox(t *testing.T) {
	e := NewEngine(ModeDefault, noSandbox, Rule{
		Decision:   Allow,
		Tool:       "exec",
		ArgvPrefix: []string{"go", "test"},
	})

	out := e.Check(exec())
	if out.Decision != Ask || !out.SandboxAbsent {
		t.Errorf("a rule must not be able to grant unsandboxed execution, got %+v", out)
	}

	verified := NewEngine(ModeDefault, verifiedSandbox, Rule{
		Decision:   Allow,
		Tool:       "exec",
		ArgvPrefix: []string{"go", "test"},
	})
	if got := verified.Check(exec()).Decision; got != Allow {
		t.Errorf("with containment the same rule should allow, got %s", got)
	}
}

func TestDenyRuleWinsOverEverything(t *testing.T) {
	e := NewEngine(ModeBypass, verifiedSandbox,
		Rule{Decision: Allow, Tool: "exec"},
		Rule{Decision: Deny, Tool: "exec", ArgvPrefix: []string{"rm"}},
	)

	rm := Request{Tool: "exec", Effect: EffectExecute, Argv: []string{"rm", "-rf", "build"}}
	if got := e.Check(rm).Decision; got != Deny {
		t.Errorf("decision = %s, want deny even in bypass with a matching allow rule", got)
	}
	if got := e.Check(exec()).Decision; got != Allow {
		t.Errorf("the deny rule should not have caught an unrelated command, got %s", got)
	}
}

func TestPlanModeCannotBeWidenedByARule(t *testing.T) {
	e := NewEngine(ModePlan, verifiedSandbox,
		Rule{Decision: Allow, Tool: "exec"},
		Rule{Decision: Allow, Tool: "edit"},
	)
	if got := e.Check(exec()).Decision; got != Deny {
		t.Errorf("exec in plan mode = %s, want deny", got)
	}
	if got := e.Check(write()).Decision; got != Deny {
		t.Errorf("write in plan mode = %s, want deny", got)
	}
}

func TestRuleMatching(t *testing.T) {
	shellOnly := true
	e := NewEngine(ModeDefault, verifiedSandbox,
		Rule{Decision: Deny, Effect: EffectWrite, PathGlob: "*.lock"},
		Rule{Decision: Deny, Tool: "exec", Shell: &shellOnly},
	)

	if got := e.Check(Request{Tool: "write", Effect: EffectWrite, Path: "go.lock"}).Decision; got != Deny {
		t.Errorf("path glob did not match: %s", got)
	}
	if got := e.Check(Request{Tool: "write", Effect: EffectWrite, Path: "go.mod"}).Decision; got != Ask {
		t.Errorf("path glob matched too much: %s", got)
	}
	if got := e.Check(Request{Tool: "exec", Effect: EffectExecute, Shell: true, Argv: []string{"ls | wc"}}).Decision; got != Deny {
		t.Errorf("shell-mode rule did not match: %s", got)
	}
	// An argv command falls through to the default mode's answer, which is to
	// ask. What matters is that the shell-only deny rule did not catch it.
	if got := e.Check(exec()).Decision; got != Ask {
		t.Errorf("shell-mode rule wrongly caught an argv command: %s", got)
	}
}

func TestRememberIsExactMatchOnly(t *testing.T) {
	e := NewEngine(ModeDefault, noSandbox)

	approved := exec()
	e.Remember(approved, true)

	out := e.Check(approved)
	if out.Decision != Allow {
		t.Errorf("the remembered command = %s, want allow", out.Decision)
	}
	// The approval stands, but the command still runs uncontained and the
	// outcome must keep saying so.
	if !out.SandboxAbsent {
		t.Error("a remembered approval must not erase the fact that there is no sandbox")
	}

	// A different command must not inherit the approval, or the user has
	// approved something they never saw.
	other := Request{Tool: "exec", Effect: EffectExecute, Argv: []string{"go", "test", "./...", "-run", "TestDelete"}}
	if got := e.Check(other).Decision; got != Ask {
		t.Errorf("a longer argv reused the approval: %s", got)
	}

	shellForm := approved
	shellForm.Shell = true
	if got := e.Check(shellForm).Decision; got != Ask {
		t.Errorf("shell mode reused an argv-mode approval: %s", got)
	}
}

func TestRememberedDenialSticks(t *testing.T) {
	e := NewEngine(ModeDefault, noSandbox)
	e.Remember(write(), false)
	if got := e.Check(write()).Decision; got != Deny {
		t.Errorf("decision = %s, want deny after the user declined", got)
	}
}

type stubAsker struct {
	resp  Response
	err   error
	calls int
	last  Outcome
}

func (s *stubAsker) Ask(_ context.Context, _ Request, out Outcome) (Response, error) {
	s.calls++
	s.last = out
	return s.resp, s.err
}

func TestResolveConsultsTheAsker(t *testing.T) {
	e := NewEngine(ModeDefault, noSandbox)
	asker := &stubAsker{resp: Response{Approved: true, Remember: true}}

	ok, _, err := e.Resolve(context.Background(), asker, exec())
	if err != nil || !ok {
		t.Fatalf("ok = %v, err = %v", ok, err)
	}
	if asker.calls != 1 {
		t.Fatalf("asker called %d times, want 1", asker.calls)
	}
	if !asker.last.SandboxAbsent {
		t.Error("the asker must be told the prompt stands in for missing isolation")
	}

	// Remembering means the second identical call does not prompt again.
	ok, _, err = e.Resolve(context.Background(), asker, exec())
	if err != nil || !ok {
		t.Fatalf("second call: ok = %v, err = %v", ok, err)
	}
	if asker.calls != 1 {
		t.Errorf("asker called %d times, want the remembered answer to be reused", asker.calls)
	}
}

func TestResolveWithoutAnAskerDenies(t *testing.T) {
	e := NewEngine(ModeDefault, noSandbox)
	ok, out, err := e.Resolve(context.Background(), nil, exec())
	if err != nil {
		t.Fatal(err)
	}
	if ok || out.Decision != Deny {
		t.Errorf("a request that cannot be asked about must fail closed, got ok=%v %+v", ok, out)
	}
}

func TestResolvePropagatesAskerError(t *testing.T) {
	want := errors.New("terminal closed")
	e := NewEngine(ModeDefault, noSandbox)
	ok, _, err := e.Resolve(context.Background(), &stubAsker{err: want}, exec())
	if !errors.Is(err, want) {
		t.Errorf("err = %v, want %v", err, want)
	}
	if ok {
		t.Error("a failed prompt must not approve the call")
	}
}

func TestResolveAllowsReadsWithoutPrompting(t *testing.T) {
	asker := &stubAsker{}
	e := NewEngine(ModeDefault, noSandbox)
	ok, _, err := e.Resolve(context.Background(), asker, read())
	if err != nil || !ok {
		t.Fatalf("ok = %v, err = %v", ok, err)
	}
	if asker.calls != 0 {
		t.Error("reads must not prompt")
	}
}

func TestParseMode(t *testing.T) {
	for _, s := range []string{"plan", "default", "acceptEdits", "bypass"} {
		if _, err := ParseMode(s); err != nil {
			t.Errorf("ParseMode(%q): %v", s, err)
		}
	}
	if _, err := ParseMode("yolo"); err == nil {
		t.Error("an unknown mode must be an error, not a silent default")
	}
}
