package main

import (
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"
)

// The transcript is the scrollback. Entries render to styled lines once and
// are cached per width, so repainting is a slice over a flat line buffer and
// never a re-render (§14: virtualized scrollback, diffs rendered once and
// cached). Only the entry currently streaming re-renders, and that one goes
// through the plain fast path.

type entryKind int

const (
	kindUser entryKind = iota
	kindAssistant
	kindThinking
	kindTool
	kindNotice
	kindRoute
	kindInfo
)

type toolEntry struct {
	name   string
	desc   string
	done   bool
	failed bool
	took   time.Duration
	detail string
}

type entry struct {
	kind entryKind
	text string // user, assistant, thinking, notice, info

	live bool // still streaming; render with the fast path, never the cache

	tool toolEntry

	level        string   // notice level
	routeSummary string   // collapsed route line
	routeLines   []string // the full decision record
	expanded     bool

	cache map[int][]string
}

type transcript struct {
	entries []*entry
	starts  []int // flat offset where each entry's lines begin
	flat    []string

	width int
	th    *theme
	md    *markdown

	offset int // lines scrolled up from the bottom
}

func newTranscript(width int, th *theme, md *markdown) *transcript {
	return &transcript{width: width, th: th, md: md}
}

func (t *transcript) add(e *entry) *entry {
	t.entries = append(t.entries, e)
	t.starts = append(t.starts, len(t.flat))
	t.invalidate(len(t.entries) - 1)
	return e
}

func (t *transcript) last() *entry {
	if len(t.entries) == 0 {
		return nil
	}
	return t.entries[len(t.entries)-1]
}

// appendText extends an entry's raw text and re-renders just that entry.
func (t *transcript) appendText(i int, s string) {
	t.entries[i].text += s
	t.invalidate(i)
}

func (t *transcript) indexOf(e *entry) int {
	for i, x := range t.entries {
		if x == e {
			return i
		}
	}
	return -1
}

// finalize flips a streaming entry to completed, which re-renders it once
// through glamour.
func (t *transcript) finalize(e *entry) {
	if e == nil || !e.live {
		return
	}
	e.live = false
	if i := t.indexOf(e); i >= 0 {
		t.invalidate(i)
	}
}

func (t *transcript) finalizeAll() {
	for i, e := range t.entries {
		if e.live {
			e.live = false
			t.invalidate(i)
		}
	}
}

// invalidate re-renders entry i and splices its lines into the flat buffer.
func (t *transcript) invalidate(i int) {
	e := t.entries[i]
	lines := t.render(e)
	oldStart := t.starts[i]
	oldEnd := len(t.flat)
	if i+1 < len(t.entries) {
		oldEnd = t.starts[i+1]
	}
	delta := len(lines) - (oldEnd - oldStart)
	if delta == 0 {
		copy(t.flat[oldStart:], lines)
		return
	}
	tail := append([]string(nil), t.flat[oldEnd:]...)
	flat := append(t.flat[:oldStart], lines...)
	t.flat = append(flat, tail...)
	for j := i + 1; j < len(t.starts); j++ {
		t.starts[j] += delta
	}
}

func (t *transcript) render(e *entry) []string {
	if !e.live {
		if lines, ok := e.cache[t.width]; ok {
			return lines
		}
	}
	lines := t.renderUncached(e)
	if !e.live {
		if e.cache == nil {
			e.cache = map[int][]string{}
		}
		e.cache[t.width] = lines
	}
	return lines
}

func (t *transcript) renderUncached(e *entry) []string {
	w := t.width
	switch e.kind {
	case kindUser:
		return t.renderUser(e.text, w)
	case kindAssistant:
		if e.live {
			return wrapPlain(e.text, w)
		}
		return t.md.render(e.text)
	case kindThinking:
		lines := wrapPlain(e.text, w)
		for i, l := range lines {
			lines[i] = t.th.thinking.Render(l)
		}
		return lines
	case kindTool:
		return t.renderTool(&e.tool, e.expanded, w)
	case kindNotice:
		return t.renderNotice(e.level, e.text, w)
	case kindRoute:
		return t.renderRoute(e, w)
	default: // kindInfo
		lines := wrapPlain(e.text, w)
		for i, l := range lines {
			lines[i] = t.th.dim.Render(l)
		}
		return lines
	}
}

func (t *transcript) renderUser(text string, w int) []string {
	inner := w - 2
	if inner < 20 {
		inner = 20
	}
	var lines []string
	for i, l := range wrapPlain(text, inner) {
		if i == 0 {
			lines = append(lines, t.th.user.Render("› ")+t.th.text.Render(l))
		} else {
			lines = append(lines, "  "+t.th.text.Render(l))
		}
	}
	return lines
}

func (t *transcript) renderTool(tool *toolEntry, expanded bool, w int) []string {
	head := t.th.accent.Render("⏺ ") + t.th.bold.Render(tool.name)
	if tool.desc != "" {
		head += t.th.dim.Render("(" + truncate(tool.desc, max(w-12-len(tool.name), 8)) + ")")
	}
	if !tool.done {
		return []string{head}
	}
	status := t.th.dim.Render("ok in " + formatDuration(tool.took))
	if tool.failed {
		status = t.th.err.Render("failed in " + formatDuration(tool.took))
	}
	lines := []string{head, "  " + t.th.faint.Render("⎿  ") + status}

	detail := strings.TrimRight(tool.detail, "\n")
	if detail == "" {
		return lines
	}
	if !expanded {
		if first := firstLine(detail); first != "" {
			lines[1] += t.th.dim.Render(": " + first)
		}
		if tool.failed {
			lines = append(lines, indentLines(t.th.err, tailLines(detail, 24), 5)...)
		}
		return lines
	}
	style := t.th.dim
	if tool.failed {
		style = t.th.err
	}
	lines = append(lines, indentLines(style, tailLines(detail, 200), 5)...)
	return lines
}

func (t *transcript) renderNotice(level, text string, w int) []string {
	style := t.th.dim
	prefix := "  note: "
	switch level {
	case "warn":
		style, prefix = t.th.warn, "  warn: "
	case "error":
		style, prefix = t.th.err, "  error: "
	case "route":
		style, prefix = t.th.accent, "  route: "
	case "advisor":
		style, prefix = t.th.accent, "  advisor: "
	}
	var lines []string
	for i, l := range wrapPlain(text, max(w-len(prefix), 20)) {
		if i == 0 {
			lines = append(lines, style.Render(prefix+l))
		} else {
			lines = append(lines, style.Render(strings.Repeat(" ", len(prefix))+l))
		}
	}
	return lines
}

// renderRoute draws a router decision collapsed to one line, per §14, with the
// full record behind ctrl-o.
func (t *transcript) renderRoute(e *entry, w int) []string {
	line := t.th.accent.Render("⏺ route ") + t.th.dim.Render(e.routeSummary)
	if !e.expanded {
		return []string{line + t.th.faint.Render("  (ctrl-o to expand)")}
	}
	lines := []string{line}
	for _, l := range e.routeLines {
		for _, wl := range wrapPlain(l, max(w-4, 20)) {
			lines = append(lines, t.th.dim.Render("    "+wl))
		}
	}
	return lines
}

// view returns the visible window. offset counts lines up from the bottom.
func (t *transcript) view(height int) string {
	if height <= 0 {
		return ""
	}
	total := len(t.flat)
	end := total - t.offset
	if end < 0 {
		end = 0
	}
	start := end - height
	if start < 0 {
		start = 0
	}
	visible := t.flat[start:end]
	if pad := height - len(visible); pad > 0 {
		visible = append(make([]string, pad), visible...)
	}
	return strings.Join(visible, "\n")
}

func (t *transcript) scrollBy(n int) {
	t.offset += n
	if t.offset < 0 {
		t.offset = 0
	}
	if t.offset > len(t.flat) {
		t.offset = len(t.flat)
	}
}

func (t *transcript) scrollToBottom() { t.offset = 0 }

func (t *transcript) setWidth(width int) {
	if width == t.width {
		return
	}
	t.width = width
	t.md.setWidth(width)
	// Width-keyed caches make re-render a cache hit where this width was seen
	// before; either way each entry re-renders once rather than per repaint.
	t.flat = nil
	t.starts = t.starts[:0]
	for _, e := range t.entries {
		t.starts = append(t.starts, len(t.flat))
		t.flat = append(t.flat, t.render(e)...)
	}
}

func (t *transcript) setTheme(th *theme) {
	t.th = th
	for _, e := range t.entries {
		e.cache = nil
	}
	w := t.width
	t.width = -1
	t.setWidth(w)
}

func (t *transcript) reset() {
	t.entries = nil
	t.starts = nil
	t.flat = nil
	t.offset = 0
}

// lastExpandable returns the most recent route or tool entry, which is what
// ctrl-o toggles.
func (t *transcript) lastExpandable() int {
	for i := len(t.entries) - 1; i >= 0; i-- {
		if t.entries[i].kind == kindRoute || t.entries[i].kind == kindTool {
			return i
		}
	}
	return -1
}

func indentLines(style lipgloss.Style, lines []string, indent int) []string {
	pad := strings.Repeat(" ", indent)
	out := make([]string, len(lines))
	for i, l := range lines {
		out[i] = style.Render(pad + l)
	}
	return out
}

func tailLines(s string, n int) []string {
	lines := strings.Split(s, "\n")
	if len(lines) > n {
		lines = append([]string{fmt.Sprintf("… %d earlier lines …", len(lines)-n)}, lines[len(lines)-n:]...)
	}
	return lines
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
