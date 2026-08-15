package main

import (
	"os"
	"path/filepath"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

func TestHistoryRoundTripsThroughDisk(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	ws := filepath.Join(home, "project")
	appendHistory(ws, "first prompt")
	appendHistory(ws, "second\nprompt with a newline")

	got := loadHistory(ws)
	if len(got) != 2 {
		t.Fatalf("loaded %d entries, want 2", len(got))
	}
	if got[1] != "second\nprompt with a newline" {
		t.Fatalf("a multiline prompt did not survive the disk: %q", got[1])
	}

	other := loadHistory(filepath.Join(home, "elsewhere"))
	if len(other) != 0 {
		t.Fatal("another workspace sees this one's history")
	}

	info, err := os.Stat(filepath.Join(home, ".switchboard", "history"))
	if err != nil || !info.IsDir() {
		t.Fatalf("history directory missing: %v", err)
	}
}

func TestReverseSearchFindsNewestFirst(t *testing.T) {
	m := testModel(t)
	m.history = []string{"fix the parser", "run the tests", "fix the linter"}

	m.startHistorySearch()
	for _, r := range "fix" {
		m.historySearchKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
	}
	if hit := m.historyMatch(m.histMatch); hit != "fix the linter" {
		t.Fatalf("first match should be the newest, got %q", hit)
	}

	m.historySearchKey(tea.KeyMsg{Type: tea.KeyCtrlR})
	if hit := m.historyMatch(m.histMatch); hit != "fix the parser" {
		t.Fatalf("ctrl+r should step to the next older match, got %q", hit)
	}

	m.historySearchKey(tea.KeyMsg{Type: tea.KeyEnter})
	if m.histSearch {
		t.Fatal("enter should end the search")
	}
	if got := m.ta.Value(); got != "fix the parser" {
		t.Fatalf("enter should accept the match into the input, got %q", got)
	}
}

func TestReverseSearchEscapeLeavesInputAlone(t *testing.T) {
	m := testModel(t)
	m.history = []string{"something"}
	m.ta.SetValue("draft in progress")

	m.startHistorySearch()
	m.historySearchKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("some")})
	m.historySearchKey(tea.KeyMsg{Type: tea.KeyEsc})

	if m.ta.Value() != "draft in progress" {
		t.Fatalf("escape clobbered the draft: %q", m.ta.Value())
	}
}
