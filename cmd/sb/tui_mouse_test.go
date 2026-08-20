package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/switchboard-code/switchboard/internal/config"
)

// notices runs a command and collects the notice text it produced, flattening
// one level of batch. A mouse change is a batch: the terminal has to be told
// as well as the user.
func notices(t *testing.T, cmd tea.Cmd) []string {
	t.Helper()
	if cmd == nil {
		return nil
	}
	var out []string
	switch msg := cmd().(type) {
	case noticeMsg:
		out = append(out, msg.text)
	case tea.BatchMsg:
		for _, sub := range msg {
			out = append(out, notices(t, sub)...)
		}
	}
	return out
}

// The mouse is off unless it was asked for, because a terminal reporting mouse
// events to a program will not select text with them, and copying a command
// out of a transcript has no keyboard substitute the way scrolling does.
func TestMouseIsOffUntilAskedFor(t *testing.T) {
	m := testModel(t)
	if m.app.config.Mouse {
		t.Fatal("a config nobody set has the mouse on")
	}
	said := strings.Join(notices(t, cmdMouse(m, "")), " ")
	if !strings.Contains(said, "mouse is off") {
		t.Errorf("bare /mouse said %q, want it to report off", said)
	}
	// The state worth naming is the consequence, not the setting: someone
	// reading this is here because they tried to select text.
	if !strings.Contains(said, "select") {
		t.Errorf("bare /mouse said %q, without saying selection is the terminal's", said)
	}
}

func TestMouseOnAndOffPersist(t *testing.T) {
	m := testModel(t)
	m.app.config.Path = filepath.Join(t.TempDir(), config.FileName)

	if said := strings.Join(notices(t, cmdMouse(m, "on")), " "); !strings.Contains(said, "mouse is on") {
		t.Errorf("/mouse on said %q", said)
	}
	if !m.app.config.Mouse {
		t.Error("/mouse on did not turn it on")
	}
	written, err := os.ReadFile(m.app.config.Path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(written), "mouse = true") {
		t.Errorf("the setting did not reach the file:\n%s", written)
	}

	if said := strings.Join(notices(t, cmdMouse(m, "off")), " "); !strings.Contains(said, "mouse is off") {
		t.Errorf("/mouse off said %q", said)
	}
	if m.app.config.Mouse {
		t.Error("/mouse off did not turn it off")
	}
	// Off is the default, so the file carries nothing rather than carrying
	// the default written down, the way the settings beside it behave.
	written, err = os.ReadFile(m.app.config.Path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(written), "mouse") {
		t.Errorf("the default was written to the file:\n%s", written)
	}
}

// A mouse change has to reach the running terminal, not just the file: the
// mode is set on the program, so a setting saved without telling it would take
// effect only at the next launch.
func TestMouseChangeTellsTheTerminalToo(t *testing.T) {
	m := testModel(t)
	m.app.config.Path = filepath.Join(t.TempDir(), config.FileName)

	batch, ok := cmdMouse(m, "on")().(tea.BatchMsg)
	if !ok {
		t.Fatal("/mouse on returned no batch, so the terminal was never told")
	}
	if len(batch) != 2 {
		t.Errorf("/mouse on batched %d commands, want the mode change and the notice", len(batch))
	}
}

func TestMouseRejectsAnythingElse(t *testing.T) {
	m := testModel(t)
	said := strings.Join(notices(t, cmdMouse(m, "wheel")), " ")
	if !strings.Contains(said, "on or off") {
		t.Errorf("/mouse wheel said %q, want the two values it takes", said)
	}
}
