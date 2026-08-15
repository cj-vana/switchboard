package main

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/cj-vana/switchboard/internal/credential"
)

// The login flow's contract: the secret is taken masked, and no rendering of
// the dialog ever contains what was typed.
func TestSecretDialogNeverEchoesTheSecret(t *testing.T) {
	m := testModel(t)
	var stored string
	m.dlg = newSecretDialog(credential.Ref{Provider: "kimi", Account: "coding"}, "test store", func(v string) tea.Cmd {
		stored = v
		return nil
	})

	const secret = "sk-test-not-a-real-key"
	for _, r := range secret {
		m.dlg.update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}}, m.th)
	}
	if view := m.dlg.view(80, m.th); strings.Contains(view, secret) {
		t.Fatalf("the dialog rendered the secret:\n%s", view)
	}

	done, _ := m.dlg.update(tea.KeyMsg{Type: tea.KeyEnter}, m.th)
	if !done {
		t.Fatal("enter did not close the dialog")
	}
	if stored != secret {
		t.Fatalf("submit delivered %q, want the typed secret", stored)
	}
}

func TestSecretDialogEscapeStoresNothing(t *testing.T) {
	m := testModel(t)
	submitted := false
	m.dlg = newSecretDialog(credential.Ref{Provider: "kimi", Account: "coding"}, "test store", func(string) tea.Cmd {
		submitted = true
		return nil
	})
	m.dlg.update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("abc")}, m.th)
	done, _ := m.dlg.update(tea.KeyMsg{Type: tea.KeyEsc}, m.th)
	if !done {
		t.Fatal("escape did not close the dialog")
	}
	if submitted {
		t.Fatal("escape submitted the secret anyway")
	}
}

// /login with no argument resolves each reference's standing off the UI
// goroutine and comes back as a picker; the ladder's own targets lead it.
func TestLoginBuildsPickerFromLadderAndCatalog(t *testing.T) {
	m := testModel(t)
	cmd := cmdLogin(m, "")
	if cmd == nil {
		t.Fatal("/login produced no command")
	}
	msg, ok := cmd().(authItemsMsg)
	if !ok {
		t.Fatalf("expected authItemsMsg, got %T", cmd())
	}
	if len(msg.items) == 0 {
		t.Fatal("the picker has nothing to offer")
	}
	if msg.items[0].id != "ollama/local" {
		t.Errorf("the ladder's own reference should lead, got %q", msg.items[0].id)
	}
	if msg.items[0].desc != "local server, no key needed" {
		t.Errorf("ollama's standing should say no key is needed, got %q", msg.items[0].desc)
	}

	m.Update(msg)
	if m.dlg == nil {
		t.Fatal("authItemsMsg did not open the picker")
	}
}

func TestLoginWithArgumentGoesStraightToThePrompt(t *testing.T) {
	m := testModel(t)
	cmd := cmdLogin(m, "kimi/coding")
	if cmd == nil {
		t.Fatal("/login kimi/coding produced no command")
	}
	msg := cmd()
	prompt, ok := msg.(secretPromptMsg)
	if !ok {
		// On a platform with no writable OS store the command degrades to a
		// notice naming the environment variable; that is also correct.
		if n, isNotice := msg.(noticeMsg); isNotice && strings.Contains(n.text, "SB_") {
			return
		}
		t.Fatalf("expected secretPromptMsg or an env-var notice, got %T", msg)
	}
	if prompt.ref.Provider != "kimi" || prompt.ref.Account != "coding" {
		t.Fatalf("prompt is for %+v, want kimi/coding", prompt.ref)
	}

	m.Update(msg)
	if m.dlg == nil {
		t.Fatal("secretPromptMsg did not open the masked input")
	}
}
