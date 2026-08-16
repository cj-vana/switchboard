package session

import (
	"os"
	"strings"
	"testing"

	"github.com/cj-vana/switchboard/internal/provider"
)

// forkFixture records two turns: a plain exchange, then a turn with a tool
// round, with usage after each assistant message.
func forkFixture(t *testing.T) (*Store, *Session) {
	t.Helper()
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	sess, err := store.Create(t.TempDir(), "scripted/local/test", "rev-1")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { sess.Close() })

	append := func(m provider.Message) {
		t.Helper()
		if err := sess.AppendMessage(m); err != nil {
			t.Fatal(err)
		}
	}
	usage := func(cost int64) {
		t.Helper()
		if err := sess.AppendUsage(Usage{Usage: provider.Usage{InputTokens: 10, OutputTokens: 5}, CostMicroUSD: cost}); err != nil {
			t.Fatal(err)
		}
	}

	append(provider.UserText("first question"))
	append(provider.Message{Role: provider.RoleAssistant, Content: []provider.Block{provider.Text{Text: "first answer"}}})
	usage(100)
	append(provider.UserText("second question"))
	append(provider.Message{Role: provider.RoleAssistant, Content: []provider.Block{
		provider.ToolUse{ID: "call_1", Name: "read", Input: []byte(`{"path":"a.txt"}`)},
	}})
	usage(200)
	append(provider.Message{Role: provider.RoleTool, Content: []provider.Block{
		provider.ToolResult{ToolUseID: "call_1", Name: "read", Content: "contents"},
	}})
	append(provider.Message{Role: provider.RoleAssistant, Content: []provider.Block{provider.Text{Text: "second answer"}}})
	usage(300)
	return store, sess
}

func TestForkCopiesThePrefixAndItsUsage(t *testing.T) {
	store, src := forkFixture(t)

	fork, err := store.Fork(src.ID(), 2) // the first turn and its usage
	if err != nil {
		t.Fatal(err)
	}
	defer fork.Close()

	state := fork.State()
	if state.ID == src.ID() {
		t.Fatal("a fork must be a new session, not the source reopened")
	}
	if len(state.Messages) != 2 {
		t.Fatalf("fork holds %d messages, want 2", len(state.Messages))
	}
	if state.Messages[1].Role != provider.RoleAssistant || state.Messages[1].Text() != "first answer" {
		t.Errorf("fork's last message = %+v, want the first turn's answer", state.Messages[1])
	}
	if state.CostMicroUSD != 100 {
		t.Errorf("fork cost = %d, want only the kept turn's 100", state.CostMicroUSD)
	}
	if state.Workspace != src.State().Workspace || state.CatalogRevision != "rev-1" {
		t.Error("workspace and catalog revision must carry over")
	}
}

func TestForkNeverWritesTheSource(t *testing.T) {
	store, src := forkFixture(t)
	before, err := os.ReadFile(src.Path())
	if err != nil {
		t.Fatal(err)
	}

	fork, err := store.Fork(src.ID(), 6)
	if err != nil {
		t.Fatal(err)
	}
	fork.Close()

	after, err := os.ReadFile(src.Path())
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatal("the source log changed; a fork reads, never writes")
	}
}

func TestForkRefusesACutInsideATurn(t *testing.T) {
	store, src := forkFixture(t)

	// Message 4 is the assistant's tool call: cutting there would drop the
	// call's results while keeping the call.
	_, err := store.Fork(src.ID(), 4)
	if err == nil || !strings.Contains(err.Error(), "inside a turn") {
		t.Fatalf("err = %v, want the mid-turn cut refused", err)
	}
}

func TestForkBoundsAndProvenance(t *testing.T) {
	store, src := forkFixture(t)

	if _, err := store.Fork(src.ID(), 0); err == nil {
		t.Error("keeping zero messages must refuse; that is /clear")
	}
	if _, err := store.Fork(src.ID(), 99); err == nil {
		t.Error("keeping more than the session holds must refuse")
	}
	if _, err := store.Fork("no-such-id", 1); err == nil {
		t.Error("an unknown session must refuse")
	}

	// The full-copy fork resumes cleanly from disk, records where it came
	// from, and shares the complete history.
	fork, err := store.Fork(src.ID(), 6)
	if err != nil {
		t.Fatal(err)
	}
	id := fork.ID()
	fork.Close()
	reopened, err := store.Open(id)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if got := len(reopened.State().Messages); got != 6 {
		t.Errorf("reopened fork holds %d messages, want all 6", got)
	}
	if reopened.State().CostMicroUSD != 600 {
		t.Errorf("reopened fork cost = %d, want the full 600", reopened.State().CostMicroUSD)
	}
}
