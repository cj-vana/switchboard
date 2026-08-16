package main

import (
	"strings"
	"testing"

	"github.com/cj-vana/switchboard/internal/delegate"
)

func TestAgentsCommandListsDefinitionsAndNotes(t *testing.T) {
	m := testModel(t)
	m.app.agents = []delegate.Agent{
		{Name: "reviewer", Description: "reviews a diff", Tier: "t2", Tools: []string{"read", "grep"}},
		{Name: "scout", Description: "finds things", FromHome: true},
	}
	m.app.agentNotes = []string{"agent bad.md: grants \"bash\""}

	m.ta.SetValue("/agents")
	m.submit()
	joined := strings.Join(m.tr.flat, "\n")
	for _, want := range []string{"reviewer", "t2", "read, grep", "scout", "the full core suite", "~/.switchboard/agents", `grants "bash"`} {
		if !strings.Contains(joined, want) {
			t.Errorf("/agents missing %q:\n%s", want, joined)
		}
	}
	// A rungless agent runs on the ladder's bottom, and the listing says so
	// rather than leaving a blank the user has to interpret.
	if !strings.Contains(joined, "scout") || !strings.Contains(joined, "t1") {
		t.Errorf("/agents did not resolve scout's default rung:\n%s", joined)
	}
}

func TestAgentsCommandExplainsWhenEmpty(t *testing.T) {
	m := testModel(t)
	m.ta.SetValue("/agents")
	m.submit()
	joined := strings.Join(m.tr.flat, "\n")
	if !strings.Contains(joined, ".switchboard/agents") {
		t.Fatalf("/agents on an empty session should say where definitions live:\n%s", joined)
	}
}
