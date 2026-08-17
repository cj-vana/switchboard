package main

import (
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"
)

// The redesign's invariants, asserted at the SGR level for the same reason
// the markdown tests are: a style field can be dead under a changed
// formatter, and the emitted sequence is what the terminal actually shows.

func TestUserTurnRendersAsCard(t *testing.T) {
	lipgloss.SetColorProfile(termenv.ANSI256)
	tr := testTranscript(t, 80)
	tr.add(&entry{kind: kindUser, text: "fix the failing test and explain what broke so it stays fixed"})

	var lines []string
	for _, l := range tr.flat {
		if strings.Contains(stripANSI(l), "fix the failing test") || strings.Contains(l, "48;5;235") {
			lines = append(lines, l)
		}
	}
	if len(lines) == 0 {
		t.Fatalf("no user card rendered:\n%s", strings.Join(tr.flat, "\n"))
	}
	for i, l := range lines {
		plain := stripANSI(l)
		if !strings.Contains(l, "48;5;235") {
			t.Fatalf("card line %d left the surface ground: %q", i, l)
		}
		if got := lipgloss.Width(plain); got != 80 {
			t.Fatalf("card line %d is %d cells, want the full 80: %q", i, got, plain)
		}
		if i == 0 && !strings.HasPrefix(strings.TrimPrefix(plain, " "), "▌") {
			t.Fatalf("the card's first line lost its patch bar: %q", plain)
		}
		if i > 0 && strings.Contains(plain, "▌") {
			t.Fatalf("continuation line repeats the bar; the card is one object: %q", plain)
		}
	}
}

func TestToolCompletionCarriesAVerdict(t *testing.T) {
	lipgloss.SetColorProfile(termenv.ANSI256)
	tr := testTranscript(t, 80)
	tr.add(&entry{kind: kindTool, tool: toolEntry{name: "exec", desc: "go test ./...", done: true, took: time.Second}})
	tr.add(&entry{kind: kindTool, tool: toolEntry{name: "exec", desc: "go vet ./...", done: true, failed: true, took: 2 * time.Second}})

	joined := strings.Join(tr.flat, "\n")
	if !strings.Contains(joined, "✓") {
		t.Fatalf("a completed tool drew no ✓:\n%s", joined)
	}
	if !strings.Contains(joined, "✗") {
		t.Fatalf("a failed tool drew no ✗:\n%s", joined)
	}
	if strings.Contains(joined, "ok ") || strings.Contains(joined, "failed ") {
		t.Fatalf("verdict words crept back in; the glyphs carry the verdict:\n%s", joined)
	}
}

func TestWorkingLineSpeaksOperator(t *testing.T) {
	lipgloss.SetColorProfile(termenv.ANSI256)
	m := testModel(t)
	line := stripANSI(m.workingLine())
	found := false
	for _, v := range workVerbs {
		if strings.Contains(line, v) {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("working line lost the operator's verbs: %q", line)
	}
	if !strings.Contains(line, m.app.tier.ID) {
		t.Fatalf("working line lost who is working: %q", line)
	}
}

// The transcript anchors at the top: a session shorter than the viewport
// starts where the eye starts, and the empty rows fall below the content.
func TestShortTranscriptAnchorsTop(t *testing.T) {
	lipgloss.SetColorProfile(termenv.ANSI256)
	tr := testTranscript(t, 80)
	tr.add(&entry{kind: kindUser, text: "hello"})
	view := strings.Split(tr.view(10), "\n")
	if len(view) != 10 {
		t.Fatalf("view is %d lines, want 10", len(view))
	}
	if stripANSI(view[0]) == "" {
		t.Fatalf("content sank to the bottom; the first row is blank:\n%s", strings.Join(view, "\n"))
	}
	if stripANSI(view[len(view)-1]) != "" {
		t.Fatalf("padding went above the content, not below it")
	}
}

// Scrolling stops where the content does: a transcript that fits its
// viewport has nothing to scroll past.
func TestScrollClampsToContent(t *testing.T) {
	tr := testTranscript(t, 80)
	tr.add(&entry{kind: kindUser, text: "hello"})
	tr.view(10)
	tr.scrollBy(50)
	if tr.offset != 0 {
		t.Fatalf("scrolled %d lines past a transcript that fits the viewport", tr.offset)
	}
}

// The composer must never paint the bubbles default cursor-line slab: a
// filled input row reads as a broken artifact on any tinted terminal.
func TestComposerHasNoCursorLineSlab(t *testing.T) {
	lipgloss.SetColorProfile(termenv.ANSI256)
	m := testModel(t)
	zone := m.inputZoneView()
	for _, slab := range []string{"48;5;0m", "48;5;255m", "48;2;"} {
		if strings.Contains(zone, slab) {
			t.Fatalf("the composer painted a cursor-line background (%s):\n%q", slab, zone)
		}
	}
	if !strings.Contains(zone, "╭") || !strings.Contains(zone, "╰") {
		t.Fatalf("the composer lost its frame:\n%s", stripANSI(zone))
	}
}

// The turn's closing verdict closes a tool rail with the rail's own corner
// only when a rail is directly above; after prose the corner would hang
// from nothing and reads as a broken rail.
func TestDoneVerdictClosesOnlyAnOpenRail(t *testing.T) {
	lipgloss.SetColorProfile(termenv.ANSI256)
	tr := testTranscript(t, 80)
	tr.add(&entry{kind: kindNotice, level: "done", text: "t1 · 3s", rank: 0, rail: true})
	tr.add(&entry{kind: kindNotice, level: "done", text: "t1 · 3s", rank: 0})
	railed := stripANSI(strings.Join(tr.render(tr.entries[0]), "\n"))
	bare := stripANSI(strings.Join(tr.render(tr.entries[1]), "\n"))
	if !strings.Contains(railed, "└ ✓") {
		t.Fatalf("a rail-closing verdict lost its corner: %q", railed)
	}
	if strings.Contains(bare, "└") {
		t.Fatalf("a verdict after prose grew a corner with nothing above it: %q", bare)
	}
}

// A turn boundary breathes: a user card after content opens with a blank
// line, and the first entry after the banner does not double it.
func TestTurnBoundaryBreathes(t *testing.T) {
	tr := testTranscript(t, 80)
	tr.add(&entry{kind: kindTool, tool: toolEntry{name: "read", desc: "a.go", done: true}})
	e := tr.add(&entry{kind: kindUser, text: "next task"})
	lines := tr.render(e)
	if len(lines) == 0 || stripANSI(lines[0]) != "" {
		t.Fatalf("a user card after a rail did not open with air:\n%q", lines)
	}
}

// When the terminal narrows, the status bar sheds luxuries before facts:
// the sparkline leaves, the mode and context stay.
func TestStatusBarShedsLuxuriesFirst(t *testing.T) {
	lipgloss.SetColorProfile(termenv.ANSI256)
	m := testModel(t)
	m.busy = true
	m.samples = []float64{10, 20, 30}
	m.ctxWindow = 100000
	m.callTokens = 34000
	m.moves = []int{0}
	m.Update(tea.WindowSizeMsg{Width: 60, Height: 30})
	line := stripANSI(m.statusLine())
	if strings.Contains(line, "tok/s") {
		t.Fatalf("a 60-cell bar kept the sparkline: %q", line)
	}
	for _, want := range []string{"default", "ctx 34%"} {
		if !strings.Contains(line, want) {
			t.Fatalf("a 60-cell bar dropped %q: %q", want, line)
		}
	}
}

// The tab's title answers "which terminal was that": workspace and tier,
// marked while a turn runs. Startup and every later update share one
// formatter, so the two can never disagree about what the title holds.
func TestTitleNamesWorkspaceAndTierAndMarksWork(t *testing.T) {
	m := testModel(t)
	idle := m.titleText()
	if !strings.Contains(idle, "ws") || !strings.Contains(idle, "t1") {
		t.Fatalf("idle title lost the workspace or the tier: %q", idle)
	}
	m.busy = true
	if busy := m.titleText(); !strings.HasPrefix(busy, "● ") {
		t.Fatalf("a running turn is not marked in the title: %q", busy)
	}
	m.busy = false
	m.syncTitle()
	if cmd := m.syncTitle(); cmd != nil {
		t.Fatal("an unchanged title was rewritten; the memo should keep quiet")
	}
}

// A click lands where the view says it does: entryAt mirrors the viewport
// math, so clicking a tool rail toggles that rail and a click below the
// content toggles nothing.
func TestClickMapsToTheEntryOnThatRow(t *testing.T) {
	lipgloss.SetColorProfile(termenv.ANSI256)
	m := testModel(t)
	m.tr.reset()
	m.tr.add(&entry{kind: kindInfo, text: "one line"})
	tool := m.tr.add(&entry{kind: kindTool, tool: toolEntry{name: "exec", desc: "go test", done: true, took: time.Second, detail: "ok\nmore"}})
	m.tr.view(10)

	toolStart := m.tr.starts[m.tr.indexOf(tool)]
	if got := m.tr.entryAt(toolStart); got != m.tr.indexOf(tool) {
		t.Fatalf("the tool's first row maps to entry %d, want %d", got, m.tr.indexOf(tool))
	}
	if got := m.tr.entryAt(9); got != -1 {
		t.Fatalf("a click on bottom padding mapped to entry %d, want none", got)
	}
	if got := m.tr.entryAt(-1); got != -1 {
		t.Fatal("a row outside the viewport mapped to an entry")
	}
}

// Queued prompts are visible and droppable: a prompt that silently queued
// is a prompt the user may believe was lost.
func TestQueueShowsAndClears(t *testing.T) {
	m := testModel(t)
	m.queue = []string{"first waiting prompt", "second waiting prompt"}
	cmdQueue(m, "")
	joined := strings.Join(m.tr.flat, "\n")
	if !strings.Contains(joined, "2 queued") || !strings.Contains(joined, "second waiting") {
		t.Fatalf("/queue did not list the queue:\n%s", joined)
	}
	cmdQueue(m, "clear")
	if len(m.queue) != 0 {
		t.Fatal("/queue clear left prompts queued")
	}
}
