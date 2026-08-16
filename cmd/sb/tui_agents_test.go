package main

import (
	"path/filepath"
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

func TestBudgetCommandSetsShowsAndClears(t *testing.T) {
	m := testModel(t)
	m.app.budget = &budgetState{}
	m.app.config.Path = filepath.Join(t.TempDir(), "config.toml")

	m.ta.SetValue("/budget 2.50")
	if cmd := m.submit(); cmd != nil {
		if msg := cmd(); msg != nil {
			m.Update(msg)
		}
	}
	if got := m.app.budget.get(); got != 2_500_000 {
		t.Fatalf("ceiling = %d micro-dollars, want 2.50 set", got)
	}
	if m.app.config.Budget != 2_500_000 {
		t.Error("the ceiling did not persist to the config")
	}

	m.ta.SetValue("/budget")
	m.submit()
	joined := strings.Join(m.tr.flat, "\n")
	if !strings.Contains(joined, "ceiling") || !strings.Contains(joined, "$2.50") {
		t.Errorf("/budget did not show the ceiling:\n%s", joined)
	}

	m.ta.SetValue("/budget off")
	if cmd := m.submit(); cmd != nil {
		if msg := cmd(); msg != nil {
			m.Update(msg)
		}
	}
	if got := m.app.budget.get(); got != 0 {
		t.Errorf("ceiling = %d after /budget off, want cleared", got)
	}
}

func TestBudgetCommandRejectsJunk(t *testing.T) {
	m := testModel(t)
	m.app.budget = &budgetState{}
	m.ta.SetValue("/budget lots")
	if cmd := m.submit(); cmd != nil {
		if msg := cmd(); msg != nil {
			m.Update(msg)
		}
	}
	if got := m.app.budget.get(); got != 0 {
		t.Errorf("junk input set a ceiling of %d", got)
	}
	last := m.tr.last()
	if last == nil || last.kind != kindNotice || last.level != "error" {
		t.Fatalf("junk input did not produce an error notice: %+v", last)
	}
}
