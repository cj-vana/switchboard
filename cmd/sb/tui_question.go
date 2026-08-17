package main

import (
	"context"
	"strings"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/cj-vana/switchboard/internal/tools"
)

// questionMsg carries the ask tool's question from the loop's goroutine into
// the program, the permission ask's pattern: the loop blocks on respond
// until the user answers or the turn is cancelled.
type questionMsg struct {
	q       tools.Question
	respond chan tools.Answer
}

// tuiQuestioner resolves the ask tool against a dialog. A program that has
// already quit leaves no one to answer, and the cancelled context is what
// unblocks the loop — never a fabricated answer.
type tuiQuestioner struct{ p *tea.Program }

func (a *tuiQuestioner) AskUser(ctx context.Context, q tools.Question) (tools.Answer, error) {
	respond := make(chan tools.Answer, 1)
	a.p.Send(questionMsg{q: q, respond: respond})
	select {
	case ans := <-respond:
		return ans, nil
	case <-ctx.Done():
		return tools.Answer{}, ctx.Err()
	}
}

// questionDialog is the ask tool's face: the options, a row for an answer of
// the user's own, and esc as the decline. Declining resolves rather than
// dismissing, because the model is blocked mid-turn on this channel and a
// closed dialog that answered nothing would hang the turn.
type questionDialog struct {
	q       tools.Question
	respond chan tools.Answer
	sel     int
	marked  []bool
	typing  bool
	input   textinput.Model
}

func newQuestionDialog(q tools.Question, respond chan tools.Answer) *questionDialog {
	ti := textinput.New()
	ti.Prompt = ""
	return &questionDialog{
		q:       q,
		respond: respond,
		marked:  make([]bool, len(q.Options)),
		input:   ti,
	}
}

// otherRow is the index of the type-your-own row, one past the options.
func (d *questionDialog) otherRow() int { return len(d.q.Options) }

func (d *questionDialog) resolve(ans tools.Answer) bool {
	select {
	case d.respond <- ans:
	default:
	}
	return true
}

// picks collects the marked labels in the order they were offered, so the
// model reads the answer in the shape it asked the question.
func (d *questionDialog) picks() []string {
	var out []string
	for i, opt := range d.q.Options {
		if d.marked[i] {
			out = append(out, opt.Label)
		}
	}
	return out
}

func (d *questionDialog) update(key tea.KeyMsg, th *theme) (bool, tea.Cmd) {
	if d.typing {
		switch key.String() {
		case "esc":
			d.typing = false
			d.input.Reset()
			return false, nil
		case "enter":
			text := strings.TrimSpace(d.input.Value())
			if text == "" {
				d.typing = false
				d.input.Reset()
				return false, nil
			}
			return d.resolve(tools.Answer{Text: text}), nil
		}
		var cmd tea.Cmd
		d.input, cmd = d.input.Update(key)
		return false, cmd
	}

	switch key.String() {
	case "esc":
		return d.resolve(tools.Answer{Declined: true}), nil
	case "up", "k":
		if d.sel > 0 {
			d.sel--
		}
	case "down", "j":
		if d.sel < d.otherRow() {
			d.sel++
		}
	case " ":
		if d.q.Multi && d.sel < len(d.q.Options) {
			d.marked[d.sel] = !d.marked[d.sel]
		}
	case "enter":
		if d.sel == d.otherRow() {
			d.typing = true
			d.input.Focus()
			return false, textinput.Blink
		}
		if d.q.Multi {
			// Marks win when any exist; enter on a bare list answers with
			// the highlighted option, so a single-answer user of a multi
			// question is never forced through the marking step.
			if picked := d.picks(); len(picked) > 0 {
				return d.resolve(tools.Answer{Picked: picked}), nil
			}
		}
		return d.resolve(tools.Answer{Picked: []string{d.q.Options[d.sel].Label}}), nil
	default:
		// Digits quick-pick on a single-select question; on a multi they
		// toggle, so the keyboard shape matches what enter will send.
		if s := key.String(); len(s) == 1 && s[0] >= '1' && s[0] <= '9' {
			i := int(s[0] - '1')
			if i < len(d.q.Options) {
				if d.q.Multi {
					d.marked[i] = !d.marked[i]
					d.sel = i
				} else {
					return d.resolve(tools.Answer{Picked: []string{d.q.Options[i].Label}}), nil
				}
			}
		}
	}
	return false, nil
}

func (d *questionDialog) view(width int, th *theme) string {
	var b strings.Builder
	b.WriteString(th.bold.Render(" "+d.q.Question) + "\n\n")

	for i, opt := range d.q.Options {
		mark := ""
		if d.q.Multi {
			mark = "[ ] "
			if d.marked[i] {
				mark = "[x] "
			}
		}
		row := mark + opt.Label
		if opt.Detail != "" {
			row += "  " + th.dim.Render(opt.Detail)
		}
		if i == d.sel && !d.typing {
			b.WriteString(th.accent.Render(" ▌ ") + th.bold.Render(mark+opt.Label))
			if opt.Detail != "" {
				b.WriteString("  " + th.dim.Render(opt.Detail))
			}
			b.WriteString("\n")
		} else {
			b.WriteString(th.dim.Render("   ") + row + "\n")
		}
	}

	if d.typing {
		b.WriteString(th.accent.Render(" ▌ ") + d.input.View() + "\n")
		b.WriteString(th.faint.Render(" enter answer · esc back to the options"))
	} else {
		other := "type your own answer"
		if d.sel == d.otherRow() {
			b.WriteString(th.accent.Render(" ▌ ") + th.bold.Render(other) + "\n")
		} else {
			b.WriteString(th.dim.Render("   "+other) + "\n")
		}
		hint := " ↑↓ choose · enter answer · esc decline"
		if d.q.Multi {
			hint = " ↑↓ choose · space mark · enter answer · esc decline"
		}
		b.WriteString(th.faint.Render(hint))
	}

	box := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(th.accent.GetForeground()).
		Padding(0, 1).
		Width(max(width-4, 40))
	return box.Render(strings.TrimRight(b.String(), "\n"))
}
