package main

import (
	"os"

	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"
	"strings"
	"testing"
	"time"

	"github.com/cj-vana/switchboard/internal/config"
	"github.com/cj-vana/switchboard/internal/provider"
)

// TestRenderPreview writes a fully composed frame to SB_PREVIEW so the design
// can be looked at instead of imagined: banner, a turn on t1, an escalation
// to t3, tools on both rungs, advice, and the status line. Not an assertion
// test; it exists because a TUI redesign reviewed only through its code is a
// redesign nobody has seen.
func TestRenderPreview(t *testing.T) {
	out := os.Getenv("SB_PREVIEW")
	if out == "" {
		t.Skip("set SB_PREVIEW=/path/to/file to write a rendered frame")
	}
	// Tests have no TTY, so lipgloss would silently strip every color and
	// the preview would prove nothing about the one thing it exists to show.
	lipgloss.SetColorProfile(termenv.ANSI256)

	m := testModel(t)
	m.app.config.Tiers = []config.Tier{
		{ID: "t1", Label: "light", Target: provider.RouteTarget{Provider: "ollama", Surface: "local", ModelID: "qwen3.5:9b-mlx"}},
		{ID: "t2", Label: "deep", Target: provider.RouteTarget{Provider: "ollama", Surface: "local", ModelID: "qwen3.8:27b-mlx"}},
		{ID: "t3", Label: "kimi", Target: provider.RouteTarget{Provider: "kimi", Surface: "coding", ModelID: "kimi-for-coding-highspeed"}},
		{ID: "t4", Label: "codex", Target: provider.RouteTarget{Provider: "openai", Surface: "subscription", ModelID: "gpt-5.6-sol"}},
	}
	m.app.tier = m.app.config.Tiers[0]

	m.addBanner(m.app.loop.Session, false)

	m.tr.add(&entry{kind: kindUser, text: "fix the failing test in internal/config and explain what broke"})
	m.tr.add(&entry{kind: kindInfo, text: ""})
	m.tr.add(&entry{kind: kindRoute, routeSummary: "t1 via heuristic (short prompt, tools needed)", rank: 0})
	m.tr.add(&entry{kind: kindTool, tool: toolEntry{name: "exec", desc: "go test ./internal/config/", done: true, failed: true, took: 3 * time.Second, detail: "FAIL: TestSaveRoundTrips (0.01s)"}, rank: 0})
	m.tr.add(&entry{kind: kindNotice, level: "route", text: "escalated: two tool errors in a row"})
	m.tr.add(&entry{kind: kindRoute, routeSummary: "moved to t3 (error spike)", rank: 2})
	m.tr.add(&entry{kind: kindTool, tool: toolEntry{name: "edit", desc: "internal/config/save.go", done: true, took: time.Second, detail: "1 replacement"}, rank: 2})
	m.tr.add(&entry{kind: kindNotice, level: "advisor", text: "The failure is the omitempty tag, not the encoder; check the struct tags before rerunning the suite."})
	m.tr.add(&entry{kind: kindAssistant, text: "Fixed. The round-trip test broke because `updates.check` serialized explicitly; the tag now omits it."})
	m.tr.finalizeAll()

	m.app.tier = m.app.config.Tiers[2]
	m.callTokens, m.ctxWindow = 91_000, 262_144

	frame := m.tr.view(28) + "\n" + m.ta.View() + "\n" + m.statusLine()
	if err := os.WriteFile(out, []byte(frame), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Logf("wrote %d bytes, %d lines", len(frame), strings.Count(frame, "\n"))
}
