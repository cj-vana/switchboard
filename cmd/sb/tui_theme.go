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
	return t
}
