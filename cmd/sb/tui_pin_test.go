package main

import (
	"strings"
	"testing"

	"github.com/switchboard-code/switchboard/internal/provider"
)

func TestPinCommandNamesThePointAndListsIt(t *testing.T) {
	m := testModel(t)
	sess := m.app.loop.Session
	if err := sess.AppendMessage(provider.UserText("first question")); err != nil {
		t.Fatal(err)
	}
	if err := sess.AppendMessage(provider.Message{Role: provider.RoleAssistant,
		Content: []provider.Block{provider.Text{Text: "first answer"}}}); err != nil {
		t.Fatal(err)
	}

	if cmd := cmdPin(m, "before-refactor"); cmd == nil {
		t.Fatal("setting a pin said nothing")
	}
	pin, ok := m.app.loop.Session.State().Pin("before-refactor")
	if !ok || pin.Messages != 2 {
		t.Fatalf("the pin did not land at the tip: %+v ok=%v", pin, ok)
	}

	before := len(m.tr.entries)
	cmdPin(m, "")
	if len(m.tr.entries) == before {
		t.Fatal("listing rendered nothing")
	}
	listing := m.tr.entries[len(m.tr.entries)-1].text
	if !strings.Contains(listing, "before-refactor") || !strings.Contains(listing, "at the tip") {
		t.Errorf("the listing does not place the pin: %q", listing)
	}
}

func TestPinRefusesNamesForkCouldNotReach(t *testing.T) {
	m := testModel(t)
	if err := m.app.loop.Session.AppendMessage(provider.UserText("q")); err != nil {
		t.Fatal(err)
	}

	// A number is /fork's turn count; a two-word name is unaddressable.
	cmdPin(m, "3")
	cmdPin(m, "two words")
	if len(m.app.loop.Session.State().Pins) != 0 {
		t.Fatal("an unreachable pin name was accepted")
	}
}

func TestPinOnAnEmptySessionIsRefused(t *testing.T) {
	m := testModel(t)
	cmdPin(m, "start")
	if len(m.app.loop.Session.State().Pins) != 0 {
		t.Fatal("pinned an empty session")
	}
}

func TestForkByUnknownPinSaysSo(t *testing.T) {
	m := testModel(t)
	if err := m.app.loop.Session.AppendMessage(provider.UserText("q")); err != nil {
		t.Fatal(err)
	}
	cmd := cmdFork(m, "nowhere")
	if cmd == nil {
		t.Fatal("an unknown pin returned nothing")
	}
	msg := cmd()
	notice, ok := msg.(noticeMsg)
	if !ok || !strings.Contains(notice.text, "no pin named nowhere") {
		t.Fatalf("the error does not name the missing pin: %+v", msg)
	}
}
