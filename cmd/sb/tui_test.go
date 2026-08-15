package main

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/cj-vana/switchboard/internal/agent"
	"github.com/cj-vana/switchboard/internal/catalog"
	"github.com/cj-vana/switchboard/internal/config"
	"github.com/cj-vana/switchboard/internal/execution"
	"github.com/cj-vana/switchboard/internal/permission"
	"github.com/cj-vana/switchboard/internal/provider"
	"github.com/cj-vana/switchboard/internal/session"
)

func testModel(t *testing.T) *tuiModel {
	t.Helper()
	store, err := session.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	target := provider.RouteTarget{Provider: "ollama", Surface: "local", ModelID: "test:7b"}
	sess, err := store.Create(t.TempDir(), target.ID(), "test")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { sess.Close() })

	cfg := &config.Config{Tiers: []config.Tier{{ID: "t1", Label: "light", Target: target}}}
	loop := &agent.Loop{
		Session: sess,
		Target:  target,
		Perms:   permission.NewEngine(permission.ModeDefault, execution.Capability{}),
	}
	app := &tuiApp{
		loop:      loop,
		config:    cfg,
		catalog:   &catalog.Catalog{Revision: "test"},
		tier:      cfg.Tiers[0],
		workspace: "/tmp/ws",
	}
	m := newTUIModel(app, darkTheme(), newMarkdown(80, true), newTextarea())
	m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	return m
}

// The status line is the product's always-on readout: routing visible at rest.
func TestStatusLineShowsRouteAtRest(t *testing.T) {
	m := testModel(t)
	view := m.View()
	for _, want := range []string{"t1 light", "ollama/local/test:7b", "default", "unpriced"} {
		if !strings.Contains(view, want) {
			t.Errorf("status line missing %q:\n%s", want, view)
		}
	}
}

func TestSlashSuggestionsAppear(t *testing.T) {
	m := testModel(t)
	m.ta.SetValue("/he")
	if !m.suggestionsVisible() {
		t.Fatal("typing /he showed no suggestions")
	}
	if view := m.suggestionsView(); !strings.Contains(view, "/help") {
		t.Fatalf("suggestions missing /help:\n%s", view)
	}
}

func TestHelpCommandRenders(t *testing.T) {
	m := testModel(t)
	m.ta.SetValue("/help")
	m.submit()
	joined := strings.Join(m.tr.flat, "\n")
	if !strings.Contains(joined, "commands") || !strings.Contains(joined, "/resume") {
		t.Fatalf("/help did not land in the transcript:\n%s", joined)
	}
}

func TestUnknownCommandIsANotice(t *testing.T) {
	m := testModel(t)
	m.ta.SetValue("/frobnicate")
	if cmd := m.submit(); cmd != nil {
		if msg := cmd(); msg != nil {
			m.Update(msg)
		}
	}
	last := m.tr.last()
	if last == nil || last.kind != kindNotice || last.level != "error" {
		t.Fatalf("unknown command did not produce an error notice: %+v", last)
	}
}

func TestModeCycleMovesAndReports(t *testing.T) {
	m := testModel(t)
	m.cycleMode()
	if m.mode != permission.ModeAcceptEdits {
		t.Fatalf("shift+tab moved to %s, want acceptEdits", m.mode)
	}
	if m.app.loop.Perms.Mode() != permission.ModeAcceptEdits {
		t.Fatal("the engine was not updated")
	}
}

func testTranscript(t *testing.T, width int) *transcript {
	t.Helper()
	return newTranscript(width, darkTheme(), newMarkdown(width, true))
}

func TestTranscriptRendersAndScrolls(t *testing.T) {
	tr := testTranscript(t, 80)
	tr.add(&entry{kind: kindUser, text: "hello"})
	e := tr.add(&entry{kind: kindAssistant, text: "", live: true})
	tr.appendText(tr.indexOf(e), "world")
	tr.finalize(e)

	if len(tr.flat) == 0 {
		t.Fatal("nothing rendered")
	}
	joined := strings.Join(tr.flat, "\n")
	if !strings.Contains(joined, "hello") || !strings.Contains(joined, "world") {
		t.Fatalf("transcript lost content:\n%s", joined)
	}

	for i := 0; i < 50; i++ {
		tr.add(&entry{kind: kindInfo, text: "line"})
	}
	view := tr.view(10)
	if got := strings.Count(view, "\n") + 1; got != 10 {
		t.Fatalf("view height = %d, want 10", got)
	}
	tr.scrollBy(5)
	if tr.offset != 5 {
		t.Fatalf("offset = %d, want 5", tr.offset)
	}
	tr.scrollBy(-100)
	if tr.offset != 0 {
		t.Fatalf("offset below bottom = %d", tr.offset)
	}
}

func TestCompletedEntriesRenderOncePerWidth(t *testing.T) {
	tr := testTranscript(t, 80)
	e := tr.add(&entry{kind: kindAssistant, text: "some **bold** text"})
	if _, ok := e.cache[80]; !ok {
		t.Fatal("completed entry was not cached at its width")
	}
	// A width change re-renders, but returning to a seen width is a cache hit.
	tr.setWidth(40)
	tr.setWidth(80)
	if _, ok := e.cache[80]; !ok {
		t.Fatal("per-width cache did not survive a resize round trip")
	}
}

func TestStreamingEntryNeverTouchesTheCache(t *testing.T) {
	tr := testTranscript(t, 80)
	e := tr.add(&entry{kind: kindAssistant, live: true})
	tr.appendText(tr.indexOf(e), "in flight")
	if len(e.cache) != 0 {
		t.Fatal("a streaming entry was cached; the fast path is not the fast path")
	}
}

func TestToolEntryCollapseAndExpand(t *testing.T) {
	tr := testTranscript(t, 80)
	e := tr.add(&entry{kind: kindTool, tool: toolEntry{name: "exec", desc: "go test ./..."}})
	if got := tr.render(e); len(got) != 1 {
		t.Fatalf("running tool rendered %d lines, want 1", len(got))
	}
	e.tool.done = true
	e.tool.detail = "ok  github.com/example/pkg"
	e.cache = nil
	if got := tr.render(e); len(got) != 2 {
		t.Fatalf("collapsed tool rendered %d lines, want 2:\n%s", len(got), strings.Join(got, "\n"))
	}
	e.tool.detail = strings.Repeat("line\n", 100)
	e.expanded = true
	e.cache = nil
	if got := tr.render(e); len(got) <= 2 {
		t.Fatal("expanded tool did not show its detail")
	}
}

func TestRouteEntryCollapsesToOneLine(t *testing.T) {
	tr := testTranscript(t, 80)
	e := tr.add(&entry{kind: kindRoute, routeSummary: "t2 via heuristic (test)", routeLines: []string{"route t2", "estimate $0.01"}})
	if got := tr.render(e); len(got) != 1 {
		t.Fatalf("collapsed route rendered %d lines, want 1", len(got))
	}
	e.expanded = true
	e.cache = nil
	if got := tr.render(e); len(got) <= 1 {
		t.Fatal("expanded route did not show the decision record")
	}
}

func TestMatchingCommandsIncludesTiers(t *testing.T) {
	cfg := &config.Config{Tiers: []config.Tier{{ID: "t9", Label: "deep"}}}
	matches := matchingCommands("t", cfg)
	found := false
	for _, m := range matches {
		if m.name == "t9" {
			found = true
		}
	}
	if !found {
		t.Fatal("tier entries missing from suggestions")
	}
	matches = matchingCommands("exi", cfg)
	if len(matches) != 1 || matches[0].name != "exit" {
		t.Fatalf("prefix matching is off: %+v", matches)
	}
}

func TestNewerVersion(t *testing.T) {
	cases := []struct {
		candidate, current string
		want               bool
	}{
		{"v0.3.0", "v0.2.9", true},
		{"0.2.9", "v0.2.9", false},
		{"v1.0.0", "v0.99.99", true},
		{"v0.2.9-rc1", "v0.2.9", false},
		{"garbage", "v0.2.9", false},
		// The beta channel's whole path: beta.1 → beta.2 → release, each a
		// real upgrade, none repeating.
		{"v0.4.0-beta.2", "v0.4.0-beta.1", true},
		{"v0.4.0", "v0.4.0-beta.2", true},
		{"v0.4.0-beta.1", "v0.4.0-beta.1", false},
		{"v0.4.0-beta.1", "v0.4.0", false},
		{"v0.4.0-beta.10", "v0.4.0-beta.9", true},
		{"v0.4.0-rc.1", "v0.4.0-beta.2", true},
	}
	for _, c := range cases {
		if got := newerVersion(c.candidate, c.current); got != c.want {
			t.Errorf("newerVersion(%q, %q) = %v, want %v", c.candidate, c.current, got, c.want)
		}
	}
}

func TestChecksumFor(t *testing.T) {
	sums := []byte("abc123  sb_0.3.0_darwin_arm64.tar.gz\ndef456 *sb_0.3.0_linux_amd64.tar.gz\n")
	got, err := checksumFor(sums, "sb_0.3.0_darwin_arm64.tar.gz")
	if err != nil || got != "abc123" {
		t.Fatalf("got %q, %v", got, err)
	}
	got, err = checksumFor(sums, "sb_0.3.0_linux_amd64.tar.gz")
	if err != nil || got != "def456" {
		t.Fatalf("binary-mode entry: got %q, %v", got, err)
	}
	if _, err := checksumFor(sums, "missing"); err == nil {
		t.Fatal("a missing asset must be an error, not an empty checksum")
	}
}

func TestCompact(t *testing.T) {
	if got := compact(999); got != "999" {
		t.Errorf("compact(999) = %s", got)
	}
	if got := compact(1500); got != "1.5k" {
		t.Errorf("compact(1500) = %s", got)
	}
	if got := compact(2_500_000); got != "2.5M" {
		t.Errorf("compact(2.5M) = %s", got)
	}
}
