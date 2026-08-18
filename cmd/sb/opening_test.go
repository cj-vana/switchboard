package main

import (
	"strings"
	"testing"

	"github.com/switchboard-code/switchboard/internal/provider"
	"github.com/switchboard-code/switchboard/internal/session"
)

func TestOpeningLabelSkipsTheCompactSeedPreamble(t *testing.T) {
	store, err := session.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	workspace := t.TempDir()
	sess, err := store.Create(workspace, "ollama/local/qwen3.5:9b-mlx", "rev")
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()

	// Auto-compaction means the users with the most sessions to tell apart
	// are exactly the ones whose logs open with this seed, and a label made
	// of its preamble would render their whole resume list identical.
	seed := compactSeedHead + "20260801T000000.000000-aaaa). What follows is a summary of that conversation; treat it as established context.\n\nThe auth refactor: token refresh moved into the client, tests pending."
	if err := sess.AppendMessage(provider.UserText(seed)); err != nil {
		t.Fatal(err)
	}

	label := openingLabel(sess.Path())
	if !strings.HasPrefix(label, "The auth refactor:") {
		t.Fatalf("label = %q, want the summary's first words", label)
	}
	if strings.Contains(label, "continues an earlier one") {
		t.Fatalf("label carries the shared preamble: %q", label)
	}
}

func TestOpeningLabelCollapsesAndCuts(t *testing.T) {
	store, err := session.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	sess, err := store.Create(t.TempDir(), "ollama/local/qwen3.5:9b-mlx", "rev")
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()

	long := "fix the\n\nflaky   auth test " + strings.Repeat("and its friends ", 20)
	if err := sess.AppendMessage(provider.UserText(long)); err != nil {
		t.Fatal(err)
	}

	label := openingLabel(sess.Path())
	if strings.ContainsAny(label, "\n") {
		t.Fatalf("label holds a newline: %q", label)
	}
	if !strings.HasPrefix(label, "fix the flaky auth test") {
		t.Fatalf("label = %q", label)
	}
	if len(label) > 60 {
		t.Fatalf("label was not cut to listing width: %d bytes", len(label))
	}
}
