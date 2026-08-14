package router

import (
	"fmt"
	"sync"
	"testing"
)

func has(signals []Signal, want Signal) bool {
	for _, s := range signals {
		if s == want {
			return true
		}
	}
	return false
}

// Loop detection: the same call with the same arguments cannot be making
// progress. It is reported once, or every further repetition escalates again
// on the same evidence.
func TestRepeatedToolCallIsReportedOnce(t *testing.T) {
	d := NewDetector()

	if got := d.ToolCall("read", []byte(`{"path":"main.go"}`)); len(got) != 0 {
		t.Errorf("a first call reported %v", got)
	}
	if got := d.ToolCall("read", []byte(`{"path":"main.go"}`)); !has(got, RepeatedToolCall) {
		t.Error("an identical repeat was not reported")
	}
	if got := d.ToolCall("read", []byte(`{"path":"main.go"}`)); len(got) != 0 {
		t.Errorf("the same repetition was reported twice: %v", got)
	}

	// Different arguments are a different call, not a loop.
	if got := d.ToolCall("read", []byte(`{"path":"other.go"}`)); len(got) != 0 {
		t.Errorf("a different argument was treated as a repeat: %v", got)
	}
}

// Tools fail routinely, so one failure is not news.
func TestErrorSpikeNeedsSeveralFailures(t *testing.T) {
	d := NewDetector()
	d.ErrorSpikeAt = 3

	for i := range 2 {
		if got := d.ToolResult("exec", "ls", "no such file", true); has(got, ToolErrorSpike) {
			t.Fatalf("a spike was reported after %d failures", i+1)
		}
	}
	if got := d.ToolResult("exec", "ls", "no such file", true); !has(got, ToolErrorSpike) {
		t.Error("three failures did not report a spike")
	}
	// Reported once: after that the policy would escalate repeatedly on it.
	if got := d.ToolResult("exec", "ls", "no such file", true); has(got, ToolErrorSpike) {
		t.Error("the spike was reported twice")
	}
}

func TestSuccessfulCallsReportNothing(t *testing.T) {
	d := NewDetector()
	if got := d.ToolResult("exec", "go test ./...", "ok", false); len(got) != 0 {
		t.Errorf("a successful call reported %v", got)
	}
}

// §8.3 counts a *new* failure signature. The same failure twice is one problem
// observed twice, and counting it again escalates for persistence rather than
// difficulty.
func TestOnlyANewTestFailureCounts(t *testing.T) {
	d := NewDetector()
	const output = "--- FAIL: TestThing (0.01s)\n    thing_test.go:42: got 1, want 2\n"

	if got := d.ToolResult("exec", "go test ./...", output, true); !has(got, NewTestFailure) {
		t.Fatal("a first test failure was not reported")
	}
	if got := d.ToolResult("exec", "go test ./...", output, true); has(got, NewTestFailure) {
		t.Error("the same failure was reported as new")
	}

	// A different failure is new.
	other := "--- FAIL: TestOther (0.02s)\n    other_test.go:9: boom\n"
	if got := d.ToolResult("exec", "go test ./...", other, true); !has(got, NewTestFailure) {
		t.Error("a different failure was not reported as new")
	}
}

// Output carries timings and counts that differ between two runs of the same
// broken thing. Comparing whole outputs would make every retry look new.
func TestTheSameFailureSurvivesNoisyOutput(t *testing.T) {
	d := NewDetector()

	first := "ok  \tpkg\t0.412s\n--- FAIL: TestThing (0.01s)\n    thing_test.go:42: got 1, want 2\n"
	second := "ok  \tpkg\t0.998s\n--- FAIL: TestThing (0.03s)\n    thing_test.go:43: got 1, want 2\n"

	if got := d.ToolResult("exec", "go test ./...", first, true); !has(got, NewTestFailure) {
		t.Fatal("the first failure was not reported")
	}
	if got := d.ToolResult("exec", "go test ./...", second, true); has(got, NewTestFailure) {
		t.Error("the same failure with different timings and a shifted line was reported as new")
	}
}

// A failing command that is not a test run is a tool error, not a test failure.
func TestANonTestFailureIsNotATestFailure(t *testing.T) {
	d := NewDetector()
	got := d.ToolResult("exec", "ls /nowhere", "--- FAIL: something", true)
	if has(got, NewTestFailure) {
		t.Error("a non-test command reported a test failure")
	}
}

func TestTestCommandsAreRecognisedAcrossEcosystems(t *testing.T) {
	for _, argv := range []string{
		"go test ./...", "npm test", "pnpm test", "pytest -q",
		"cargo test", "make test", "dotnet test", "mvn test", "npx vitest run",
	} {
		if !looksLikeTests(argv) {
			t.Errorf("%q was not recognised as a test run", argv)
		}
	}
	for _, argv := range []string{"go build ./...", "ls", "git status", "npm install"} {
		if looksLikeTests(argv) {
			t.Errorf("%q was treated as a test run", argv)
		}
	}
}

// Hedging is weak and reported once, so volume cannot stand in for evidence.
func TestHedgingIsReportedOnce(t *testing.T) {
	d := NewDetector()

	if got := d.AssistantText("I'm not sure this is right."); !has(got, UncertaintyLanguage) {
		t.Fatal("hedging was not reported")
	}
	if got := d.AssistantText("It's unclear, and I can't tell."); len(got) != 0 {
		t.Errorf("hedging was reported twice: %v", got)
	}
	if got := NewDetector().AssistantText("Done. It prints hi."); len(got) != 0 {
		t.Errorf("ordinary output was read as hedging: %v", got)
	}
}

// Signatures do not survive a turn: §8.3 counts consecutive failures within
// one, and carrying them across would escalate a fresh turn for something
// already dealt with.
func TestResetClearsPerTurnState(t *testing.T) {
	d := NewDetector()
	d.ToolCall("read", []byte(`{"path":"a"}`))
	d.ToolResult("exec", "go test ./...", "--- FAIL: TestThing", true)
	d.AssistantText("I'm not sure")

	d.Reset()

	if got := d.ToolCall("read", []byte(`{"path":"a"}`)); len(got) != 0 {
		t.Errorf("a call from the previous turn counted as a repeat: %v", got)
	}
	if got := d.ToolResult("exec", "go test ./...", "--- FAIL: TestThing", true); !has(got, NewTestFailure) {
		t.Error("a failure signature outlived its turn")
	}
	if got := d.AssistantText("I'm not sure"); !has(got, UncertaintyLanguage) {
		t.Error("the hedging flag outlived its turn")
	}
}

// The triggers §8.3 lists that need state the loop does not keep are absent
// rather than approximated, because escalating on evidence that does not exist
// is worse than not escalating.
func TestUnsupportedTriggersAreNotGuessedAt(t *testing.T) {
	d := NewDetector()
	var all []Signal
	all = append(all, d.ToolCall("edit", []byte(`{"path":"a","old":"x","new":"y"}`))...)
	all = append(all, d.ToolResult("edit", "", "applied", false)...)
	all = append(all, d.AssistantText("I reverted that change.")...)

	for _, unsupported := range []Signal{EditReverted, DiffGrew} {
		if has(all, unsupported) {
			t.Errorf("%q was emitted from state the loop does not track", unsupported)
		}
	}
}

// The agent loop runs a turn's tool calls in parallel goroutines, so every
// method here is called concurrently. That was documented as untrue in a
// comment and crashed a real corpus run on a concurrent map write after nine
// hundred seconds of work.
//
// Run with -race, which is what would have caught it.
func TestDetectorSurvivesConcurrentUse(t *testing.T) {
	d := NewDetector()

	var wg sync.WaitGroup
	for i := range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range 32 {
				d.ToolCall("read", fmt.Appendf(nil, `{"path":"%d"}`, (i+j)%5))
				d.ToolResult("exec", "go test ./...", "--- FAIL: Test", j%3 == 0)
				d.AssistantText("I'm not sure about this")
			}
		}()
	}
	wg.Wait()
}

func TestStickySurvivesConcurrentUse(t *testing.T) {
	s := NewSticky(Policy{MinimumDwell: 2}, 0)

	var wg sync.WaitGroup
	for range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 32 {
				s.Observe(RepeatedToolCall)
				s.AfterCall(3)
				_ = s.Rank()
				_ = s.EscalatedLastTurn()
			}
		}()
	}
	wg.Wait()
}
