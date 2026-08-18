package main

import (
	"path/filepath"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/switchboard-code/switchboard/internal/catalog"
	"github.com/switchboard-code/switchboard/internal/config"
)

// drain runs a command chain until it produces a terminal message, feeding
// each pickerMsg's first matching item back through its action, which is how
// a user walks the /models dialogs.
func pick(t *testing.T, cmd tea.Cmd, choose func(pickerMsg) string) tea.Msg {
	t.Helper()
	for i := 0; i < 5; i++ {
		if cmd == nil {
			t.Fatal("the flow ended with no message")
		}
		msg := cmd()
		p, ok := msg.(pickerMsg)
		if !ok {
			return msg
		}
		id := choose(p)
		cmd = p.action(id)
	}
	t.Fatal("the dialog chain never terminated")
	return nil
}

func modelsTestModel(t *testing.T) *tuiModel {
	t.Helper()
	m := testModel(t)
	m.app.config.Path = filepath.Join(t.TempDir(), config.FileName)
	m.app.providers = newProviders("http://127.0.0.1:1", m.app.config)
	cat, err := catalog.LoadBundled()
	if err != nil {
		t.Fatal(err)
	}
	m.app.catalog = cat
	return m
}

func TestModelsBindsANewRungAndSaves(t *testing.T) {
	m := modelsTestModel(t)

	var boundModel string
	msg := pick(t, cmdModels(m, ""), func(p pickerMsg) string {
		switch {
		case strings.HasPrefix(p.title, "bind a model"):
			// Bind the first catalog model; the local server is not running
			// in this test, so every item is a catalog entry.
			boundModel = p.items[0].id
			return boundModel
		case strings.HasPrefix(p.title, "which tier"):
			last := p.items[len(p.items)-1]
			if last.id != "t2" {
				t.Fatalf("the new rung on a one-rung ladder should be t2, got %q", last.id)
			}
			return last.id
		case strings.HasPrefix(p.title, "reasoning effort"):
			return "" // provider default
		default:
			t.Fatalf("unexpected picker %q", p.title)
			return ""
		}
	})

	n, ok := msg.(noticeMsg)
	if !ok || n.level == "error" {
		t.Fatalf("binding did not succeed: %#v", msg)
	}
	if len(m.app.config.Tiers) != 2 {
		t.Fatalf("the ladder has %d rungs, want 2", len(m.app.config.Tiers))
	}

	saved, err := config.LoadFile(m.app.config.Path)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := saved.Tier("t2"); !ok {
		t.Fatal("the binding was not persisted")
	}
}

func TestModelsRefusesToRemoveTheActiveRung(t *testing.T) {
	m := modelsTestModel(t)
	msg := pick(t, removeRungCmd(m), func(p pickerMsg) string {
		return m.app.tier.ID
	})
	n, ok := msg.(noticeMsg)
	if !ok || n.level != "error" {
		t.Fatalf("removing the active tier should refuse, got %#v", msg)
	}
	if len(m.app.config.Tiers) != 1 {
		t.Fatal("the active rung was removed anyway")
	}
}

func TestHighestRungSkipsGaps(t *testing.T) {
	cfg := &config.Config{}
	for _, id := range []string{"t1", "t4"} {
		if err := cfg.BindTier(id, "", "ollama/x", "", ""); err != nil {
			t.Fatal(err)
		}
	}
	if got := highestRung(cfg); got != 4 {
		t.Fatalf("highestRung = %d, want 4", got)
	}
}
