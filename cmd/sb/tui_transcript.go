package main

import (
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"

	"github.com/cj-vana/switchboard/internal/tools"
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
	kindTodo
	// kindRaw holds pre-styled lines rendered verbatim, for the banner: the
	// one place composition is done by the builder, not the renderer.
	kindRaw
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
	rail         bool     // a "done" notice closing a tool rail draws └
	routeSummary string   // collapsed route line
	routeLines   []string // the full decision record
	todos        []tools.TodoItem
	expanded     bool

	// padTop marks an entry that opens with a blank line: a turn boundary
	// (user card, first prose after rails) breathes. Decided once, at add
	// time, when everything before it is final.
	padTop bool

	// rank is the ladder position this entry happened on, captured at
	// creation so a t1-era line keeps t1's color after an escalation; -1
	// means the entry has no rung association and renders neutral.
	rank int

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
	height int // the viewport view last drew, for the scroll clamp
}

func newTranscript(width int, th *theme, md *markdown) *transcript {
	return &transcript{width: width, th: th, md: md}
}

func (t *transcript) add(e *entry) *entry {
	// A user card or the turn's first prose opens with air. The decision is
	// safe to bake into the cache because everything before this entry is
	// already final when it arrives.
	if (e.kind == kindUser || e.kind == kindAssistant) && len(t.flat) > 0 && t.flat[len(t.flat)-1] != "" {
		e.padTop = true
	}
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
	lines := t.composed(e)
	if !e.live {
		if e.cache == nil {
			e.cache = map[int][]string{}
		}
		e.cache[t.width] = lines
	}
	return lines
}

// composed is renderUncached plus the page's composition rules, applied in
// the one place every flat rebuild goes through: a one-cell left margin so
// content never presses against the terminal edge, and a breathing line
// after each block. Tool and thinking entries stay tight — the rail groups
// them — and blanks do not stack.
func (t *transcript) composed(e *entry) []string {
	lines := t.renderUncached(e)
	for i, l := range lines {
		if l != "" {
			lines[i] = " " + l
		}
	}
	if e.padTop {
		lines = append([]string{""}, lines...)
	}
	switch e.kind {
	case kindTool, kindThinking, kindInfo:
		return lines
	}
	// The gap is earned, not fixed: multi-line blocks and user turns breathe,
	// while consecutive one-line notices and route lines pack tight the way
	// the tool rail does. Density around chatter, air around substance.
	if e.kind != kindUser && len(lines) <= 1 {
		return lines
	}
	if len(lines) > 0 && lines[len(lines)-1] != "" {
		lines = append(lines, "")
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
		return t.renderTool(&e.tool, e.expanded, e.rank, w)
	case kindTodo:
		return t.renderTodo(e.todos, e.rank, w)
	case kindNotice:
		return t.renderNotice(e, w)
	case kindRoute:
		return t.renderRoute(e, w)
	case kindRaw:
		return strings.Split(e.text, "\n")
	default: // kindInfo
		lines := wrapPlain(e.text, w)
		for i, l := range lines {
			lines[i] = t.th.dim.Render(l)
		}
		return lines
	}
}

// The transcript's glyph language is the patch panel: a heavy bar marks what
// the user plugged in, thin rails carry tool activity, and a diamond junction
// marks the router switching jacks. Box-drawing characters only, because
// they render everywhere a terminal does.
//
// A user turn renders as a card on the surface ground, bar on the first line
// and the rest padded flush: in a stream of rung-colored rails the eye needs
// what *you* said to land as one object, not as lines that happen to share a
// prefix. Every segment carries the ground, the way the status bar does.
func (t *transcript) renderUser(text string, w int) []string {
	inner := w - 4 // the 1-cell page margin, the two-cell bar, a right pad
	if inner < 20 {
		inner = 20
	}
	on := t.th.onSurface
	bar := on(t.th.user).Render("▌ ")
	pad := on(t.th.user).Render("  ")
	var lines []string
	for i, l := range wrapPlain(text, inner) {
		lead := pad
		if i == 0 {
			lead = bar
		}
		gap := w - 1 - 2 - lipgloss.Width(l)
		if gap < 1 {
			gap = 1
		}
		lines = append(lines, lead+on(t.th.text).Render(l)+on(t.th.text).Render(strings.Repeat(" ", gap)))
	}
	return lines
}

func (t *transcript) renderTool(tool *toolEntry, expanded bool, rank int, w int) []string {
	rail := t.th.faint
	if rank >= 0 {
		rail = t.th.rung(rank)
	}
	head := rail.Render("│ ") + t.th.bold.Render(tool.name)
	if tool.desc != "" {
		head += t.th.dim.Render(" " + truncate(tool.desc, max(w-12-len(tool.name), 8)))
	}
	if !tool.done {
		return []string{head}
	}
	// Completion is a verdict glyph, not a word: ✓ and ✗ read at scroll speed,
	// where "ok" and "failed" read at reading speed.
	status := t.th.ok.Render("✓") + t.th.dim.Render(" "+formatDuration(tool.took))
	if tool.failed {
		status = t.th.err.Render("✗ " + formatDuration(tool.took))
	}
	lines := []string{head, rail.Render("└ ") + status}

	detail := strings.TrimRight(tool.detail, "\n")
	if detail == "" {
		return lines
	}
	if !expanded {
		// A failure shows its tail in full below; repeating the first line
		// inline as well would print a one-line error twice.
		if first := firstLine(detail); first != "" && !tool.failed {
			lines[1] += t.th.dim.Render(" · " + first)
		}
		if tool.failed {
			lines = append(lines, indentLines(t.th.err, tailLines(detail, 24), 4)...)
		}
		return lines
	}
	style := t.th.dim
	if tool.failed {
		style = t.th.err
	}
	lines = append(lines, indentLines(style, tailLines(detail, 200), 4)...)
	return lines
}

// renderTodo draws the task list the model maintains. It replaces the tool
// rail entry for a todo call, so the transcript shows the list itself where
// every other tool shows a verdict line: the list is the result. The glyphs
// are the transcript's own: ✓ for done, ▸ for the one active item, · for
// pending, and the rail carries the rung color like every tool line.
func (t *transcript) renderTodo(items []tools.TodoItem, rank int, w int) []string {
	rail := t.th.faint
	if rank >= 0 {
		rail = t.th.rung(rank)
	}
	done := 0
	for _, item := range items {
		if item.Status == tools.TodoDone {
			done++
		}
	}
	head := rail.Render("│ ") + t.th.bold.Render("tasks") +
		t.th.dim.Render(fmt.Sprintf(" %d/%d", done, len(items)))
	lines := []string{head}
	for i, item := range items {
		lead := "│ "
		if i == len(items)-1 {
			lead = "└ "
		}
		text := truncate(item.Text, max(w-8, 8))
		var body string
		switch item.Status {
		case tools.TodoDone:
			body = t.th.ok.Render("✓ ") + t.th.dim.Render(text)
		case tools.TodoActive:
			body = t.th.bold.Render("▸ " + text)
		default:
			body = t.th.dim.Render("· " + text)
		}
		lines = append(lines, rail.Render(lead)+body)
	}
	return lines
}

// renderNotice speaks the transcript's glyph vocabulary: the glyph carries
// the color and the severity, the text stays quiet. Word prefixes read as
// debug output; a mark reads as the page's own voice.
func (t *transcript) renderNotice(e *entry, w int) []string {
	style, glyph, body := t.th.dim, "·", t.th.dim
	switch e.level {
	case "warn":
		style, glyph, body = t.th.warn, "△", t.th.warn
	case "error":
		style, glyph, body = t.th.err, "✗", t.th.err
	case "route":
		// The junction marker wears the rung the move landed on; a route
		// event with no destination rank stays on the accent.
		style, glyph = t.th.accent, "◆"
		if e.rank >= 0 {
			style = t.th.rung(e.rank)
		}
	case "advisor":
		style, glyph = t.th.accent, "◇"
	case "watch":
		style, glyph = t.th.ok, "✓"
	case "done":
		// The turn's closing verdict. It closes a tool rail with the rail's
		// own └ when one is directly above; after prose it stands alone,
		// because a corner with nothing over it reads as a broken rail.
		rail := t.th.dim
		if e.rank >= 0 {
			rail = t.th.rung(e.rank)
		}
		lead := t.th.ok.Render("✓ ")
		if e.rail {
			lead = rail.Render("└ ") + t.th.ok.Render("✓ ")
		}
		return []string{lead + t.th.dim.Render(e.text)}
	}
	var lines []string
	for i, l := range wrapPlain(e.text, max(w-2, 20)) {
		if i == 0 {
			lines = append(lines, style.Render(glyph+" ")+body.Render(l))
		} else {
			lines = append(lines, "  "+body.Render(l))
		}
	}
	return lines
}

// renderRoute draws a router decision collapsed to one line, per §14, with
// the full record behind ctrl-o. The diamond is the junction marker, colored
// by the rung the decision landed on: the one glyph that means "the router
// switched jacks here".
func (t *transcript) renderRoute(e *entry, w int) []string {
	marker := t.th.accent
	if e.rank >= 0 {
		marker = t.th.rung(e.rank)
	}
	line := marker.Render("◆ ") + t.th.dim.Render(e.routeSummary)
	if !e.expanded {
		return []string{line}
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
// A transcript shorter than the viewport anchors at the top — the session
// starts where the eye starts, and the empty rows fall below the content.
func (t *transcript) view(height int) string {
	if height <= 0 {
		return ""
	}
	t.height = height
	t.clampOffset()
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
		visible = append(append([]string(nil), visible...), make([]string, pad)...)
	}
	return strings.Join(visible, "\n")
}

func (t *transcript) scrollBy(n int) {
	t.offset += n
	t.clampOffset()
}

// clampOffset keeps the offset inside what can actually scroll: when the
// whole transcript fits the viewport there is nothing to scroll past.
func (t *transcript) clampOffset() {
	limit := len(t.flat) - t.height
	if t.height <= 0 {
		limit = len(t.flat)
	}
	if limit < 0 {
		limit = 0
	}
	if t.offset > limit {
		t.offset = limit
	}
	if t.offset < 0 {
		t.offset = 0
	}
}

func (t *transcript) scrollToBottom() { t.offset = 0 }

// maxTextWidth caps prose regardless of terminal width. On a wide monitor,
// uncapped text running edge to edge is the most legible nobody-designed-this
// signal a transcript can send.
const maxTextWidth = 120

func (t *transcript) setWidth(width int) {
	if width > maxTextWidth {
		width = maxTextWidth
	}
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
