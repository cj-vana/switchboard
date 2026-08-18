package main

import (
	"fmt"
	"reflect"
	"strings"
	"testing"
)

func TestAggregateStartupNotesDeduplicatesRoutineDiagnostics(t *testing.T) {
	var notes []mcpNote
	for i := 0; i < 50; i++ {
		notes = append(notes, mcpNote{
			level: "warn",
			text:  fmt.Sprintf("plugins unsupported-control: Context block is not supported (/tmp/plugin-%02d/SKILL.md)", i),
		})
	}
	notes = append(notes,
		mcpNote{"", "skills: 12 discovered, 8 model-visible"},
		mcpNote{"warn", "mcp server docs did not connect: deadline exceeded"},
		mcpNote{"", "hooks: 3 loaded"},
		mcpNote{"warn", "LSP: gopls unavailable"},
		mcpNote{"warn", "plugins/native managed-policy-path: ignored an optional source"},
		mcpNote{"", "delegate: 2 named agents"},
	)

	report := aggregateStartupNotes(notes)
	if len(report.Details) != len(notes) {
		t.Fatalf("details = %d, want every one of %d notes", len(report.Details), len(notes))
	}
	want := []startupNoteGroup{
		{startupNoteSkills, 1, 1},
		{startupNotePlugins, 1, 50},
		{startupNoteMCP, 1, 1},
		{startupNoteHooks, 1, 1},
		{startupNoteLSP, 1, 1},
		{startupNotePolicy, 1, 1},
		{startupNoteOther, 1, 1},
	}
	if !reflect.DeepEqual(report.Groups, want) {
		t.Fatalf("groups = %#v, want %#v", report.Groups, want)
	}
	joined := strings.Join(report.Summary, "\n")
	for _, wantText := range []string{"56 notes", "7 unique", "49 repeated", "plugins 1/50", "/doctor extensions"} {
		if !strings.Contains(joined, wantText) {
			t.Errorf("summary omitted %q:\n%s", wantText, joined)
		}
	}
	assertBoundedStartupSummary(t, report.Summary)
}

func TestAggregateStartupNotesPreservesImportantNotesIndividually(t *testing.T) {
	notes := []mcpNote{
		{"warn", "t2 is served by its fallback local/qwen: primary unavailable"},
		{"error", "plugin alpha could not load"},
		{"fatal", "plugins: signature verification failed"},
		{"warn", "required MCP server build is unavailable"},
		{"warn", "repository hooks stay off until /trust grant"},
		{"warn", "sandbox containment self-test failed"},
		{"", "skills: 3 discovered"},
	}
	report := aggregateStartupNotes(notes)
	if len(report.Highlights) != 6 {
		t.Fatalf("highlights = %#v, want six individually visible notes", report.Highlights)
	}
	joined := startupNoteText(report.Highlights)
	for _, important := range notes[:6] {
		if !strings.Contains(joined, important.text) {
			t.Errorf("important startup fact was hidden: %q\n%s", important.text, joined)
		}
	}
	if strings.Contains(joined, notes[6].text) {
		t.Errorf("routine inventory note became a highlight: %s", joined)
	}
	if report.Groups[2].Unique != 1 || report.Groups[2].Total != 1 {
		t.Fatalf("required MCP note was not counted individually: %#v", report.Groups[2])
	}
}

func TestAggregateStartupNotesDetailsStayOrderedExactAndSafe(t *testing.T) {
	notes := []mcpNote{
		{"warn", "first\x1b]0;spoof\x07"},
		{"error", "second\rOVERWRITE"},
		{"", "third\u202ereverse"},
	}
	report := aggregateStartupNotes(notes)
	want := []mcpNote{
		{"warn", `first\x1b]0;spoof\x07`},
		{"error", `second\x0dOVERWRITE`},
		{"", `third\u202ereverse`},
	}
	if !reflect.DeepEqual(report.Details, want) {
		t.Fatalf("details = %#v, want ordered safe expansion %#v", report.Details, want)
	}
	for _, note := range append(report.Details, report.Highlights...) {
		for _, unsafe := range []string{"\x1b", "\x07", "\r", "\u202e"} {
			if strings.Contains(note.level+note.text, unsafe) {
				t.Fatalf("renderable note retained terminal control %q: %#v", unsafe, note)
			}
		}
	}
	if notes[0].text != "first\x1b]0;spoof\x07" {
		t.Fatal("aggregation mutated its input")
	}
}

func TestAggregateStartupNotesSummaryAndHighlightsAreDeterministic(t *testing.T) {
	forward := []mcpNote{
		{"warn", "LSP: rust-analyzer unavailable"},
		{"error", "plugins: alpha is invalid"},
		{"warn", "skills: metadata ignored"},
		{"warn", "t1 is served by its fallback local"},
		{"", "hooks: 2 loaded"},
	}
	reverse := append([]mcpNote(nil), forward...)
	for left, right := 0, len(reverse)-1; left < right; left, right = left+1, right-1 {
		reverse[left], reverse[right] = reverse[right], reverse[left]
	}

	a := aggregateStartupNotes(forward)
	b := aggregateStartupNotes(reverse)
	if !reflect.DeepEqual(a.Summary, b.Summary) {
		t.Fatalf("summary depends on discovery order:\n%q\n%q", a.Summary, b.Summary)
	}
	if !reflect.DeepEqual(a.Highlights, b.Highlights) {
		t.Fatalf("highlights depend on discovery order:\n%#v\n%#v", a.Highlights, b.Highlights)
	}
	if reflect.DeepEqual(a.Details, b.Details) {
		t.Fatal("details lost the exact discovery order")
	}
	assertBoundedStartupSummary(t, a.Summary)
}

func TestAggregateStartupNotesRenderingIsHardBounded(t *testing.T) {
	var notes []mcpNote
	for i, category := range []string{"skills", "plugins", "MCP", "hooks", "LSP", "policy", "other"} {
		for j := 0; j <= i; j++ {
			notes = append(notes, mcpNote{"", fmt.Sprintf("%s: distinct problem %d", category, j)})
		}
	}
	report := aggregateStartupNotes(notes)
	assertBoundedStartupSummary(t, report.Summary)
	if len(report.Groups) != len(startupNoteCategoryOrder) {
		t.Fatalf("groups = %d, want all %d categories", len(report.Groups), len(startupNoteCategoryOrder))
	}
}

func TestAggregateStartupNotesBoundsNonfatalErrorFlood(t *testing.T) {
	var notes []mcpNote
	for i := 0; i < 50; i++ {
		notes = append(notes, mcpNote{
			level: "error",
			text:  fmt.Sprintf("plugins invalid-component: unsupported field (/tmp/duplicate-%02d/plugin.json)", i),
		})
	}
	for i := 0; i < 50; i++ {
		notes = append(notes, mcpNote{
			level: "error",
			text:  fmt.Sprintf("plugins invalid-component-%02d: distinct malformed declaration", i),
		})
	}
	for i := 0; i < 2; i++ {
		notes = append(notes,
			mcpNote{"fatal", "plugins: extension registry is corrupt"},
			mcpNote{"error", "plugins: required extension compiler is unavailable"},
		)
	}

	report := aggregateStartupNotes(notes)
	if len(report.Details) != 104 {
		t.Fatalf("details = %d, want every one of 104 diagnostics", len(report.Details))
	}
	if got := report.Groups[1]; got != (startupNoteGroup{startupNotePlugins, 53, 104}) {
		t.Fatalf("plugin group = %#v, want duplicate issues counted once while mandatory highlights stay individual", got)
	}
	if len(report.Highlights) != startupNoncriticalHighlightLimit+4 {
		t.Fatalf("highlights = %d, want %d bounded ordinary plus four mandatory failures: %#v",
			len(report.Highlights), startupNoncriticalHighlightLimit+4, report.Highlights)
	}
	highlights := startupNoteText(report.Highlights)
	if got := strings.Count(highlights, "extension registry is corrupt"); got != 2 {
		t.Fatalf("fatal failures shown %d times, want both individually:\n%s", got, highlights)
	}
	if got := strings.Count(highlights, "required extension compiler is unavailable"); got != 2 {
		t.Fatalf("required failures shown %d times, want both individually:\n%s", got, highlights)
	}
	ordinary := 0
	for _, note := range report.Highlights {
		if note.level == "error" && !strings.Contains(note.text, "required") {
			ordinary++
		}
	}
	if ordinary != startupNoncriticalHighlightLimit {
		t.Fatalf("ordinary visible errors = %d, want hard limit %d", ordinary, startupNoncriticalHighlightLimit)
	}
	joined := strings.Join(report.Summary, "\n")
	for _, want := range []string{"104 notes", "53 unique", "51 repeated", "46 more in /doctor extensions", "plugins 53/104"} {
		if !strings.Contains(joined, want) {
			t.Errorf("bounded summary omitted %q:\n%s", want, joined)
		}
	}
	assertBoundedStartupSummary(t, report.Summary)
}

func TestAggregateStartupNotesBoundsDistinctPolicyWarningsBeforeOrdinaryErrors(t *testing.T) {
	var notes []mcpNote
	for i := 0; i < 50; i++ {
		notes = append(notes, mcpNote{"error", fmt.Sprintf("plugins policy-%02d: managed source unavailable", i)})
	}
	for i := 0; i < 50; i++ {
		notes = append(notes, mcpNote{"error", fmt.Sprintf("plugins malformed-%02d: invalid declaration", i)})
	}
	report := aggregateStartupNotes(notes)
	if len(report.Details) != 100 || len(report.Highlights) != startupNoncriticalHighlightLimit {
		t.Fatalf("details=%d highlights=%d, want 100 and %d", len(report.Details), len(report.Highlights), startupNoncriticalHighlightLimit)
	}
	for _, note := range report.Highlights {
		if !strings.Contains(note.text, "policy-") {
			t.Fatalf("ordinary error displaced a policy warning: %#v", report.Highlights)
		}
		if len([]rune(note.text)) > startupHighlightMaxRunes {
			t.Fatalf("highlight is unbounded: %d runes", len([]rune(note.text)))
		}
	}
	if joined := strings.Join(report.Summary, "\n"); !strings.Contains(joined, "95 more in /doctor extensions") {
		t.Fatalf("summary omitted bounded overflow: %s", joined)
	}
}

func TestAggregateStartupNotesDeduplicatesButKeepsTrustWarningVisible(t *testing.T) {
	notes := []mcpNote{
		{"warn", "plugins: executable stays off until its bytes are trusted (/tmp/one/plugin.json)"},
		{"warning", "PLUGINS:   executable stays off until its bytes are trusted (/tmp/two/plugin.json)."},
	}
	report := aggregateStartupNotes(notes)
	if len(report.Details) != 2 {
		t.Fatalf("details = %#v, want both exact diagnostics", report.Details)
	}
	if len(report.Highlights) != 1 || !strings.Contains(report.Highlights[0].text, "trusted") {
		t.Fatalf("trust warning was hidden or repeated: %#v", report.Highlights)
	}
	if got := report.Groups[1]; got.Unique != 1 || got.Total != 2 {
		t.Fatalf("trust warning group = %#v, want one semantic issue from two reports", got)
	}
}

func TestAggregateStartupNotesEmptyReportHasStableGroups(t *testing.T) {
	report := aggregateStartupNotes(nil)
	if len(report.Summary) != 0 || len(report.Highlights) != 0 || len(report.Details) != 0 {
		t.Fatalf("empty input produced visible output: %#v", report)
	}
	if len(report.Groups) != len(startupNoteCategoryOrder) {
		t.Fatalf("empty report omitted categories: %#v", report.Groups)
	}
	for i, group := range report.Groups {
		if group.Category != startupNoteCategoryOrder[i] || group.Unique != 0 || group.Total != 0 {
			t.Fatalf("empty group %d = %#v", i, group)
		}
	}
}

func assertBoundedStartupSummary(t *testing.T, lines []string) {
	t.Helper()
	if len(lines) > startupSummaryMaxLines {
		t.Fatalf("summary uses %d lines, max is %d: %q", len(lines), startupSummaryMaxLines, lines)
	}
	for _, line := range lines {
		if len(line) > startupSummaryMaxColumns {
			t.Fatalf("summary line uses %d columns, max is %d: %q", len(line), startupSummaryMaxColumns, line)
		}
	}
}

func startupNoteText(notes []mcpNote) string {
	var b strings.Builder
	for _, note := range notes {
		b.WriteString(note.level)
		b.WriteByte(':')
		b.WriteString(note.text)
		b.WriteByte('\n')
	}
	return b.String()
}
