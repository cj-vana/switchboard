package main

import (
	"slices"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/cj-vana/switchboard/internal/permission"
)

// --- slash-command suggestions ----------------------------------------------

func (m *tuiModel) suggestions() []commandItem {
	v := m.ta.Value()
	if !strings.HasPrefix(v, "/") || strings.ContainsAny(v, " \n\t") {
		return nil
	}
	prefix := strings.TrimPrefix(v, "/")
	out := matchingCommands(prefix, m.app.config)
	for _, c := range m.custom {
		if strings.HasPrefix(c.name, prefix) {
			out = append(out, commandItem{name: c.name, desc: c.desc})
		}
	}
	return out
}

func (m *tuiModel) suggestionsVisible() bool {
	return !m.busy && m.dlg == nil && !m.sugClosed && len(m.suggestions()) > 0
}

func (m *tuiModel) suggestionsView() string {
	items := m.suggestions()
	if !m.suggestionsVisible() || len(items) == 0 {
		return ""
	}
	const maxRows = 6
	if m.sugSel >= len(items) {
		m.sugSel = len(items) - 1
	}
	shown := items
	start := 0
	if len(items) > maxRows {
		start = m.sugSel - maxRows + 1
		if start < 0 {
			start = 0
		}
		shown = items[start : start+maxRows]
		if len(shown) > maxRows {
			shown = shown[:maxRows]
		}
	}

	width := 0
	for _, it := range items {
		if n := len(it.name) + len(it.usage) + 1; n > width {
			width = n
		}
	}

	// The selected row is one object: the highlight runs the row's full
	// width, name and description together, the way every picker in the
	// terminal-tool generation the user's hands know behaves.
	var rows []string
	for i, it := range shown {
		name := "/" + it.name
		if it.usage != "" {
			name += " " + it.usage
		}
		if start+i == m.sugSel {
			on := func(s lipgloss.Style) lipgloss.Style { return s.Background(m.th.selected.GetBackground()) }
			rows = append(rows, m.th.accent.Render("▌")+
				on(m.th.bold).Render(padRight(name, width+2))+
				on(m.th.dim).Render(padRight(it.desc, max(m.width-width-5, 0))))
			continue
		}
		rows = append(rows, " "+padRight(name, width+2)+m.th.dim.Render(it.desc))
	}
	return lipgloss.JoinVertical(lipgloss.Left, rows...)
}

func padRight(s string, n int) string {
	if len(s) >= n {
		return s
	}
	return s + strings.Repeat(" ", n-len(s))
}

func (m *tuiModel) acceptSuggestion() {
	items := m.suggestions()
	if len(items) == 0 {
		return
	}
	if m.sugSel >= len(items) {
		m.sugSel = 0
	}
	m.ta.SetValue("/" + items[m.sugSel].name + " ")
	m.ta.CursorEnd()
	m.sugSel = 0
}

// exactCommand reports whether the input is exactly a command name, so enter
// runs it rather than completing it.
func (m *tuiModel) exactCommand() bool {
	v := strings.TrimPrefix(m.ta.Value(), "/")
	for _, it := range m.suggestions() {
		if it.name == v || slices.Contains(it.aliases, v) {
			return true
		}
	}
	return false
}

// --- history -----------------------------------------------------------------

func (m *tuiModel) historyMove(delta int) {
	if len(m.history) == 0 {
		return
	}
	m.histIdx += delta
	if m.histIdx < 0 {
		m.histIdx = 0
		return
	}
	if m.histIdx >= len(m.history) {
		m.histIdx = len(m.history)
		m.ta.SetValue("")
	} else {
		m.ta.SetValue(m.history[m.histIdx])
	}
	m.ta.CursorEnd()
	m.growInput()
}

// growInput keeps the prompt between one and six rows.
func (m *tuiModel) growInput() {
	lines := strings.Count(m.ta.Value(), "\n") + 1
	if lines > 6 {
		lines = 6
	}
	m.ta.SetHeight(lines)
}

// --- slash dispatch -----------------------------------------------------------

// runSlash handles a /command. While a turn runs, commands that would touch
// the session are refused rather than racing it; /exit still works.
func (m *tuiModel) runSlash(v string) tea.Cmd {
	name, rest, _ := strings.Cut(strings.TrimPrefix(v, "/"), " ")
	rest = strings.TrimSpace(rest)

	// A bare tier name switches to it; with a prompt attached it runs just this
	// prompt there, which is §14's command-prefix override.
	if _, ok := m.app.config.Tier(name); ok {
		if m.busy {
			return noticeCmd("warn", "a turn is running; esc to interrupt it first")
		}
		if rest == "" {
			return m.app.switchTier(name)
		}
		return m.enqueue(rest, name)
	}

	for _, c := range commands() {
		if c.name == name || slices.Contains(c.aliases, name) {
			if m.busy && !c.busySafe {
				return noticeCmd("warn", "a turn is running; esc to interrupt it first")
			}
			return c.run(m, rest)
		}
	}
	for _, c := range m.custom {
		if c.name == name {
			return runCustom(m, c, rest)
		}
	}
	return noticeCmd("error", "unknown command "+name+"; try /help")
}

// cycleMode is shift+tab: default → acceptEdits → bypass → plan → default.
func (m *tuiModel) cycleMode() tea.Cmd {
	order := []permission.Mode{
		permission.ModeDefault,
		permission.ModeAcceptEdits,
		permission.ModeBypass,
		permission.ModePlan,
	}
	i := slices.Index(order, m.mode)
	next := order[(i+1)%len(order)]
	return m.setMode(next)
}

func (m *tuiModel) setMode(mode permission.Mode) tea.Cmd {
	m.app.loop.Perms.SetMode(mode)
	m.mode = mode
	m.addInfo("mode is now " + string(mode))
	if mode == permission.ModeBypass && !m.app.capability.AutomaticExecutionAllowed() {
		// Saying this once, plainly, beats letting the user discover it by
		// being prompted anyway and reading it as a bug (§19.3).
		m.addNotice("warn", "commands will still be approved one at a time: "+m.app.capability.Summary())
	}
	return nil
}

func (m *tuiModel) openTierPicker() tea.Cmd {
	if len(m.app.config.Tiers) == 0 {
		return noticeCmd("", "no tiers configured in "+m.app.config.Path)
	}
	var items []pickerItem
	for _, t := range m.app.config.Tiers {
		desc := string(t.Target.ID())
		if t.Label != "" {
			desc = t.Label + "  " + desc
		}
		items = append(items, pickerItem{
			id:      t.ID,
			label:   t.ID,
			desc:    desc,
			current: t.ID == m.app.tier.ID,
		})
	}
	m.dlg = &pickerDialog{
		title:  "switch tier",
		items:  items,
		onPick: func(id string) tea.Cmd { return m.app.switchTier(id) },
	}
	return nil
}
