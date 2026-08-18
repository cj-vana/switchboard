package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/switchboard-code/switchboard/internal/agent"
	"github.com/switchboard-code/switchboard/internal/permission"
	"github.com/switchboard-code/switchboard/internal/provider"
	route "github.com/switchboard-code/switchboard/internal/router"
	"github.com/switchboard-code/switchboard/internal/session"
	"github.com/switchboard-code/switchboard/internal/tools"
)

func testWatcher(t *testing.T, policy route.Policy) (*watcher, *bytes.Buffer, *int) {
	t.Helper()
	var buf bytes.Buffer
	out := &renderer{w: bufio.NewWriter(&buf), atLineTop: true}

	moved := 0
	sticky := route.NewSticky(policy, 0)
	w := newWatcher(out, sticky, 2, func(context.Context, int, string) (func() bool, func(), bool) {
		return func() bool { moved++; return true }, nil, true
	})
	return w, &buf, &moved
}

func execReq(argv string) permission.Request {
	return permission.Request{Tool: "exec", Argv: strings.Fields(argv)}
}

var nextWatcherCall int

func watcherRound(w *watcher, name string, req permission.Request, res tools.Result) {
	nextWatcherCall++
	input, _ := json.Marshal(struct {
		Path string   `json:"path,omitempty"`
		Argv []string `json:"argv,omitempty"`
	}{Path: req.Path, Argv: req.Argv})
	call := provider.ToolUse{ID: fmt.Sprintf("call-%d", nextWatcherCall), Name: name, Input: input}
	w.TurnUsage(session.Usage{})
	w.ToolStart(call, req)
	w.ToolEnd(call, req, res, time.Millisecond)
	w.ToolBatchEnd(context.Background())
}

// The connection this exists to prove: a loop event reaches the escalation
// policy and moves the primary. Everything below it was tested in isolation and
// none of it did anything until this was wired.
func TestARepeatedCallEscalatesThePrimary(t *testing.T) {
	w, buf, moved := testWatcher(t, route.Policy{MinimumDwell: 1})

	watcherRound(w, "exec", execReq("go test ./..."), tools.Result{Content: "FAIL"})

	// The same command with the same arguments cannot be making progress.
	watcherRound(w, "exec", execReq("go test ./..."), tools.Result{Content: "FAIL"})

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

	watcherRound(w, "exec", execReq("go test ./..."), tools.Result{Content: "FAIL"})
	watcherRound(w, "exec", execReq("go test ./..."), tools.Result{Content: "FAIL"})

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
	watcherRound(w, "exec", execReq("go test ./pkg/a"), tools.Result{Content: output, IsError: true})
	watcherRound(w, "exec", execReq("go test ./pkg/b"), tools.Result{Content: output, IsError: true})

	if *moved != 0 {
		t.Errorf("the same failure twice escalated:\n%s", buf.String())
	}
}

// Two different failures reach the threshold together.
func TestTwoDistinctFailuresEscalate(t *testing.T) {
	w, buf, moved := testWatcher(t, route.Policy{MinimumDwell: 1})

	watcherRound(w, "exec", execReq("go test ./pkg/a"), tools.Result{
		Content: "--- FAIL: TestAdd\n    a_test.go:7: got 0, want 4\n", IsError: true})
	watcherRound(w, "exec", execReq("go test ./pkg/b"), tools.Result{
		Content: "--- FAIL: TestMul\n    b_test.go:9: got 1, want 6\n", IsError: true})

	if *moved == 0 {
		t.Fatalf("two distinct failures did not escalate:\n%s", buf.String())
	}
}

// Ordinary successful work must not move anything, or the policy would escalate
// every session that does more than one thing.
func TestSuccessfulWorkDoesNotEscalate(t *testing.T) {
	w, buf, moved := testWatcher(t, route.Policy{MinimumDwell: 1})

	for _, path := range []string{"a.go", "b.go", "c.go", "d.go"} {
		watcherRound(w, "read", permission.Request{Tool: "read", Path: path}, tools.Result{Content: "package main"})
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

	watcherRound(w, "exec", execReq("go test ./..."), tools.Result{Content: "FAIL", IsError: true})

	w.StartTurn()

	// The identical call is no longer a repeat, because the previous turn's
	// calls are gone.
	watcherRound(w, "exec", execReq("go test ./..."), tools.Result{Content: "FAIL", IsError: true})

	if *moved != 0 {
		t.Error("evidence from the previous turn escalated this one")
	}
}

func TestParallelBatchCountsAsOneModelCall(t *testing.T) {
	w, buf, moved := testWatcher(t, route.Policy{MinimumDwell: 2})
	req := permission.Request{Tool: "read", Path: "same.go"}
	input := json.RawMessage(`{"path":"same.go"}`)
	first := provider.ToolUse{ID: "parallel-1", Name: "read", Input: input}
	second := provider.ToolUse{ID: "parallel-2", Name: "read", Input: input}

	w.TurnUsage(session.Usage{})
	w.ToolStart(first, req)
	w.ToolStart(second, req)
	w.ToolEnd(second, req, tools.Result{Content: "package p"}, time.Millisecond)
	w.ToolEnd(first, req, tools.Result{Content: "package p"}, time.Millisecond)
	w.ToolBatchEnd(context.Background())

	if *moved != 0 || w.sticky.Rank() != 0 {
		t.Fatalf("one parallel batch satisfied a two-call dwell: moved=%d rank=%d", *moved, w.sticky.Rank())
	}
	if !strings.Contains(buf.String(), "served 1 of 2 calls") {
		t.Fatalf("dwell did not count the batch as one model call:\n%s", buf.String())
	}

	// A later model call reaches the dwell, and one batch boundary commits at
	// most one move regardless of how many tools the prior response requested.
	w.TurnUsage(session.Usage{})
	w.ToolBatchEnd(context.Background())
	if *moved != 1 || w.sticky.Rank() != 1 {
		t.Fatalf("second model call did not commit exactly one move: moved=%d rank=%d", *moved, w.sticky.Rank())
	}
}

func TestRejectedDestinationDoesNotAdvanceOrAnnounce(t *testing.T) {
	var buf bytes.Buffer
	out := &renderer{w: bufio.NewWriter(&buf), atLineTop: true}
	sticky := route.NewSticky(route.Policy{MinimumDwell: 1}, 0)
	w := newWatcher(out, sticky, 2, func(context.Context, int, string) (func() bool, func(), bool) { return nil, nil, false })

	watcherRound(w, "read", permission.Request{Tool: "read", Path: "same.go"}, tools.Result{})
	watcherRound(w, "read", permission.Request{Tool: "read", Path: "same.go"}, tools.Result{})

	if sticky.Rank() != 0 || w.MoveCount() != 0 {
		t.Fatalf("rejected move changed policy state: rank=%d moves=%d", sticky.Rank(), w.MoveCount())
	}
	if strings.Contains(buf.String(), "escalated:") {
		t.Fatalf("a rejected move was announced as successful:\n%s", buf.String())
	}
}

func TestCancelledBoundaryCannotMoveThePrimary(t *testing.T) {
	var callbackCalls int
	sticky := route.NewSticky(route.Policy{MinimumDwell: 1}, 0)
	w := newWatcher(agent.NopObserver{}, sticky, 2, func(context.Context, int, string) (func() bool, func(), bool) {
		callbackCalls++
		return nil, nil, true
	})
	w.TurnUsage(session.Usage{})
	w.ToolStart(provider.ToolUse{ID: "same-1", Name: "read", Input: json.RawMessage(`{"path":"same"}`)},
		permission.Request{Tool: "read", Path: "same"})
	w.ToolStart(provider.ToolUse{ID: "same-2", Name: "read", Input: json.RawMessage(`{"path":"same"}`)},
		permission.Request{Tool: "read", Path: "same"})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	w.ToolBatchEnd(ctx)
	if callbackCalls != 0 || sticky.Rank() != 0 {
		t.Fatalf("cancelled batch moved: callbacks=%d rank=%d", callbackCalls, sticky.Rank())
	}
}

func TestMovePublicationRunsAfterStickyCommit(t *testing.T) {
	sticky := route.NewSticky(route.Policy{MinimumDwell: 1}, 0)
	publishedRank := -1
	w := newWatcher(agent.NopObserver{}, sticky, 2, func(context.Context, int, string) (func() bool, func(), bool) {
		return nil, func() { publishedRank = sticky.Rank() }, true
	})
	w.TurnUsage(session.Usage{})
	w.ToolStart(provider.ToolUse{ID: "same-1", Name: "read", Input: json.RawMessage(`{"path":"same"}`)},
		permission.Request{Tool: "read", Path: "same"})
	w.ToolStart(provider.ToolUse{ID: "same-2", Name: "read", Input: json.RawMessage(`{"path":"same"}`)},
		permission.Request{Tool: "read", Path: "same"})
	w.ToolBatchEnd(context.Background())
	if publishedRank != 1 {
		t.Fatalf("move was published at rank %d, want committed rank 1", publishedRank)
	}
}

func TestStaleWatcherProposalNeverRunsDurablePrecommit(t *testing.T) {
	sticky := route.NewSticky(route.Policy{MinimumDwell: 1}, 0)
	precommits := 0
	w := newWatcher(agent.NopObserver{}, sticky, 2, func(context.Context, int, string) (func() bool, func(), bool) {
		// Simulate a permanent surface action landing after destination probing
		// but before the prepared move enters Sticky's atomic apply.
		sticky.Pin(0)
		return func() bool { precommits++; return true }, nil, true
	})
	w.TurnUsage(session.Usage{})
	w.ToolStart(provider.ToolUse{ID: "same-1", Name: "read", Input: json.RawMessage(`{"path":"same"}`)},
		permission.Request{Tool: "read", Path: "same"})
	w.ToolStart(provider.ToolUse{ID: "same-2", Name: "read", Input: json.RawMessage(`{"path":"same"}`)},
		permission.Request{Tool: "read", Path: "same"})
	w.ToolBatchEnd(context.Background())
	if precommits != 0 || sticky.Rank() != 0 || w.MoveCount() != 0 {
		t.Fatalf("stale watcher move ran precommit: calls=%d rank=%d moves=%d", precommits, sticky.Rank(), w.MoveCount())
	}
}

func TestRawToolArgumentsPreventFalseRepeats(t *testing.T) {
	w, _, moved := testWatcher(t, route.Policy{MinimumDwell: 1})
	req := permission.Request{Tool: "grep", Path: "."}
	for i, pattern := range []string{"one", "two"} {
		call := provider.ToolUse{
			ID: fmt.Sprintf("grep-%d", i), Name: "grep",
			Input: json.RawMessage(fmt.Sprintf(`{"path":".","pattern":%q}`, pattern)),
		}
		w.TurnUsage(session.Usage{})
		w.ToolStart(call, req)
		w.ToolEnd(call, req, tools.Result{Content: "match"}, time.Millisecond)
		w.ToolBatchEnd(context.Background())
	}
	if *moved != 0 {
		t.Fatal("different grep inputs were treated as an identical repeated call")
	}
}
