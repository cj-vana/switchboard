package main

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

func pickerType(d *pickerDialog, text string) {
	d.update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(text)}, darkTheme())
}

func pickerIDs(matches []pickerMatch) []string {
	ids := make([]string, len(matches))
	for i := range matches {
		ids[i] = matches[i].item.id
	}
	return ids
}

func TestPickerFiltersAndFuzzyRanksEveryField(t *testing.T) {
	d := &pickerDialog{items: []pickerItem{
		{id: "archive", label: "Archive", desc: "store the current session"},
		{id: "diff", label: "/diff", desc: "review uncommitted changes"},
		{id: "inspect", label: "Compare", desc: "show different revisions"},
		{id: "session-export", label: "Save transcript", desc: "write markdown"},
	}}

	for _, test := range []struct {
		name  string
		query string
		want  []string
	}{
		{name: "id exact before description", query: "diff", want: []string{"diff", "inspect"}},
		{name: "label", query: "transcript", want: []string{"session-export"}},
		{name: "description", query: "markdown", want: []string{"session-export"}},
		{name: "fuzzy subsequence", query: "dff", want: []string{"diff", "inspect"}},
		{name: "case insensitive", query: "ARCH", want: []string{"archive"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			d.query = test.query
			got := pickerIDs(d.matches())
			if strings.Join(got, ",") != strings.Join(test.want, ",") {
				t.Fatalf("matches for %q = %v, want %v", test.query, got, test.want)
			}
		})
	}
}

func TestPickerEditingSelectionAndEmptyState(t *testing.T) {
	picked := ""
	d := &pickerDialog{
		title: "test picker",
		items: []pickerItem{
			{id: "alpha", label: "Alpha", desc: "first option"},
			{id: "beta", label: "Beta", desc: "second option"},
			{id: "gamma", label: "Gamma", desc: "third option"},
		},
		sel: 1,
		onPick: func(id string) tea.Cmd {
			picked = id
			return nil
		},
	}

	// Filtering keeps the same logical item selected when it still matches,
	// even when ranking moves it to a different visible row.
	pickerType(d, "second")
	if d.query != "second" || d.sel != 0 || d.matches()[d.sel].item.id != "beta" {
		t.Fatalf("filter did not preserve beta: query=%q sel=%d matches=%v", d.query, d.sel, pickerIDs(d.matches()))
	}
	d.update(tea.KeyMsg{Type: tea.KeyBackspace}, darkTheme())
	if d.query != "secon" || d.matches()[d.sel].item.id != "beta" {
		t.Fatalf("backspace lost the selected item: query=%q matches=%v", d.query, pickerIDs(d.matches()))
	}
	d.update(tea.KeyMsg{Type: tea.KeyCtrlU}, darkTheme())
	if d.query != "" || d.sel != 1 {
		t.Fatalf("ctrl+u should clear and restore beta's original row: query=%q sel=%d", d.query, d.sel)
	}

	// Arrow navigation remains the unfiltered picker's behavior when there is
	// no query.
	d.update(tea.KeyMsg{Type: tea.KeyDown}, darkTheme())
	if d.sel != 2 {
		t.Fatalf("down selection = %d, want 2", d.sel)
	}
	d.update(tea.KeyMsg{Type: tea.KeyDown}, darkTheme())
	if d.sel != 2 {
		t.Fatalf("selection escaped lower bound: %d", d.sel)
	}
	d.update(tea.KeyMsg{Type: tea.KeyUp}, darkTheme())
	if d.sel != 1 {
		t.Fatalf("up selection = %d, want 1", d.sel)
	}
	d.sel = 99
	if done, _ := d.update(tea.KeyMsg{Type: tea.KeyEnter}, darkTheme()); !done || picked != "gamma" {
		t.Fatalf("clamped enter = (done %v, picked %q), want (true, gamma)", done, picked)
	}
	picked = ""

	pickerType(d, "no such command")
	if got := d.view(80, darkTheme()); !strings.Contains(got, "no matches") || !strings.Contains(got, "no such command") {
		t.Fatalf("empty picker does not explain its query:\n%s", got)
	}
	if done, _ := d.update(tea.KeyMsg{Type: tea.KeyEnter}, darkTheme()); done {
		t.Fatal("enter closed a picker with no visible result")
	}
	if picked != "" {
		t.Fatalf("empty picker selected %q", picked)
	}
}

func TestPickerWordEditingAndQueryBound(t *testing.T) {
	d := &pickerDialog{items: []pickerItem{{id: "one", label: "one"}}}
	pickerType(d, "alpha beta")
	d.update(tea.KeyMsg{Type: tea.KeyCtrlW}, darkTheme())
	if d.query != "alpha " {
		t.Fatalf("ctrl+w query = %q, want %q", d.query, "alpha ")
	}
	d.update(tea.KeyMsg{Type: tea.KeyCtrlW}, darkTheme())
	if d.query != "" {
		t.Fatalf("second ctrl+w query = %q, want empty", d.query)
	}

	pickerType(d, strings.Repeat("x", pickerQueryMaxRunes+20))
	if got := len([]rune(d.query)); got != pickerQueryMaxRunes {
		t.Fatalf("bounded query length = %d, want %d", got, pickerQueryMaxRunes)
	}
	d.appendQuery([]rune{'\n', '\x1b'})
	if got := len([]rune(d.query)); got != pickerQueryMaxRunes {
		t.Fatalf("non-printable input changed bounded query length to %d", got)
	}
}

func TestCommandPaletteKeyboardSearchesMoreThanFiftyCommandsAndRestoresPrompt(t *testing.T) {
	m := testModel(t)
	m.ta.SetValue("unfinished prompt")

	if cmd := m.key(tea.KeyMsg{Type: tea.KeyCtrlP}); cmd != nil {
		t.Fatal("ctrl+p unexpectedly returned a command")
	}
	d, ok := m.dlg.(*pickerDialog)
	if !ok {
		t.Fatalf("ctrl+p opened %T, want picker", m.dlg)
	}
	if len(d.items) <= 50 {
		t.Fatalf("regression fixture has %d commands, want more than 50", len(d.items))
	}

	picked := ""
	d.onPick = func(id string) tea.Cmd {
		picked = id
		return nil
	}
	for _, r := range "diff" {
		m.key(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
	}
	if matches := d.matches(); len(matches) == 0 || matches[0].item.label != "/diff" {
		t.Fatalf("typing diff ranked %v, want /diff first", pickerIDs(matches))
	}
	if view := d.view(80, m.th); !strings.Contains(view, "diff") || !strings.Contains(view, "/diff") {
		t.Fatalf("palette does not show the editable query and result:\n%s", view)
	}
	m.key(tea.KeyMsg{Type: tea.KeyEnter})
	if picked != "diff" {
		t.Fatalf("enter selected %q, want diff", picked)
	}
	if m.dlg != nil {
		t.Fatal("enter did not close the command palette")
	}

	m.key(tea.KeyMsg{Type: tea.KeyCtrlP})
	m.key(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("undo")})
	m.key(tea.KeyMsg{Type: tea.KeyEsc})
	if m.dlg != nil {
		t.Fatal("escape did not cancel the command palette")
	}
	if got := m.ta.Value(); got != "unfinished prompt" {
		t.Fatalf("escape restored prompt %q, want %q", got, "unfinished prompt")
	}
}
