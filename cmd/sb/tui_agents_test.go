package main

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/switchboard-code/switchboard/internal/delegate"
	"github.com/switchboard-code/switchboard/internal/provider"
	"github.com/switchboard-code/switchboard/internal/session"
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

func TestForkCommandBranchesAndLeavesTurnsBehind(t *testing.T) {
	m := testModel(t)
	store, err := session.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	sess, err := store.Create(t.TempDir(), "ollama/local/test:7b", "test")
	if err != nil {
		t.Fatal(err)
	}
	m.app.store = store
	m.app.loop.Session = sess
	say := func(msg provider.Message) {
		t.Helper()
		if err := sess.AppendMessage(msg); err != nil {
			t.Fatal(err)
		}
	}
	say(provider.UserText("one"))
	say(provider.Message{Role: provider.RoleAssistant, Content: []provider.Block{provider.Text{Text: "answer one"}}})
	say(provider.UserText("two"))
	say(provider.Message{Role: provider.RoleAssistant, Content: []provider.Block{provider.Text{Text: "answer two"}}})
	srcID := sess.ID()

	m.ta.SetValue("/fork 1")
	cmd := m.submit()
	if cmd == nil {
		t.Fatal("/fork 1 produced no command")
	}
	m.Update(cmd())

	got := m.app.loop.Session
	if got.ID() == srcID {
		t.Fatal("the session did not swap to the fork")
	}
	msgs := got.State().Messages
	if len(msgs) != 2 || msgs[1].Text() != "answer one" {
		t.Fatalf("fork holds %d messages ending %q, want the first turn only", len(msgs), msgs[len(msgs)-1].Text())
	}
	joined := strings.Join(m.tr.flat, "\n")
	if !strings.Contains(joined, "forked from "+srcID) {
		t.Errorf("the swap did not say where the fork came from:\n%s", joined)
	}
	got.Close()
}

func TestForkCommandRefusesDroppingEverything(t *testing.T) {
	m := testModel(t)
	store, err := session.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	sess, err := store.Create(t.TempDir(), "ollama/local/test:7b", "test")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { sess.Close() })
	m.app.store = store
	m.app.loop.Session = sess
	if err := sess.AppendMessage(provider.UserText("only turn")); err != nil {
		t.Fatal(err)
	}

	m.ta.SetValue("/fork 1")
	if cmd := m.submit(); cmd != nil {
		if msg := cmd(); msg != nil {
			m.Update(msg)
		}
	}
	if m.app.loop.Session.ID() != sess.ID() {
		t.Fatal("dropping the only turn should not have swapped sessions")
	}
	last := m.tr.last()
	if last == nil || last.level != "error" {
		t.Fatalf("want an error notice naming /clear, got %+v", last)
	}
}
