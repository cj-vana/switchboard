package main

import (
	"bufio"
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/cj-vana/switchboard/internal/permission"
	route "github.com/cj-vana/switchboard/internal/router"
	"github.com/cj-vana/switchboard/internal/tools"
)

func testWatcher(t *testing.T, policy route.Policy) (*watcher, *bytes.Buffer, *int) {
	t.Helper()
	var buf bytes.Buffer
	out := &renderer{w: bufio.NewWriter(&buf), atLineTop: true}

	moved := 0
	sticky := route.NewSticky(policy, 0)
	w := newWatcher(out, sticky, 2, func(int, string) { moved++ })
	return w, &buf, &moved
}

func execReq(argv string) permission.Request {
	return permission.Request{Tool: "exec", Argv: strings.Fields(argv)}
}

// The connection this exists to prove: a loop event reaches the escalation
// policy and moves the primary. Everything below it was tested in isolation and
// none of it did anything until this was wired.
func TestARepeatedCallEscalatesThePrimary(t *testing.T) {
	w, buf, moved := testWatcher(t, route.Policy{MinimumDwell: 1})

	w.ToolStart("exec", execReq("go test ./..."))
	w.ToolEnd("exec", tools.Result{Content: "FAIL"}, time.Millisecond)

	// The same command with the same arguments cannot be making progress.
	w.ToolStart("exec", execReq("go test ./..."))
	w.ToolEnd("exec", tools.Result{Content: "FAIL"}, time.Millisecond)

	if *moved == 0 {
		t.Fatalf("a repeated tool call did not move the primary:\n%s", buf.String())
	}
	if !strings.Contains(buf.String(), "escalated") {
		t.Errorf("the move was not reported to the user:\n%s", buf.String())
	}
	if !strings.Contains(buf.String(), "repeated") {
		t.Errorf("the reason was not shown:\n%s", buf.String())
	}
}

// A held switch is reported too. Otherwise the dwell looks like the policy
// doing nothing at all.
func TestAHeldSwitchIsReported(t *testing.T) {
	w, buf, moved := testWatcher(t, route.Policy{MinimumDwell: 5})

	w.ToolStart("exec", execReq("go test ./..."))
	w.ToolEnd("exec", tools.Result{Content: "FAIL"}, time.Millisecond)
	w.ToolStart("exec", execReq("go test ./..."))
	w.ToolEnd("exec", tools.Result{Content: "FAIL"}, time.Millisecond)

	if *moved != 0 {
		t.Error("the primary moved inside the dwell")
	}
	if !strings.Contains(buf.String(), "would escalate") {
		t.Errorf("a warranted but held switch was not reported:\n%s", buf.String())
	}
}

// One distinct test failure is half the threshold, so a run that fails the same
// way twice does not escalate. Escalating there would move on persistence
// rather than difficulty.
func TestTheSameFailureTwiceDoesNotEscalate(t *testing.T) {
	w, buf, moved := testWatcher(t, route.Policy{MinimumDwell: 1})

	const output = "--- FAIL: TestAdd (0.00s)\n    main_test.go:7: Add(2,2) = 0, want 4\n"
	// Different arguments each time, so the repeat-call rule does not fire and
	// this measures the failure rule alone.
	w.ToolStart("exec", execReq("go test ./pkg/a"))
	w.ToolEnd("exec", tools.Result{Content: output, IsError: true}, time.Millisecond)
	w.ToolStart("exec", execReq("go test ./pkg/b"))
	w.ToolEnd("exec", tools.Result{Content: output, IsError: true}, time.Millisecond)

	if *moved != 0 {
		t.Errorf("the same failure twice escalated:\n%s", buf.String())
	}
}

// Two different failures reach the threshold together.
func TestTwoDistinctFailuresEscalate(t *testing.T) {
	w, buf, moved := testWatcher(t, route.Policy{MinimumDwell: 1})

	w.ToolStart("exec", execReq("go test ./pkg/a"))
	w.ToolEnd("exec", tools.Result{
		Content: "--- FAIL: TestAdd\n    a_test.go:7: got 0, want 4\n", IsError: true}, time.Millisecond)
	w.ToolStart("exec", execReq("go test ./pkg/b"))
	w.ToolEnd("exec", tools.Result{
		Content: "--- FAIL: TestMul\n    b_test.go:9: got 1, want 6\n", IsError: true}, time.Millisecond)

	if *moved == 0 {
		t.Fatalf("two distinct failures did not escalate:\n%s", buf.String())
	}
}

// Ordinary successful work must not move anything, or the policy would escalate
// every session that does more than one thing.
func TestSuccessfulWorkDoesNotEscalate(t *testing.T) {
	w, buf, moved := testWatcher(t, route.Policy{MinimumDwell: 1})

	for _, path := range []string{"a.go", "b.go", "c.go", "d.go"} {
		w.ToolStart("read", permission.Request{Tool: "read", Path: path})
		w.ToolEnd("read", tools.Result{Content: "package main"}, time.Millisecond)
	}
	w.TextDelta("Done. All three files look consistent.")

	if *moved != 0 {
		t.Errorf("ordinary work escalated:\n%s", buf.String())
	}
}

// Signatures do not carry across turns: §8.3 counts consecutive failures within
// one, and a fresh turn should not inherit the last one's evidence.
func TestStartTurnClearsEvidence(t *testing.T) {
	w, _, moved := testWatcher(t, route.Policy{MinimumDwell: 1})

	w.ToolStart("exec", execReq("go test ./..."))
	w.ToolEnd("exec", tools.Result{Content: "FAIL", IsError: true}, time.Millisecond)

	w.StartTurn()

	// The identical call is no longer a repeat, because the previous turn's
	// calls are gone.
	w.ToolStart("exec", execReq("go test ./..."))
	w.ToolEnd("exec", tools.Result{Content: "FAIL", IsError: true}, time.Millisecond)

	if *moved != 0 {
		t.Error("evidence from the previous turn escalated this one")
	}
}
