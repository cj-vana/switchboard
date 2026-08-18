package main

// /pin names the current point in the conversation so /fork can cut back to
// it by name instead of by counting turns. A pin is a record in the log and
// nothing else: it survives resume because the log does, it rides a fork
// whose prefix contains it, and it deliberately promises nothing about
// files — /undo owns those, and a pin that claimed otherwise would be
// claiming what the log cannot keep.

import (
	"fmt"
	"strconv"
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/switchboard-code/switchboard/internal/provider"
	"github.com/switchboard-code/switchboard/internal/session"
)

func cmdPin(m *tuiModel, args string) tea.Cmd {
	name := strings.TrimSpace(args)
	state := m.app.loop.Session.State()

	if name == "" {
		if len(state.Pins) == 0 {
			m.addInfo("  no pins set; /pin <name> marks this point, /fork <name> returns to it")
			return nil
		}
		var b strings.Builder
		for _, p := range state.Pins {
			b.WriteString("  " + p.Name + "  " + pinPlace(state, p) + "\n")
		}
		b.WriteString("  /fork <name> branches back to one; the session itself is never rewritten")
		m.addInfo(strings.TrimRight(b.String(), "\n"))
		return nil
	}

	if strings.ContainsAny(name, " \t") {
		return noticeCmd("error", "a pin name is one word, e.g. /pin before-refactor")
	}
	// /fork reads a number as a turn count, so a numeric pin could never be
	// reached by the command that exists to reach it.
	if _, err := strconv.Atoi(name); err == nil {
		return noticeCmd("error", "a pin name cannot be a number; /fork "+name+" already means something else")
	}
	if len(state.Messages) == 0 {
		return noticeCmd("", "nothing to pin yet; the session is empty")
	}

	_, moved := state.Pin(name)
	if _, err := m.app.loop.Session.AppendPin(name); err != nil {
		return noticeCmd("error", "the pin was not saved: "+err.Error())
	}
	if moved {
		return noticeCmd("", fmt.Sprintf("pin %q moved here; /fork %s now returns to this point", name, name))
	}
	return noticeCmd("", fmt.Sprintf("pinned %q; /fork %s branches back to this point", name, name))
}

// pinPlace says where a pin sits in terms the user counts in: whole turns
// behind the tip, because that is the unit /fork drops.
func pinPlace(state session.State, p session.Pin) string {
	behind := 0
	for _, msg := range state.Messages[min(p.Messages, len(state.Messages)):] {
		if msg.Role == provider.RoleUser {
			behind++
		}
	}
	if behind == 0 {
		return "at the tip"
	}
	if behind == 1 {
		return "1 user turn back"
	}
	return fmt.Sprintf("%d user turns back", behind)
}
