package main

// /steer and ctrl+s: the user's own words into a turn already in flight.
//
// The queue answers "run this after"; steering answers "read this now". The
// loop's round boundary is the only seam where a user message is legal in
// every wire format, so a steer waits there at most — it never rewrites what
// was sent, and it never lands under an in-flight round. What misses the turn
// entirely is not dropped: at turn end it leads the prompt queue, because it
// was typed before anything queued behind it.

import (
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/switchboard-code/switchboard/internal/credential"
	"github.com/switchboard-code/switchboard/internal/provider"
)

func (a *tuiApp) queueSteer(text string) {
	a.steerMu.Lock()
	defer a.steerMu.Unlock()
	a.steers = append(a.steers, text)
}

func (a *tuiApp) takeSteers() []string {
	a.steerMu.Lock()
	defer a.steerMu.Unlock()
	out := a.steers
	a.steers = nil
	return out
}

// steerRound is the user's slice of the injection seam, first in the
// composition: between two rounds the user's words outrank every machine
// note. The [steer] lead is the marking injected text already carries, so
// /retry's opening detection reads one as what it is — something that rode in
// mid-turn, never a turn's opening.
func (a *tuiApp) steerRound() []provider.Message {
	steers := a.takeSteers()
	out := make([]provider.Message, 0, len(steers))
	for _, text := range steers {
		out = append(out, provider.UserText("[steer] "+text))
	}
	return out
}

// steerKey is ctrl+s. With a turn running, the composed text steers it; at a
// quiet prompt the key is an ordinary send, because steering nothing is just
// typing.
func (m *tuiModel) steerKey() tea.Cmd {
	if !m.busy && !m.turnPlanning && !m.operationActive {
		return m.submit()
	}
	text := strings.TrimSpace(m.ta.Value())
	if text == "" {
		return nil
	}
	if m.race != nil {
		// Race arms run their own loops with no injection seam by design;
		// accepting the text would promise a delivery that does not exist.
		return noticeCmd("warn", "a race's arms cannot hear you; the session that continues can be steered after the verdict")
	}
	m.ta.Reset()
	m.growInput()
	m.sugClosed = false
	m.sugSel = 0
	// Same history as a send: a correction recalled with up-arrow is a
	// correction you can steer again without retyping.
	if len(m.history) == 0 || m.history[len(m.history)-1] != text {
		m.history = append(m.history, text)
		appendHistory(m.app.workspace, text)
	}
	m.histIdx = len(m.history)
	if m.operationActive {
		// An operation is not a turn: no round boundary is coming. The words
		// are a prompt, so they queue as one.
		m.queue = append(m.queue, text)
		m.addNotice("", "queued; it runs when the current operation finishes")
		return nil
	}
	return m.steer(text)
}

// steer hands text to the running turn. Mentions expand the way they would
// for a send, and the secret gate is the same one a plain turn passes,
// because the destination is the same provider. A picture cannot ride a
// round boundary — the injection seam carries text — so one is named rather
// than silently dropped.
func (m *tuiModel) steer(text string) tea.Cmd {
	expanded, images := m.expandMentions(text)
	send := func(p string) tea.Cmd {
		m.app.queueSteer(p)
		m.addUser(text)
		note := "steers the running turn; it lands at the next round boundary"
		if len(images) > 0 {
			note = "steers the running turn as text at the next round boundary; the attached image(s) cannot ride it"
		}
		m.addNotice("", note)
		return nil
	}
	if leaks := credential.ScanPrompt(expanded); len(leaks) > 0 {
		return m.openSecretGate(leaks, expanded, send)
	}
	return send(expanded)
}

func cmdSteer(m *tuiModel, args string) tea.Cmd {
	text := strings.TrimSpace(args)
	if text == "" {
		return noticeCmd("", "/steer <text>, or type and press ctrl+s, sends your words into the running turn at its next round boundary; with nothing running they start a turn; /tasks steer <id> <text> steers a delegate")
	}
	if m.race != nil {
		return noticeCmd("warn", "a race's arms cannot hear you; the session that continues can be steered after the verdict")
	}
	if m.busy || m.turnPlanning {
		return m.steer(text)
	}
	return m.startTurn(text, "")
}
