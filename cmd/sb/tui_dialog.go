package main

import (
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/cj-vana/switchboard/internal/permission"
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
	desc := describeRequest(d.req)

	var b strings.Builder
	b.WriteString(th.bold.Render(" approve "+d.req.Tool) + " " + th.dim.Render(desc) + "\n")
	if d.out.Reason != "" {
		b.WriteString(th.dim.Render(" "+d.out.Reason) + "\n")
	}
	if d.out.SandboxAbsent {
		b.WriteString(th.warn.Render(" this command is not sandboxed and can do anything your account can") + "\n")
	}
	b.WriteString("\n")

	always := "yes, and don't ask again for this exact command"
	if d.req.Effect == permission.EffectExternal {
		// The remembered answer for an external tool covers the tool, not one
		// byte-exact invocation; the label has to say what it grants.
		always = "yes, and allow this tool for the rest of the session"
	}
	// The border states the stakes: accent for a routine ask, amber the moment
	// the command leaves the sandbox. Color is information here, not chrome,
	// and the selection bar speaks the same color as the frame.
	frame := th.accent
	if d.out.SandboxAbsent {
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
	title  string
	items  []pickerItem
	sel    int
	onPick func(id string) tea.Cmd
}

func (d *pickerDialog) update(key tea.KeyMsg, th *theme) (bool, tea.Cmd) {
	switch key.String() {
	case "esc":
		return true, nil
	case "up", "k":
		if d.sel > 0 {
			d.sel--
		}
	case "down", "j":
		if d.sel < len(d.items)-1 {
			d.sel++
		}
	case "enter":
		if d.sel >= 0 && d.sel < len(d.items) {
			return true, d.onPick(d.items[d.sel].id)
		}
		return true, nil
	}
	return false, nil
}

func (d *pickerDialog) view(width int, th *theme) string {
	const maxRows = 10
	start := 0
	if d.sel >= maxRows {
		start = d.sel - maxRows + 1
	}
	end := start + maxRows
	if end > len(d.items) {
		end = len(d.items)
	}

	var b strings.Builder
	b.WriteString(th.bold.Render(" "+d.title) + "\n\n")
	for i := start; i < end; i++ {
		it := d.items[i]
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
	b.WriteString(th.faint.Render(" ↑↓ choose · enter select · esc cancel"))
	return b.String()
}
