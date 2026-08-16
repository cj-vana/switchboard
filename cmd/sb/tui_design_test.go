package main

import (
	"strings"
	"testing"
	"time"

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
