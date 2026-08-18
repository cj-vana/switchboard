package main

import (
	"fmt"
	"sort"
	"strings"
	"unicode"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/switchboard-code/switchboard/internal/permission"
	"github.com/switchboard-code/switchboard/internal/terminaltext"
)

// dialog is a modal that takes over the input zone until it resolves. The
// transcript stays visible above it.
type dialog interface {
	update(key tea.KeyMsg, th *theme) (done bool, cmd tea.Cmd)
	view(width int, th *theme) string
}

// permissionDialog resolves a permission Ask. Design principle 4 applies to
// the drawing as much as to the engine: the moment of approval is the moment
// that has to be plain, so an unsandboxed command says so in the box.
type permissionDialog struct {
	req     permission.Request
	out     permission.Outcome
	respond chan permission.Response
	sel     int
}

func newPermissionDialog(req permission.Request, out permission.Outcome, respond chan permission.Response) *permissionDialog {
	return &permissionDialog{req: req, out: out, respond: respond}
}

func (d *permissionDialog) resolve(resp permission.Response) bool {
	select {
	case d.respond <- resp:
	default:
	}
	return true
}

func (d *permissionDialog) update(key tea.KeyMsg, th *theme) (bool, tea.Cmd) {
	switch key.String() {
	case "y":
		return d.resolve(permission.Response{Approved: true}), nil
	case "a":
		return d.resolve(permission.Response{Approved: true, Remember: true}), nil
	case "n", "esc":
		return d.resolve(permission.Response{}), nil
	case "up", "k":
		if d.sel > 0 {
			d.sel--
		}
	case "down", "j":
		if d.sel < 2 {
			d.sel++
		}
	case "enter":
		switch d.sel {
		case 0:
			return d.resolve(permission.Response{Approved: true}), nil
		case 1:
			return d.resolve(permission.Response{Approved: true, Remember: true}), nil
		default:
			return d.resolve(permission.Response{}), nil
		}
	}
	return false, nil
}

func (d *permissionDialog) view(width int, th *theme) string {
	desc := approvalDescription(d.req)

	var b strings.Builder
	b.WriteString(th.bold.Render(" approve "+terminaltext.Escape(d.req.Tool)) + " " + th.dim.Render(desc) + "\n")
	if d.out.Reason != "" {
		b.WriteString(th.dim.Render(" "+approvalReason(d.out.Reason)) + "\n")
	}
	if d.out.SandboxAbsent {
		b.WriteString(th.warn.Render(" FULL HOST ACCESS: this command is not sandboxed; it can access files outside the workspace and the network") + "\n")
	}
	if d.req.Effect == permission.EffectExecute && d.req.Network {
		b.WriteString(th.warn.Render(" FULL NETWORK ACCESS REQUESTED: this command can send workspace data off this machine") + "\n")
	}
	b.WriteString("\n")

	always := "yes, and don't ask again for this exact command"
	if d.req.Effect == permission.EffectExternal {
		// The remembered answer for an external tool covers the tool, not one
		// byte-exact invocation; the label has to say what it grants. A web
		// tool carries the host in its path, so its remembered answer is
		// per-host and the label says the host.
		always = "yes, and allow this tool for the rest of the session"
		if d.req.Path != "" {
			always = "yes, and allow " + terminaltext.Escape(d.req.Path) + " for the rest of the session"
		}
	}
	// The border states the stakes: accent for a routine ask, amber the moment
	// the command leaves the sandbox. Color is information here, not chrome,
	// and the selection bar speaks the same color as the frame.
	frame := th.accent
	if d.out.SandboxAbsent || (d.req.Effect == permission.EffectExecute && d.req.Network) {
		frame = th.warn
	}
	options := []string{
		"yes",
		always,
		"no",
	}
	for i, opt := range options {
		if i == d.sel {
			b.WriteString(frame.Render(" ▌ ") + th.bold.Render(opt) + "\n")
		} else {
			b.WriteString(th.dim.Render("   "+opt) + "\n")
		}
	}
	b.WriteString(th.faint.Render(" y yes · a always · n no · ↑↓ choose"))
	box := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(frame.GetForeground()).
		Padding(0, 1).
		Width(max(width-4, 40))
	return box.Render(strings.TrimRight(b.String(), "\n"))
}

func borderColor(th *theme) lipgloss.Color {
	if th.dark {
		return lipgloss.Color("238")
	}
	return lipgloss.Color("250")
}

// pickerDialog is the generic chooser behind /tier, /resume, /mode, and
// /theme.
type pickerItem struct {
	id      string
	label   string
	desc    string
	current bool
}

type pickerDialog struct {
	title    string
	items    []pickerItem
	sel      int
	query    string
	onPick   func(id string) tea.Cmd
	onCancel func() tea.Cmd
}

const pickerQueryMaxRunes = 128

type pickerMatch struct {
	item  pickerItem
	index int
	score int
}

func (d *pickerDialog) update(key tea.KeyMsg, th *theme) (bool, tea.Cmd) {
	matches := d.matches()
	d.clampSelection(len(matches))
	switch key.String() {
	case "esc":
		if d.onCancel != nil {
			return true, d.onCancel()
		}
		return true, nil
	case "up":
		if d.sel > 0 {
			d.sel--
		}
	case "down":
		if d.sel < len(matches)-1 {
			d.sel++
		}
	case "backspace":
		runes := []rune(d.query)
		if len(runes) > 0 {
			d.setQuery(string(runes[:len(runes)-1]))
		}
	case "ctrl+w":
		d.setQuery(trimLastPickerWord(d.query))
	case "ctrl+u":
		d.setQuery("")
	case "enter":
		if d.sel >= 0 && d.sel < len(matches) {
			if d.onPick == nil {
				return true, nil
			}
			return true, d.onPick(matches[d.sel].item.id)
		}
		return false, nil
	default:
		if key.Type == tea.KeyRunes {
			d.appendQuery(key.Runes)
		} else if key.Type == tea.KeySpace {
			d.appendQuery([]rune{' '})
		}
	}
	return false, nil
}

func (d *pickerDialog) view(width int, th *theme) string {
	const maxRows = 10
	matches := d.matches()
	d.clampSelection(len(matches))
	start := 0
	if d.sel >= maxRows {
		start = d.sel - maxRows + 1
	}
	end := start + maxRows
	if end > len(matches) {
		end = len(matches)
	}

	var b strings.Builder
	b.WriteString(th.bold.Render(" "+d.title) + "\n")
	b.WriteString(th.accent.Render(" search ") + " " + terminaltext.Escape(d.query) + th.faint.Render("▌") + "\n\n")
	if len(matches) == 0 {
		b.WriteString(th.dim.Render(" no matches") + "\n")
	}
	for i := start; i < end; i++ {
		it := matches[i].item
		marker := "   "
		if it.current {
			marker = th.ok.Render(" ● ")
		}
		row := marker + it.label
		if it.desc != "" {
			row += "  " + th.dim.Render(it.desc)
		}
		if i == d.sel {
			row = th.accent.Render(" ▌ ") + th.bold.Render(it.label)
			if it.desc != "" {
				row += "  " + th.dim.Render(it.desc)
			}
		}
		b.WriteString(row + "\n")
	}
	// A list cut off at the viewport with nothing to say so reads as the
	// whole list, and the row someone came for sits below the fold unfound.
	if rest := len(matches) - end; rest > 0 {
		b.WriteString(th.dim.Render(fmt.Sprintf("   ↓ %d more", rest)) + "\n")
	}
	hint := " type to filter · ↑↓ choose · enter select · esc cancel"
	if len(matches) > maxRows {
		hint = fmt.Sprintf(" %d-%d of %d · %s", start+1, end, len(matches), strings.TrimSpace(hint))
	}
	b.WriteString(th.faint.Render(hint))
	return b.String()
}

func (d *pickerDialog) appendQuery(typed []rune) {
	query := []rune(d.query)
	remaining := pickerQueryMaxRunes - len(query)
	if remaining <= 0 {
		return
	}
	for _, r := range typed {
		if remaining == 0 {
			break
		}
		if !unicode.IsPrint(r) {
			continue
		}
		query = append(query, r)
		remaining--
	}
	d.setQuery(string(query))
}

func (d *pickerDialog) setQuery(query string) {
	before := d.matches()
	selected := -1
	if d.sel >= 0 && d.sel < len(before) {
		selected = before[d.sel].index
	}

	d.query = query
	after := d.matches()
	if selected >= 0 {
		for i := range after {
			if after[i].index == selected {
				d.sel = i
				return
			}
		}
	}
	d.sel = 0
	d.clampSelection(len(after))
}

func (d *pickerDialog) clampSelection(n int) {
	if n == 0 {
		d.sel = 0
		return
	}
	if d.sel < 0 {
		d.sel = 0
	}
	if d.sel >= n {
		d.sel = n - 1
	}
}

func (d *pickerDialog) matches() []pickerMatch {
	query := strings.TrimSpace(strings.ToLower(d.query))
	matches := make([]pickerMatch, 0, len(d.items))
	for i, item := range d.items {
		score, ok := pickerItemScore(query, item)
		if !ok {
			continue
		}
		matches = append(matches, pickerMatch{item: item, index: i, score: score})
	}
	if query != "" {
		sort.SliceStable(matches, func(i, j int) bool {
			if matches[i].score != matches[j].score {
				return matches[i].score < matches[j].score
			}
			return matches[i].index < matches[j].index
		})
	}
	return matches
}

func pickerItemScore(query string, item pickerItem) (int, bool) {
	if query == "" {
		return 0, true
	}
	fields := []struct {
		text   string
		weight int
	}{
		{strings.ToLower(item.id), 0},
		{strings.ToLower(item.label), 8},
		{strings.ToLower(item.desc), 24},
	}

	total := 0
	for _, term := range strings.Fields(query) {
		best := -1
		for _, field := range fields {
			if score, ok := pickerFieldScore(term, field.text); ok {
				score += field.weight
				if best < 0 || score < best {
					best = score
				}
			}
		}
		if best < 0 {
			return 0, false
		}
		total += best
	}
	return total, true
}

func pickerFieldScore(query, field string) (int, bool) {
	if query == field {
		return 0, true
	}
	if strings.HasPrefix(field, query) {
		return 20 + len([]rune(field)) - len([]rune(query)), true
	}
	if at := strings.Index(field, query); at >= 0 {
		return 100 + at*2 + len([]rune(field)) - len([]rune(query)), true
	}

	queryRunes := []rune(query)
	fieldRunes := []rune(field)
	qi := 0
	first := -1
	last := -1
	for i, r := range fieldRunes {
		if qi >= len(queryRunes) || r != queryRunes[qi] {
			continue
		}
		if first < 0 {
			first = i
		}
		last = i
		qi++
	}
	if qi != len(queryRunes) {
		return 0, false
	}
	span := last - first + 1
	gaps := span - len(queryRunes)
	return 300 + first*2 + gaps*4 + len(fieldRunes) - len(queryRunes), true
}

func trimLastPickerWord(query string) string {
	runes := []rune(query)
	for len(runes) > 0 && unicode.IsSpace(runes[len(runes)-1]) {
		runes = runes[:len(runes)-1]
	}
	for len(runes) > 0 && !unicode.IsSpace(runes[len(runes)-1]) {
		runes = runes[:len(runes)-1]
	}
	return string(runes)
}

// textPromptMsg opens the text dialog. It is a message rather than a direct
// assignment for the same reason secretPromptMsg is: the picker that asked for
// it is mid-update, and its close would null the dialog out again.
type textPromptMsg struct {
	title   string
	help    string
	initial string

	// submit runs with the trimmed entry. An empty entry cancels, so a
	// caller never has to decide what an empty string meant.
	submit func(value string) tea.Cmd
}

// textDialog takes one line of visible text: a server address, a model id.
// It is deliberately not secretDialog with the echo turned back on — a
// dialog that sometimes hides what is typed and sometimes does not is one
// mistake away from showing a key.
type textDialog struct {
	title  string
	help   string
	input  textinput.Model
	submit func(value string) tea.Cmd
}

func newTextDialog(msg textPromptMsg) *textDialog {
	ti := textinput.New()
	ti.Prompt = ""
	ti.SetValue(msg.initial)
	ti.CursorEnd()
	ti.Focus()
	return &textDialog{title: msg.title, help: msg.help, input: ti, submit: msg.submit}
}

func (d *textDialog) update(key tea.KeyMsg, th *theme) (bool, tea.Cmd) {
	switch key.String() {
	case "esc":
		return true, nil
	case "enter":
		value := strings.TrimSpace(d.input.Value())
		if value == "" {
			return true, nil
		}
		return true, d.submit(value)
	}
	var cmd tea.Cmd
	d.input, cmd = d.input.Update(key)
	return false, cmd
}

func (d *textDialog) view(width int, th *theme) string {
	var b strings.Builder
	b.WriteString(th.bold.Render(" "+d.title) + "\n")
	if d.help != "" {
		b.WriteString(th.dim.Render(" "+d.help) + "\n")
	}
	b.WriteString("\n " + d.input.View() + "\n")
	b.WriteString(th.faint.Render(" enter save · esc cancel"))

	box := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(borderColor(th)).
		Padding(0, 1).
		Width(max(width-4, 40))
	return box.Render(b.String())
}
