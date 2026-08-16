package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cj-vana/switchboard/internal/checkpoint"
	"github.com/cj-vana/switchboard/internal/provider"
)

func appendTurn(t *testing.T, m *tuiModel, question, answer string) {
	t.Helper()
	sess := m.app.loop.Session
	if err := sess.AppendMessage(provider.UserText(question)); err != nil {
		t.Fatal(err)
	}
	if err := sess.AppendMessage(provider.Message{Role: provider.RoleAssistant,
		Content: []provider.Block{provider.Text{Text: answer}}}); err != nil {
		t.Fatal(err)
	}
}

// An injected user-role message — advice, a watch report — must not be
// mistaken for the turn it landed in.
func TestLastTurnOpeningSkipsInjectedMessages(t *testing.T) {
	messages := []provider.Message{
		provider.UserText("the real opening"),
		{Role: provider.RoleAssistant, Content: []provider.Block{
			provider.ToolUse{ID: "c1", Name: "read", Input: []byte(`{}`)},
		}},
		{Role: provider.RoleTool, Content: []provider.Block{
			provider.ToolResult{ToolUseID: "c1", Name: "read", Content: "x"},
		}},
		provider.UserText("[watch] injected mid-turn"),
		{Role: provider.RoleAssistant, Content: []provider.Block{provider.Text{Text: "done"}}},
	}
	if got := lastTurnOpening(messages); got != 0 {
		t.Fatalf("want the real opening at 0, got %d", got)
	}

	messages = append(messages, provider.UserText("second turn"))
	if got := lastTurnOpening(messages); got != 5 {
		t.Fatalf("want the second opening at 5, got %d", got)
	}
}

// A cancelled or round-limited turn ends on its tool results; the prompt
// typed after that tail opened a turn all the same.
func TestLastTurnOpeningAcceptsAPromptAfterAnInterruptedTurn(t *testing.T) {
	messages := []provider.Message{
		provider.UserText("first question"),
		{Role: provider.RoleAssistant, Content: []provider.Block{
			provider.ToolUse{ID: "c1", Name: "exec", Input: []byte(`{}`)},
		}},
		{Role: provider.RoleTool, Content: []provider.Block{
			provider.ToolResult{ToolUseID: "c1", Name: "exec", Content: "cancelled before this call ran", IsError: true},
		}},
		provider.UserText("second question, typed after the esc"),
		{Role: provider.RoleAssistant, Content: []provider.Block{provider.Text{Text: "answer"}}},
	}
	if got := lastTurnOpening(messages); got != 3 {
		t.Fatalf("the opening after the interrupted tail was missed: got %d, want 3", got)
	}
}

// The marker outranks the shape: a marked injection is skipped whatever its
// text says, and an opening that happens to mention the label is kept.
func TestLastTurnOpeningTrustsTheInjectedMarker(t *testing.T) {
	injected := provider.UserText("plain-looking advice with no label")
	injected.Injected = true
	messages := []provider.Message{
		provider.UserText("the opening"),
		{Role: provider.RoleAssistant, Content: []provider.Block{
			provider.ToolUse{ID: "c1", Name: "read", Input: []byte(`{}`)},
		}},
		{Role: provider.RoleTool, Content: []provider.Block{
			provider.ToolResult{ToolUseID: "c1", Name: "read", Content: "x"},
		}},
		injected,
	}
	if got := lastTurnOpening(messages); got != 0 {
		t.Fatalf("a marked injection was taken for an opening: got %d", got)
	}
}

func TestRetryStartDropsTheReplayWhenATurnGotThereFirst(t *testing.T) {
	m := testModel(t)
	m.busy = true
	cmd := m.retryStart(retryStartMsg{prompt: "replay"})
	if cmd == nil {
		t.Fatal("the dropped replay said nothing")
	}
	if msg, ok := cmd().(noticeMsg); !ok || !strings.Contains(msg.text, "before the retry") {
		t.Fatalf("the drop does not say what happened: %+v", msg)
	}
}

func TestRetryForksOffTheLastTurnAndReplaysItsPrompt(t *testing.T) {
	m := testModel(t)
	appendTurn(t, m, "first question", "first answer")
	appendTurn(t, m, "second question", "weak answer")
	source := m.app.loop.Session.ID()

	cmd := cmdRetry(m, "")
	if cmd == nil {
		t.Fatal("retry returned nothing")
	}
	msg, ok := cmd().(sessionSwapMsg)
	if !ok || msg.err != nil {
		t.Fatalf("retry did not produce a swap: %+v", msg)
	}
	defer msg.sess.Close()

	if got := len(msg.sess.State().Messages); got != 2 {
		t.Fatalf("the fork should keep only the first turn, holds %d messages", got)
	}
	if !strings.Contains(msg.note, source) {
		t.Errorf("the note does not say where the set-aside answer lives: %q", msg.note)
	}
	if msg.andThen == nil {
		t.Fatal("the swap carries no continuation")
	}
	start, ok := msg.andThen().(retryStartMsg)
	if !ok || start.prompt != "second question" || start.tier != "" {
		t.Fatalf("the replay is not the recorded opening: %+v", start)
	}
}

func TestRetryLabelsTheSetAsideAnswerOnTheSource(t *testing.T) {
	m := testModel(t)
	appendTurn(t, m, "only question", "answer")

	cmdRetry(m, "")

	found := false
	// The label lands on the source log before the fork cuts, so the
	// session that keeps the set-aside answer also keeps its outcome.
	if data, err := os.ReadFile(m.app.loop.Session.Path()); err == nil {
		found = strings.Contains(string(data), "user_corrected")
	}
	if !found {
		t.Fatal("the source log carries no user_corrected label")
	}
}

func TestRetryOfTheOnlyTurnStartsFresh(t *testing.T) {
	m := testModel(t)
	appendTurn(t, m, "only question", "answer")

	msg, ok := cmdRetry(m, "")().(sessionSwapMsg)
	if !ok || msg.err != nil {
		t.Fatalf("first-turn retry failed: %+v", msg)
	}
	defer msg.sess.Close()
	if !msg.fresh || len(msg.sess.State().Messages) != 0 {
		t.Fatalf("dropping the only turn should start fresh: fresh=%v messages=%d", msg.fresh, len(msg.sess.State().Messages))
	}
	if start, ok := msg.andThen().(retryStartMsg); !ok || start.prompt != "only question" {
		t.Fatalf("the replay lost the prompt: %+v", msg.andThen())
	}
}

func TestRetryTakesBackTheTurnsFilesWhenTheStackTopMatches(t *testing.T) {
	m := testModel(t)
	appendTurn(t, m, "edit the file", "done")

	dir := t.TempDir()
	f := filepath.Join(dir, "a.txt")
	if err := os.WriteFile(f, []byte("before"), 0o644); err != nil {
		t.Fatal(err)
	}
	rec := checkpoint.NewRecorder()
	rec.Begin("edit the file")
	rec.Record(f)
	if err := os.WriteFile(f, []byte("after"), 0o644); err != nil {
		t.Fatal(err)
	}
	m.app.undo = rec

	if msg, ok := cmdRetry(m, "")().(sessionSwapMsg); ok && msg.sess != nil {
		msg.sess.Close()
	}
	if data, _ := os.ReadFile(f); string(data) != "before" {
		t.Fatalf("the turn's file change survived the retry: %q", data)
	}
}

func TestRetryLeavesFilesWhenTheStackTopIsAnotherTurn(t *testing.T) {
	m := testModel(t)
	appendTurn(t, m, "second turn", "done")

	dir := t.TempDir()
	f := filepath.Join(dir, "a.txt")
	if err := os.WriteFile(f, []byte("before"), 0o644); err != nil {
		t.Fatal(err)
	}
	rec := checkpoint.NewRecorder()
	rec.Begin("an earlier turn")
	rec.Record(f)
	if err := os.WriteFile(f, []byte("after"), 0o644); err != nil {
		t.Fatal(err)
	}
	m.app.undo = rec

	if msg, ok := cmdRetry(m, "")().(sessionSwapMsg); ok && msg.sess != nil {
		msg.sess.Close()
	}
	if data, _ := os.ReadFile(f); string(data) != "after" {
		t.Fatalf("retry undid a turn it was not retrying: %q", data)
	}
}

func TestRetryRefusalsSayWhy(t *testing.T) {
	m := testModel(t)
	if msg, ok := cmdRetry(m, "")().(noticeMsg); !ok || !strings.Contains(msg.text, "nothing to retry") {
		t.Errorf("an empty session did not refuse plainly: %+v", msg)
	}

	appendTurn(t, m, "q", "a")
	if msg, ok := cmdRetry(m, "t9")().(noticeMsg); !ok || !strings.Contains(msg.text, "no tier t9") {
		t.Errorf("an unknown tier did not refuse plainly: %+v", msg)
	}
}
