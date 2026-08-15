package main

import "github.com/charmbracelet/lipgloss"

// theme holds the chrome the TUI draws with. Two themes ship, dark and light;
// markdown has its own per-theme style because glamour renders whole documents
// rather than fragments.
type theme struct {
	name string
	dark bool

	text     lipgloss.Style
	dim      lipgloss.Style
	faint    lipgloss.Style
	bold     lipgloss.Style
	accent   lipgloss.Style
	user     lipgloss.Style
	ok       lipgloss.Style
	warn     lipgloss.Style
	err      lipgloss.Style
	thinking lipgloss.Style

	// Chips are the status-line segments with a filled background.
	tierChip lipgloss.Style
	modeChip map[string]lipgloss.Style

	// Bar colors for the context gauge.
	barFill  lipgloss.Style
	barEmpty lipgloss.Style

	selected lipgloss.Style
	border   lipgloss.Style

	// rungs is the heat ramp: the ladder rendered as temperature. t1 sits at
	// cool teal and each rung above it runs warmer, ending at amber, so a
	// glance at any routing surface says where on the ladder work is running
	// and escalation literally heats up. This is the visual identity, and it
	// is one no neighboring tool can wear: nothing else routes.
	rungs     []lipgloss.Style
	rungChips []lipgloss.Style
}

// rung returns the heat style for a ladder rank, clamped: a ladder deeper
// than the ramp reuses the hottest color rather than inventing a new one.
func (t *theme) rung(rank int) lipgloss.Style {
	if rank < 0 {
		rank = 0
	}
	if rank >= len(t.rungs) {
		rank = len(t.rungs) - 1
	}
	return t.rungs[rank]
}

func (t *theme) rungChip(rank int) lipgloss.Style {
	if rank < 0 {
		rank = 0
	}
	if rank >= len(t.rungChips) {
		rank = len(t.rungChips) - 1
	}
	return t.rungChips[rank]
}

func darkTheme() *theme {
	t := &theme{name: "dark", dark: true}
	t.text = lipgloss.NewStyle().Foreground(lipgloss.Color("252"))
	t.dim = lipgloss.NewStyle().Foreground(lipgloss.Color("244"))
	t.faint = lipgloss.NewStyle().Foreground(lipgloss.Color("238"))
	t.bold = lipgloss.NewStyle().Bold(true)
	t.accent = lipgloss.NewStyle().Foreground(lipgloss.Color("39"))
	t.user = lipgloss.NewStyle().Foreground(lipgloss.Color("229")).Bold(true)
	t.ok = lipgloss.NewStyle().Foreground(lipgloss.Color("42"))
	t.warn = lipgloss.NewStyle().Foreground(lipgloss.Color("214"))
	t.err = lipgloss.NewStyle().Foreground(lipgloss.Color("196"))
	t.thinking = lipgloss.NewStyle().Foreground(lipgloss.Color("243")).Italic(true)

	t.tierChip = lipgloss.NewStyle().Foreground(lipgloss.Color("235")).Background(lipgloss.Color("39")).Bold(true)
	t.modeChip = map[string]lipgloss.Style{
		"plan":        lipgloss.NewStyle().Foreground(lipgloss.Color("235")).Background(lipgloss.Color("75")),
		"default":     lipgloss.NewStyle().Foreground(lipgloss.Color("252")).Background(lipgloss.Color("238")),
		"acceptEdits": lipgloss.NewStyle().Foreground(lipgloss.Color("235")).Background(lipgloss.Color("42")),
		"bypass":      lipgloss.NewStyle().Foreground(lipgloss.Color("235")).Background(lipgloss.Color("214")),
	}
	t.barFill = lipgloss.NewStyle().Foreground(lipgloss.Color("39"))
	t.barEmpty = lipgloss.NewStyle().Foreground(lipgloss.Color("236"))
	t.selected = lipgloss.NewStyle().Background(lipgloss.Color("237"))
	t.border = lipgloss.NewStyle().Foreground(lipgloss.Color("238"))

	// Cool to warm, muted rather than neon: teal, steel blue, violet, copper,
	// amber, coral. Mid-brightness values chosen to read on a dark ground.
	for _, c := range []string{"73", "68", "134", "173", "179", "203"} {
		t.rungs = append(t.rungs, lipgloss.NewStyle().Foreground(lipgloss.Color(c)))
		t.rungChips = append(t.rungChips, lipgloss.NewStyle().
			Foreground(lipgloss.Color("235")).Background(lipgloss.Color(c)).Bold(true))
	}
	return t
}

func lightTheme() *theme {
	t := &theme{name: "light", dark: false}
	t.text = lipgloss.NewStyle().Foreground(lipgloss.Color("235"))
	t.dim = lipgloss.NewStyle().Foreground(lipgloss.Color("242"))
	t.faint = lipgloss.NewStyle().Foreground(lipgloss.Color("250"))
	t.bold = lipgloss.NewStyle().Bold(true)
	t.accent = lipgloss.NewStyle().Foreground(lipgloss.Color("27"))
	t.user = lipgloss.NewStyle().Foreground(lipgloss.Color("94")).Bold(true)
	t.ok = lipgloss.NewStyle().Foreground(lipgloss.Color("28"))
	t.warn = lipgloss.NewStyle().Foreground(lipgloss.Color("166"))
	t.err = lipgloss.NewStyle().Foreground(lipgloss.Color("160"))
	t.thinking = lipgloss.NewStyle().Foreground(lipgloss.Color("245")).Italic(true)

	t.tierChip = lipgloss.NewStyle().Foreground(lipgloss.Color("255")).Background(lipgloss.Color("27")).Bold(true)
	t.modeChip = map[string]lipgloss.Style{
		"plan":        lipgloss.NewStyle().Foreground(lipgloss.Color("255")).Background(lipgloss.Color("61")),
		"default":     lipgloss.NewStyle().Foreground(lipgloss.Color("235")).Background(lipgloss.Color("252")),
		"acceptEdits": lipgloss.NewStyle().Foreground(lipgloss.Color("255")).Background(lipgloss.Color("28")),
		"bypass":      lipgloss.NewStyle().Foreground(lipgloss.Color("255")).Background(lipgloss.Color("166")),
	}
	t.barFill = lipgloss.NewStyle().Foreground(lipgloss.Color("27"))
	t.barEmpty = lipgloss.NewStyle().Foreground(lipgloss.Color("254"))
	t.selected = lipgloss.NewStyle().Background(lipgloss.Color("254"))
	t.border = lipgloss.NewStyle().Foreground(lipgloss.Color("250"))

	// The same ramp, darkened to hold contrast on a light ground.
	for _, c := range []string{"30", "25", "91", "130", "136", "161"} {
		t.rungs = append(t.rungs, lipgloss.NewStyle().Foreground(lipgloss.Color(c)))
		t.rungChips = append(t.rungChips, lipgloss.NewStyle().
			Foreground(lipgloss.Color("255")).Background(lipgloss.Color(c)).Bold(true))
	}
	return t
}
