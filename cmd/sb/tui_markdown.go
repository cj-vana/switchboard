package main

import (
	"strings"

	"github.com/charmbracelet/glamour"
	"github.com/charmbracelet/glamour/styles"
	"github.com/muesli/reflow/wordwrap"
)

// markdown renders completed assistant blocks. It is deliberately only for
// completed blocks: re-running a full renderer on every stream delta is the
// standard source of long-session jank (§14). In-flight text goes through the
// plain wrap path and is re-rendered once, here, when the block completes.
type markdown struct {
	width int
	dark  bool
	r     *glamour.TermRenderer
}

func newMarkdown(width int, dark bool) *markdown {
	m := &markdown{width: width, dark: dark}
	m.rebuild()
	return m
}

func (m *markdown) rebuild() {
	width := m.width - 2
	if width < 20 {
		width = 20
	}
	r, err := glamour.NewTermRenderer(
		glamour.WithWordWrap(width),
		glamour.WithPreservedNewLines(),
		styleFor(m.dark),
	)
	if err == nil {
		m.r = r
	}
}

func styleFor(dark bool) glamour.TermRendererOption {
	if dark {
		return glamour.WithStyles(styles.DarkStyleConfig)
	}
	return glamour.WithStyles(styles.LightStyleConfig)
}

func (m *markdown) setWidth(width int) {
	if width == m.width {
		return
	}
	m.width = width
	m.rebuild()
}

func (m *markdown) setDark(dark bool) {
	if dark == m.dark {
		return
	}
	m.dark = dark
	m.rebuild()
}

// render converts markdown to styled lines with trailing blank lines trimmed.
// On any renderer failure the text comes back wrapped and plain: a rendering
// bug must never eat model output.
func (m *markdown) render(text string) []string {
	if m.r != nil {
		if out, err := m.r.Render(text); err == nil {
			return trimBlankLines(strings.Split(strings.TrimRight(out, "\n"), "\n"))
		}
	}
	return wrapPlain(text, m.width)
}

// wrapPlain is the in-flight fast path: word wrap and nothing else.
func wrapPlain(text string, width int) []string {
	if width < 20 {
		width = 20
	}
	var lines []string
	for para := range strings.Lines(text) {
		para = strings.TrimRight(para, "\n")
		if para == "" {
			lines = append(lines, "")
			continue
		}
		lines = append(lines, strings.Split(wordwrap.String(para, width), "\n")...)
	}
	return trimBlankLines(lines)
}

func trimBlankLines(lines []string) []string {
	for len(lines) > 0 && strings.TrimSpace(lines[len(lines)-1]) == "" {
		lines = lines[:len(lines)-1]
	}
	return lines
}
