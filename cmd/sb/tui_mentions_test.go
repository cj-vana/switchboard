package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMentionTokenFindsOnlyTheActiveToken(t *testing.T) {
	cases := []struct {
		input, want string
	}{
		{"look at @cmd/sb", "cmd/sb"},
		{"@a", "a"},
		{"@", ""},                        // nothing typed yet
		{"mail me user@example.com ", ""}, // cursor past a space
		{"plain text", ""},
		{"two @first then @sec", "sec"},
	}
	for _, c := range cases {
		if got := mentionToken(c.input); got != c.want {
			t.Errorf("mentionToken(%q) = %q, want %q", c.input, got, c.want)
		}
	}
}

func TestExpandMentionsAttachesOnlyRealFiles(t *testing.T) {
	m := testModel(t)
	ws := t.TempDir()
	m.app.workspace = ws
	if err := os.WriteFile(filepath.Join(ws, "notes.txt"), []byte("the contents"), 0o600); err != nil {
		t.Fatal(err)
	}

	got := m.expandMentions("summarize @notes.txt and email bob@example.com")
	if !strings.Contains(got, "the contents") {
		t.Fatalf("mentioned file was not attached:\n%s", got)
	}
	if !strings.Contains(got, "summarize @notes.txt") {
		t.Fatal("the prompt text itself was altered")
	}
	if strings.Contains(got, "Contents of example.com") || strings.Count(got, "Contents of") != 1 {
		t.Fatalf("something that is not a file was attached:\n%s", got)
	}

	if got := m.expandMentions("no mentions here"); got != "no mentions here" {
		t.Fatalf("a mention-free prompt should pass through untouched, got %q", got)
	}
}

func TestShellContextDrainsIntoThePrompt(t *testing.T) {
	m := testModel(t)
	m.onShellDone(shellDoneMsg{command: "git status", output: "clean tree"})

	got := m.shellContext("what changed?")
	for _, want := range []string{"$ git status", "clean tree", "what changed?"} {
		if !strings.Contains(got, want) {
			t.Fatalf("prompt is missing %q:\n%s", want, got)
		}
	}
	if again := m.shellContext("next"); again != "next" {
		t.Fatal("shell context should drain once, not repeat on every turn")
	}
}
