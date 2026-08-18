package main

// The affordances every neighboring tool has and a user's hands expect:
// /init, /export, /context, the command palette, and the external editor.
// Each is small; their absence is what reads as unfinished.

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/switchboard-code/switchboard/internal/prefix"
	"github.com/switchboard-code/switchboard/internal/provider"
	"github.com/switchboard-code/switchboard/internal/session"
)

// initPrompt is /init: a turn like any other, using the loop and the tools it
// already has, because a canned prompt the user can read beats a bespoke
// generator they cannot.
const initPrompt = `Explore this repository and write an AGENTS.md at its root for coding agents working here. Keep it under 100 lines. Cover: what the project is in two sentences; the layout (which directories hold what, only the ones that matter); how to build, test, and lint, with exact commands; conventions an agent must follow that it could not guess from the code; and anything that looks like it would bite a newcomer. If AGENTS.md already exists, read it first and revise rather than replace. Do not pad: a rule that is obvious from reading any file does not need writing down.`

func cmdInit(m *tuiModel, _ string) tea.Cmd {
	return m.enqueue(initPrompt, "")
}

// cmdExport writes the conversation as markdown, through the same
// renderer sb export uses on any recorded session. The session state is
// the source, not the transcript: the transcript is a rendering, the
// session is the record.
func cmdExport(m *tuiModel, args string) tea.Cmd {
	state := m.app.loop.Session.State()
	name := strings.TrimSpace(args)
	if name == "" {
		name = "sb-session-" + state.ID + ".md"
	}
	path := name
	if !filepath.IsAbs(path) {
		path = filepath.Join(m.app.workspace, name)
	}

	// A log that cannot be read as a timeline degrades to the messages
	// alone rather than failing the export.
	timeline, terr := session.ReadTimeline(m.app.loop.Session.Path())
	if terr != nil {
		timeline = nil
	}
	if err := os.WriteFile(path, []byte(exportMarkdown(state, timeline)), 0o644); err != nil {
		return noticeCmd("error", "export failed: "+err.Error())
	}
	return noticeCmd("", "exported to "+path)
}

// cmdContext shows where the window is going. The constraint is invisible
// until it is fatal; a bar makes it something the user can see coming.
func cmdContext(m *tuiModel, _ string) tea.Cmd {
	state := m.app.loop.Session.State()
	used := m.callTokens
	window := m.ctxWindow

	var b strings.Builder
	if window > 0 && used > 0 {
		pct := used * 100 / window
		filled := used * 30 / window
		if filled > 30 {
			filled = 30
		}
		fmt.Fprintf(&b, "context  [%s%s] %d%%  %s of %s tokens\n",
			strings.Repeat("█", filled), strings.Repeat("░", 30-filled), pct, compact(used), compact(window))
	} else if window > 0 {
		fmt.Fprintf(&b, "context window %s; usage is measured on the first turn\n", compact(window))
	} else {
		b.WriteString("this target does not report a context window\n")
	}

	// The window's composition, in the estimator's own terms: what the next
	// request would send, split by zone. System and tools are the frozen
	// zone a provider cache holds; the conversation is what grows. The
	// split is chars-over-four (the measured floor in docs/estimator.md),
	// while the meter above is what the provider last reported, so the two
	// are stated separately rather than pretending to reconcile.
	sys := prefix.RequestTokens(provider.Request{System: m.app.loop.System})
	tools := prefix.RequestTokens(provider.Request{Tools: m.app.loop.Tools.Definitions()})
	conv := prefix.RequestTokens(provider.Request{Messages: state.Messages})
	if sys+tools+conv > 0 {
		fmt.Fprintf(&b, "the next request, estimated: system %s · tools %s · conversation %s · ~%s total\n",
			compact(sys), compact(tools), compact(conv), compact(sys+tools+conv))
	}
	// The meter's consequence sits beside the meter: a reading at 78% means
	// something different when the tripwire at 85% is in the same glance.
	if m.app.config.CompactAuto {
		fmt.Fprintf(&b, "auto-compact fires at %d%%; /compact preview states the trade, /compact auto off disarms it\n",
			compactThreshold(m.app.config))
	}
	fmt.Fprintf(&b, "messages %d · tool calls %d · session ↓%s ↑%s tokens",
		len(state.Messages), state.Calls, compact(state.Usage.InputTokens), compact(state.Usage.OutputTokens))
	m.addInfo(b.String())
	return nil
}

// openPalette is ctrl+p: every command in one searchable, fuzzy-ranked picker.
// It runs the bare command; one that needs arguments opens its own picker or
// says so.
func (m *tuiModel) openPalette() tea.Cmd {
	var items []pickerItem
	for _, c := range commands() {
		items = append(items, pickerItem{id: c.name, label: "/" + c.name, desc: c.desc})
	}
	for _, t := range m.app.config.Tiers {
		items = append(items, pickerItem{id: t.ID, label: "/" + t.ID, desc: "switch to " + t.ID, current: t.ID == m.app.tier.ID})
	}
	m.dlg = &pickerDialog{
		title:  "commands",
		items:  items,
		onPick: func(id string) tea.Cmd { return m.runSlash("/" + id) },
	}
	return nil
}

// --- external editor ---------------------------------------------------------

type editorDoneMsg struct {
	path string
	err  error
}

// openEditor is ctrl+g: the prompt in $VISUAL or $EDITOR. Bubble Tea suspends
// the TUI for the child process and resumes when it exits, which is the whole
// trick; everything else is a temp file.
func (m *tuiModel) openEditor() tea.Cmd {
	editor := os.Getenv("VISUAL")
	if editor == "" {
		editor = os.Getenv("EDITOR")
	}
	if editor == "" {
		return noticeCmd("error", "set $EDITOR or $VISUAL to use the external editor")
	}

	tmp, err := os.CreateTemp("", "sb-prompt-*.md")
	if err != nil {
		return noticeCmd("error", err.Error())
	}
	if _, err := tmp.WriteString(m.ta.Value()); err != nil {
		tmp.Close()
		return noticeCmd("error", err.Error())
	}
	tmp.Close()

	parts := strings.Fields(editor) // "code --wait" is a legitimate $EDITOR
	cmd := sanitizedCommand(parts[0], append(parts[1:], tmp.Name())...)
	return tea.ExecProcess(cmd, func(err error) tea.Msg {
		return editorDoneMsg{path: tmp.Name(), err: err}
	})
}

func (m *tuiModel) onEditorDone(msg editorDoneMsg) {
	defer os.Remove(msg.path)
	if msg.err != nil {
		m.addNotice("error", "editor: "+msg.err.Error())
		return
	}
	data, err := os.ReadFile(msg.path)
	if err != nil {
		m.addNotice("error", "reading the edited prompt: "+err.Error())
		return
	}
	m.ta.SetValue(strings.TrimRight(string(data), "\n"))
	m.ta.CursorEnd()
	m.growInput()
}
