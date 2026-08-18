package main

import (
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/switchboard-code/switchboard/internal/catalog"
	"github.com/switchboard-code/switchboard/internal/config"
)

// step delivers a message and runs any command it produces, following the
// wizard's message chain the way the Bubble Tea runtime would.
func step(t *testing.T, m *onboardModel, msg tea.Msg) {
	t.Helper()
	for msg != nil {
		_, cmd := m.Update(msg)
		if cmd == nil {
			return
		}
		msg = cmd()
		if _, quit := msg.(tea.QuitMsg); quit {
			return
		}
	}
}

func TestOnboardingBindsT1ForALocalModel(t *testing.T) {
	cfg := &config.Config{Path: filepath.Join(t.TempDir(), config.FileName)}
	m := &onboardModel{
		reg: newProviders("http://127.0.0.1:1", cfg),
		cat: &catalog.Catalog{Revision: "test"},
		cfg: cfg,
		th:  darkTheme(),
	}

	choices := map[string]modelChoice{
		"ollama/small local": {ref: "ollama/small", surface: "local", desc: "pulled locally"},
	}
	step(t, m, onboardChoicesMsg{
		items:   []pickerItem{{id: "ollama/small local", label: "ollama/small"}},
		choices: choices,
	})
	if m.dlg == nil {
		t.Fatal("the model picker never opened")
	}

	// Choose the only model: enter on the first row.
	step(t, m, tea.KeyMsg{Type: tea.KeyEnter})

	if m.cancelled || m.err != nil {
		t.Fatalf("wizard failed: cancelled=%v err=%v", m.cancelled, m.err)
	}
	// A ladder is the point of the tool, so binding one rung asks about the
	// next rather than dropping the user into a session.
	if m.quitting {
		t.Fatal("the wizard should offer another rung, not finish on the first one")
	}
	if m.step != stepMore {
		t.Fatalf("after a bind the wizard is at step %v, want the ladder question", m.step)
	}

	saved, err := config.LoadFile(cfg.Path)
	if err != nil {
		t.Fatal(err)
	}
	tier, ok := saved.Tier("t1")
	if !ok {
		t.Fatal("t1 was not persisted")
	}
	if tier.Target.ModelID != "small" || tier.Target.Provider != "ollama" {
		t.Fatalf("t1 bound to %+v, want ollama/small", tier.Target)
	}

	// The rung is already on disk, so choosing to stop is the end of setup
	// rather than a cancellation of it.
	step(t, m, tea.KeyMsg{Type: tea.KeyDown})
	step(t, m, tea.KeyMsg{Type: tea.KeyEnter})
	if !m.quitting || m.cancelled || m.err != nil {
		t.Fatalf("starting the session should end setup cleanly: quitting=%v cancelled=%v err=%v",
			m.quitting, m.cancelled, m.err)
	}
}

func TestOnboardingEscapeCancelsCleanly(t *testing.T) {
	cfg := &config.Config{Path: filepath.Join(t.TempDir(), config.FileName)}
	m := &onboardModel{
		reg: newProviders("http://127.0.0.1:1", cfg),
		cat: &catalog.Catalog{Revision: "test"},
		cfg: cfg,
		th:  darkTheme(),
	}
	step(t, m, onboardChoicesMsg{
		items:   []pickerItem{{id: "x", label: "x"}},
		choices: map[string]modelChoice{"x": {ref: "ollama/x", surface: "local"}},
	})
	step(t, m, tea.KeyMsg{Type: tea.KeyEsc})
	if !m.cancelled {
		t.Fatal("escape at the picker should cancel setup")
	}
	if len(cfg.Tiers) != 0 {
		t.Fatal("a cancelled setup bound a tier anyway")
	}
}

// First launch opens the connect checklist before any model is picked, and
// its exit row hands over to the model step.
func TestOnboardingStartsWithTheConnectChecklist(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	cfg := &config.Config{Path: filepath.Join(home, config.FileName)}
	m := &onboardModel{
		reg: newProviders("http://127.0.0.1:1", cfg),
		cat: &catalog.Catalog{Revision: "test"},
		cfg: cfg,
		th:  darkTheme(),
	}

	msg, ok := m.Init()().(pickerMsg)
	if !ok {
		t.Fatalf("first launch should open the connect checklist, got %T", msg)
	}
	if !strings.Contains(msg.title, "connect") {
		t.Fatalf("unexpected first step: %q", msg.title)
	}
	var continueRow bool
	for _, it := range msg.items {
		continueRow = continueRow || (it.id == setupDoneID && it.label == "continue")
	}
	if !continueRow {
		t.Fatal("the checklist needs its handover row")
	}

	next := msg.action(setupDoneID)
	if next == nil {
		t.Fatal("continue produced nothing")
	}
	if m.step != stepModel {
		t.Fatal("continue should advance the wizard to the model step")
	}
}

// The ladder is what this tool is, so setup has to be able to build one. The
// wizard used to bind t1 and drop the user into a session, leaving every rung
// above it to a command they had not met yet.
func TestOnboardingBindsAsManyRungsAsAsked(t *testing.T) {
	cfg := &config.Config{Path: filepath.Join(t.TempDir(), config.FileName)}
	m := &onboardModel{
		reg: newProviders("http://127.0.0.1:1", cfg),
		cat: &catalog.Catalog{Revision: "test"},
		cfg: cfg,
		th:  darkTheme(),
	}

	bind := func(model string) {
		t.Helper()
		id := "ollama/" + model + " local"
		step(t, m, onboardChoicesMsg{
			items:   []pickerItem{{id: id, label: "ollama/" + model}},
			choices: map[string]modelChoice{id: {ref: "ollama/" + model, surface: "local"}},
		})
		step(t, m, tea.KeyMsg{Type: tea.KeyEnter})
	}

	bind("small")
	// Take the "add another rung" row, which is the first offered.
	step(t, m, tea.KeyMsg{Type: tea.KeyEnter})
	bind("medium")
	step(t, m, tea.KeyMsg{Type: tea.KeyEnter})
	bind("large")

	if m.quitting {
		t.Fatal("the wizard closed while rungs were still being added")
	}
	step(t, m, tea.KeyMsg{Type: tea.KeyDown})
	step(t, m, tea.KeyMsg{Type: tea.KeyEnter})
	if !m.quitting || m.cancelled || m.err != nil {
		t.Fatalf("setup did not end cleanly: quitting=%v cancelled=%v err=%v",
			m.quitting, m.cancelled, m.err)
	}

	saved, err := config.LoadFile(cfg.Path)
	if err != nil {
		t.Fatal(err)
	}
	if len(saved.Tiers) != 3 {
		t.Fatalf("the ladder has %d rungs, want the three that were bound: %v", len(saved.Tiers), saved.Tiers)
	}
	// Rungs fill from the bottom, because a session opens on t1 and climbs.
	for i, want := range []string{"small", "medium", "large"} {
		tier := saved.Tiers[i]
		if tier.ID != "t"+strconv.Itoa(i+1) || tier.Target.ModelID != want {
			t.Fatalf("rung %d is %s/%s, want t%d/%s", i, tier.ID, tier.Target.ModelID, i+1, want)
		}
	}
}

// Escape after a rung is bound means the ladder is done. The rung is already
// written, and calling that a cancelled setup would refuse to start a session
// against a configuration that exists and is valid.
func TestBackingOutAfterARungKeepsTheLadder(t *testing.T) {
	cfg := &config.Config{Path: filepath.Join(t.TempDir(), config.FileName)}
	m := &onboardModel{
		reg: newProviders("http://127.0.0.1:1", cfg),
		cat: &catalog.Catalog{Revision: "test"},
		cfg: cfg,
		th:  darkTheme(),
	}
	id := "ollama/small local"
	step(t, m, onboardChoicesMsg{
		items:   []pickerItem{{id: id, label: "ollama/small"}},
		choices: map[string]modelChoice{id: {ref: "ollama/small", surface: "local"}},
	})
	step(t, m, tea.KeyMsg{Type: tea.KeyEnter})

	step(t, m, tea.KeyMsg{Type: tea.KeyEsc})
	if m.cancelled {
		t.Fatal("backing out of the ladder question discarded a saved rung")
	}
	if !m.quitting {
		t.Fatal("backing out should end setup")
	}
	saved, err := config.LoadFile(cfg.Path)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := saved.Tier("t1"); !ok {
		t.Fatal("t1 did not survive backing out")
	}
}
