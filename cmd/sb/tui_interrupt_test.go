package main

import (
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/switchboard-code/switchboard/internal/permission"
)

// Every dialog returned before the ctrl+c case, so a modal swallowed the
// interrupt. A subagent's approval arrives unbidden and mid-errand, and a user
// who does not want to grant it had no way out of the turn that raised it.
func TestCtrlCEscapesAPermissionDialogAndAnswersNo(t *testing.T) {
	m := testModel(t)
	respond := make(chan permission.Response, 1)
	m.pendingAsk = respond
	m.dlg = newPermissionDialog(
		permission.Request{Tool: "exec", Argv: []string{"rm", "-rf", "/"}},
		permission.Outcome{Decision: permission.Ask}, respond)

	m.key(tea.KeyMsg{Type: tea.KeyCtrlC})

	if m.dlg != nil {
		t.Fatal("ctrl+c left the modal up")
	}
	// The loop is blocked on this channel; leaving without answering hangs it.
	select {
	case got := <-respond:
		if got.Approved {
			t.Fatal("escaping a prompt approved the command")
		}
	default:
		t.Fatal("ctrl+c left the waiting loop with no answer")
	}
	if m.pendingAsk != nil {
		t.Error("the answered channel was kept")
	}
}

// A key that is not the interrupt still belongs to the dialog.
func TestOtherKeysStillReachTheDialog(t *testing.T) {
	m := testModel(t)
	respond := make(chan permission.Response, 1)
	m.pendingAsk = respond
	m.dlg = newPermissionDialog(
		permission.Request{Tool: "exec", Argv: []string{"ls"}},
		permission.Outcome{Decision: permission.Ask}, respond)

	m.key(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'y'}})
	select {
	case got := <-respond:
		if !got.Approved {
			t.Fatal("y did not approve")
		}
	default:
		t.Fatal("y never reached the dialog")
	}
}
