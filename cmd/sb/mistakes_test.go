package main

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/cj-vana/switchboard/internal/provider"
	"github.com/cj-vana/switchboard/internal/session"
)

// appendFailingRun records one exec test run that failed, the shape the loop
// writes: the call in an assistant message, the result in a user message.
func appendFailingRun(t *testing.T, sess *session.Session, callID string, command []string, output string) {
	t.Helper()
	input, err := json.Marshal(map[string]any{"command": command})
	if err != nil {
		t.Fatal(err)
	}
	if err := sess.AppendMessage(provider.Message{
		Role:    provider.RoleAssistant,
		Content: []provider.Block{provider.ToolUse{ID: callID, Name: "exec", Input: input}},
	}); err != nil {
		t.Fatal(err)
	}
	if err := sess.AppendMessage(provider.Message{
		Role:    provider.RoleUser,
		Content: []provider.Block{provider.ToolResult{ToolUseID: callID, Name: "exec", Content: output, IsError: true}},
	}); err != nil {
		t.Fatal(err)
	}
}

// The ledger's bar is a second session: the same signature met by two
// recorded sessions is a standing problem, one session's repeats are an
// afternoon's debugging, and a command that is not a test run never counts,
// because the ledger shares the escalation detector's gate.
func TestMistakesReportsOnlyCrossSessionRecurrence(t *testing.T) {
	store, err := session.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	workspace := t.TempDir()

	first, err := store.Create(workspace, "ollama/local/qwen3:4b", "test")
	if err != nil {
		t.Fatal(err)
	}
	// Digits differ between the two runs; the signature must not.
	appendFailingRun(t, first, "c1", []string{"go", "test", "./..."}, "--- FAIL: TestRunnerRace (0.03s)")
	appendFailingRun(t, first, "c2", []string{"go", "test", "./..."}, "--- FAIL: TestOnlyOnce (0.01s)")
	first.Close()

	second, err := store.Create(workspace, "ollama/local/qwen3:4b", "test")
	if err != nil {
		t.Fatal(err)
	}
	appendFailingRun(t, second, "c3", []string{"go", "test", "./..."}, "--- FAIL: TestRunnerRace (0.11s)")
	// A failing command that is not a test run stays out, however often it
	// recurs: the detector's gate is the ledger's gate.
	appendFailingRun(t, second, "c4", []string{"rm", "missing"}, "ERROR: no such file")
	second.Close()

	out := strings.Join(mistakesLines(store, workspace), "\n")

	if !strings.Contains(out, "TestRunnerRace") {
		t.Errorf("the recurring failure is missing:\n%s", out)
	}
	if !strings.Contains(out, "2 sessions") || !strings.Contains(out, "2 failing runs") {
		t.Errorf("the entry must count runs and sessions:\n%s", out)
	}
	if strings.Contains(out, "TestOnlyOnce") {
		t.Errorf("a failure one session met is not a recurrence:\n%s", out)
	}
	if strings.Contains(out, "no such file") {
		t.Errorf("a non-test failure crossed the detector's gate:\n%s", out)
	}
	if !strings.Contains(out, "/resume") || !strings.Contains(out, "/learn") {
		t.Errorf("the ledger must name the next actions:\n%s", out)
	}
	if !strings.Contains(out, "outside the exec tool") {
		t.Errorf("the scope boundary is not stated:\n%s", out)
	}
}

// A fork's copied prefix is the same observation carried over, never a
// second meeting: two logs holding the byte-identical failure record are
// one occurrence in one session, so nothing recurs.
func TestMistakesCountsAForkCopiedFailureOnce(t *testing.T) {
	store, err := session.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	workspace := t.TempDir()

	src, err := store.Create(workspace, "ollama/local/qwen3:4b", "test")
	if err != nil {
		t.Fatal(err)
	}
	appendFailingRun(t, src, "c1", []string{"go", "test", "./..."}, "--- FAIL: TestCopied (0.03s)")
	n := len(src.State().Messages)
	src.Close()

	fork, err := store.Fork(src.State().ID, n)
	if err != nil {
		t.Fatal(err)
	}
	fork.Close()

	out := strings.Join(mistakesLines(store, workspace), "\n")
	if strings.Contains(out, "TestCopied") {
		t.Errorf("a copied failure counted as a second meeting:\n%s", out)
	}
	if !strings.Contains(out, "no failure recurred") {
		t.Errorf("nothing recurred and the output must say so:\n%s", out)
	}
}

// A fresh failure in the fork is a real second meeting: the origin met it,
// the fork met it again, and the attribution of the shared record goes to
// the origin whichever log was touched last.
func TestMistakesSeesAFailureThatSurvivedAFork(t *testing.T) {
	store, err := session.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	workspace := t.TempDir()

	src, err := store.Create(workspace, "ollama/local/qwen3:4b", "test")
	if err != nil {
		t.Fatal(err)
	}
	appendFailingRun(t, src, "c1", []string{"go", "test", "./..."}, "--- FAIL: TestSurvivor (0.03s)")
	n := len(src.State().Messages)
	srcID := src.State().ID
	src.Close()

	fork, err := store.Fork(srcID, n)
	if err != nil {
		t.Fatal(err)
	}
	appendFailingRun(t, fork, "c2", []string{"go", "test", "./..."}, "--- FAIL: TestSurvivor (0.21s)")
	forkID := fork.State().ID
	fork.Close()

	out := strings.Join(mistakesLines(store, workspace), "\n")
	if !strings.Contains(out, "TestSurvivor") || !strings.Contains(out, "2 sessions") {
		t.Errorf("a failure met again after a fork must recur across both logs:\n%s", out)
	}
	if !strings.Contains(out, srcID) || !strings.Contains(out, forkID) {
		t.Errorf("both logs earned a place in the entry:\n%s", out)
	}
}

func TestMistakesWithNoHistorySaysSo(t *testing.T) {
	store, err := session.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	out := strings.Join(mistakesLines(store, t.TempDir()), "\n")
	if !strings.Contains(out, "no sessions recorded") {
		t.Errorf("an empty history did not say so: %s", out)
	}
}
