package main

import (
	"strings"
	"testing"
)

// A paragraph typed without pressing enter is one logical line and many
// terminal rows. Counting newlines called that one row and scrolled the text
// out of sight as it was typed.
func TestPromptGrowsWithWrappedText(t *testing.T) {
	m := testModel(t)
	m.width, m.height = 100, 40
	m.ta.SetWidth(m.width - 6)

	m.ta.SetValue("short")
	m.growInput()
	if got := m.ta.Height(); got != 1 {
		t.Fatalf("a short line is %d rows, want 1", got)
	}

	m.ta.SetValue(strings.Repeat("word ", 60))
	m.growInput()
	wrapped := m.ta.Height()
	if wrapped < 3 {
		t.Fatalf("a wrapped paragraph is %d rows; it should grow past one", wrapped)
	}
	if wrapped != inputRows(m.ta) {
		t.Fatalf("height %d disagrees with the textarea's own wrap count %d", wrapped, inputRows(m.ta))
	}

	// Hard newlines still count, and the two kinds add up.
	m.ta.SetValue("a\nb\nc")
	m.growInput()
	if got := m.ta.Height(); got != 3 {
		t.Fatalf("three typed lines are %d rows, want 3", got)
	}

	// The prompt never takes the whole pane from the transcript.
	m.ta.SetValue(strings.Repeat("word ", 4000))
	m.growInput()
	if got := m.ta.Height(); got > m.inputCeiling() {
		t.Fatalf("the prompt grew to %d rows, past its ceiling of %d", got, m.inputCeiling())
	}

	// Narrowing the pane rewraps what is already there.
	tall := m.ta.Height()
	m.ta.SetValue(strings.Repeat("word ", 60))
	m.growInput()
	wide := m.ta.Height()
	m.ta.SetWidth(30)
	m.growInput()
	if m.ta.Height() <= wide {
		t.Fatalf("a narrower pane should need more rows: %d then %d (ceiling %d)", wide, m.ta.Height(), tall)
	}
}
