package main

import (
	"bytes"
	"context"
	"os/exec"
	"strings"
	"time"

	"github.com/alecthomas/chroma/v2/formatters"
	"github.com/alecthomas/chroma/v2/lexers"
	"github.com/alecthomas/chroma/v2/styles"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// diffView is the /diff fullscreen. The diff is highlighted once when it
// loads — §14's "rendered once and cached" — and scrolling is a slice over
// the result.
type diffView struct {
	lines  []string
	offset int
}

type diffLoadedMsg struct {
	lines []string
	err   error
}

// openDiff diffs the workspace against HEAD. This is the harness running a
// read-only git command, not the agent: it does not pass through the
// permission engine.
func openDiff(workspace string, dark bool) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		out, err := exec.CommandContext(ctx, "git", "-C", workspace, "diff", "HEAD", "--").Output()
		if err != nil {
			return diffLoadedMsg{err: err}
		}
		const cap = 1 << 20
		text := string(out)
		if len(text) > cap {
			text = text[:cap] + "\n… diff truncated at 1MB …\n"
		}
		if strings.TrimSpace(text) == "" {
			text = "working tree clean\n"
		}
		return diffLoadedMsg{lines: highlightDiff(text, dark)}
	}
}

// highlightDiff syntax-highlights a unified diff. A highlighting failure falls
// back to plain lines: the diff matters more than its colors.
func highlightDiff(text string, dark bool) []string {
	plain := strings.Split(strings.TrimRight(text, "\n"), "\n")

	lexer := lexers.Get("diff")
	if lexer == nil {
		return plain
	}
	styleName := "github"
	if dark {
		styleName = "github-dark"
	}
	style := styles.Get(styleName)
	formatter := formatters.Get("terminal256")
	if style == nil || formatter == nil {
		return plain
	}
	it, err := lexer.Tokenise(nil, text)
	if err != nil {
		return plain
	}
	var buf bytes.Buffer
	if err := formatter.Format(&buf, style, it); err != nil {
		return plain
	}
	return strings.Split(strings.TrimRight(buf.String(), "\n"), "\n")
}

// key scrolls; it reports true when the view should close. The diff has no
// asynchronous key actions, so its command is always nil.
func (d *diffView) key(msg tea.KeyMsg) (bool, tea.Cmd) {
	switch msg.String() {
	case "esc", "q":
		return true, nil
	case "up", "k":
		d.scroll(-1)
	case "down", "j":
		d.scroll(1)
	case "pgup", "ctrl+u":
		d.scroll(-20)
	case "pgdown", "ctrl+d":
		d.scroll(20)
	case "g":
		d.offset = 0
	case "G":
		d.offset = len(d.lines)
		d.scroll(0)
	}
	return false, nil
}

func (d *diffView) mouse(msg tea.MouseMsg) {
	switch msg.Button {
	case tea.MouseButtonWheelUp:
		d.scroll(-3)
	case tea.MouseButtonWheelDown:
		d.scroll(3)
	}
}

func (d *diffView) scroll(n int) {
	d.offset += n
	if d.offset < 0 {
		d.offset = 0
	}
}

func (d *diffView) view(width, height int, th *theme) string {
	header := th.bold.Render(" git diff HEAD") +
		th.faint.Render("  ↑↓ scroll · pgup/pgdn page · g/G ends · esc close")

	bodyH := height - 2
	if bodyH < 1 {
		bodyH = 1
	}
	if max := len(d.lines) - bodyH; d.offset > max {
		d.offset = max
	}
	if d.offset < 0 {
		d.offset = 0
	}

	end := d.offset + bodyH
	if end > len(d.lines) {
		end = len(d.lines)
	}
	visible := append([]string(nil), d.lines[d.offset:end]...)
	for len(visible) < bodyH {
		visible = append(visible, "")
	}

	footer := ""
	if len(d.lines) > bodyH {
		pct := (d.offset + bodyH) * 100 / len(d.lines)
		if pct > 100 {
			pct = 100
		}
		footer = th.faint.Render(" " + itoa(pct) + "%")
	}
	return header + "\n" + lipgloss.JoinVertical(lipgloss.Left, visible...) + "\n" + footer
}
