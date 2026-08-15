package main

import (
	"path/filepath"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/cj-vana/switchboard/internal/catalog"
	"github.com/cj-vana/switchboard/internal/config"
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

	if !m.quitting {
		t.Fatal("a local model needs no key and no effort choice; the wizard should have finished")
	}
	if m.cancelled || m.err != nil {
		t.Fatalf("wizard failed: cancelled=%v err=%v", m.cancelled, m.err)
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
