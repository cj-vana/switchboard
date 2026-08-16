package session

import (
	"testing"

	"github.com/cj-vana/switchboard/internal/provider"
)

func TestReadOpeningStopsAtTheFirstUserWords(t *testing.T) {
	store, workspace := newStore(t)

	sess, err := store.Create(workspace, "ollama/local/qwen3.5:9b-mlx", "rev")
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()

	// A user message with no text — a bare screenshot — is not the user
	// speaking, so the opening is the first message that carries words.
	if err := sess.AppendMessage(provider.Message{
		Role:    provider.RoleUser,
		Content: []provider.Block{provider.Image{MediaType: "image/png", Data: []byte{1}}},
	}); err != nil {
		t.Fatal(err)
	}
	if err := sess.AppendMessage(provider.UserText("fix the flaky auth test")); err != nil {
		t.Fatal(err)
	}
	if err := sess.AppendMessage(provider.Message{
		Role:    provider.RoleAssistant,
		Content: []provider.Block{provider.Text{Text: "looking at it"}},
	}); err != nil {
		t.Fatal(err)
	}

	// The session is still open for appending: labelling a listing must not
	// need the lock, same posture as ReadState.
	opening, err := ReadOpening(sess.Path())
	if err != nil {
		t.Fatal(err)
	}
	if opening != "fix the flaky auth test" {
		t.Fatalf("opening = %q, want the first user words", opening)
	}
}

func TestReadUsagesKeepsTheCallsApart(t *testing.T) {
	store, workspace := newStore(t)

	sess, err := store.Create(workspace, "ollama/local/qwen3.5:9b-mlx", "rev")
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()

	for i, in := range []int{1_000, 250_000} {
		if err := sess.AppendUsage(Usage{
			Target: "a/b/c",
			Usage:  provider.Usage{InputTokens: in, OutputTokens: 10 * (i + 1)},
		}); err != nil {
			t.Fatal(err)
		}
	}

	// Per call, in order, while the session is open: counterfactual pricing
	// bands by the size of one call, so the sum replay keeps is not enough.
	usages, err := ReadUsages(sess.Path())
	if err != nil {
		t.Fatal(err)
	}
	if len(usages) != 2 {
		t.Fatalf("got %d usage records, want 2", len(usages))
	}
	if usages[0].Usage.InputTokens != 1_000 || usages[1].Usage.InputTokens != 250_000 {
		t.Fatalf("calls out of order or merged: %+v", usages)
	}
}

func TestReadOpeningOnASessionWithNoUserTurn(t *testing.T) {
	store, workspace := newStore(t)

	sess, err := store.Create(workspace, "ollama/local/qwen3.5:9b-mlx", "rev")
	if err != nil {
		t.Fatal(err)
	}
	path := sess.Path()
	if err := sess.Close(); err != nil {
		t.Fatal(err)
	}

	opening, err := ReadOpening(path)
	if err != nil {
		t.Fatalf("an empty log is a session, not a failure: %v", err)
	}
	if opening != "" {
		t.Fatalf("opening = %q, want empty", opening)
	}
}
