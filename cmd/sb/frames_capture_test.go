package main

// Frame capture: renders representative TUI frames through the real view
// code and writes them as ANSI text, one file per frame, into the directory
// named by SB_FRAMES. Skipped otherwise. This exists so the TUI's look can
// be reviewed (and docs/tui.svg regenerated) from the actual renderer
// rather than a hand-kept mock.
//
//	SB_FRAMES=/tmp/frames go test ./cmd/sb/ -run TestCaptureFrames

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"

	"github.com/cj-vana/switchboard/internal/agent"
	"github.com/cj-vana/switchboard/internal/catalog"
	"github.com/cj-vana/switchboard/internal/config"
	"github.com/cj-vana/switchboard/internal/execution"
	"github.com/cj-vana/switchboard/internal/permission"
	"github.com/cj-vana/switchboard/internal/provider"
	"github.com/cj-vana/switchboard/internal/session"
	"github.com/cj-vana/switchboard/internal/tools"
)

func captureModel(t *testing.T, th *theme) *tuiModel {
	t.Helper()
	store, err := session.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	tiers := []config.Tier{
		{ID: "t1", Label: "light", Target: provider.RouteTarget{Provider: "ollama", Surface: "local", ModelID: "qwen3.5:9b-mlx"}},
		{ID: "t2", Label: "deep", Target: provider.RouteTarget{Provider: "ollama", Surface: "local", ModelID: "qwen3.8:27b-mlx"}},
		{ID: "t3", Label: "kimi", Target: provider.RouteTarget{Provider: "kimi", Surface: "coding", ModelID: "kimi-for-coding-highspeed"}},
		{ID: "t4", Label: "codex", Target: provider.RouteTarget{Provider: "openai", Surface: "subscription", ModelID: "gpt-5.6-sol"}},
	}
	sess, err := store.Create(t.TempDir(), tiers[0].Target.ID(), "test")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { sess.Close() })

	cat, err := catalog.LoadBundled()
	if err != nil {
		t.Fatal(err)
	}
	registry, err := tools.NewRegistry(t.TempDir(), execution.Capability{})
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{Tiers: tiers}
	loop := &agent.Loop{
		Session: sess,
		Target:  tiers[0].Target,
		Tools:   registry,
		Perms:   permission.NewEngine(permission.ModeAcceptEdits, execution.Capability{}),
	}
	app := &tuiApp{
		loop:      loop,
		store:     store,
		config:    cfg,
		catalog:   cat,
		tier:      cfg.Tiers[0],
		workspace: "~/code/switchboard",
	}
	m := newTUIModel(app, th, newMarkdown(100, th.dark), newTextarea())
	m.Update(tea.WindowSizeMsg{Width: 100, Height: 34})
	m.mode = permission.ModeAcceptEdits
	return m
}

// playSession fills the transcript with a representative mid-task moment:
// work started on t1, failed twice, escalated to t3, and is finishing there.
func playSession(m *tuiModel) {
	sess := m.app.loop.Session
	m.addBanner(sess, false)
	m.tr.add(&entry{kind: kindUser, rank: -1,
		text: "the runner test is flaky under -count=20; make it deterministic and explain the race"})
	m.tr.add(&entry{kind: kindThinking, rank: 0,
		text: "The failure is a wait on the process group racing the reaper; reading the runner first."})
	m.tr.add(&entry{kind: kindTool, rank: 0, tool: toolEntry{
		name: "read", desc: "internal/execution/runner_test.go", done: true, took: 12 * time.Millisecond}})
	m.tr.add(&entry{kind: kindTool, rank: 0, tool: toolEntry{
		name: "exec", desc: "go test ./internal/execution/ -run TestRunner -count=20", done: true,
		failed: true, took: 9200 * time.Millisecond,
		detail: "--- FAIL: TestRunnerKillsDescendants (0.41s)\n    runner_test.go:88: descendant survived the kill\nFAIL"}})
	m.tr.add(&entry{kind: kindRoute, rank: 2,
		routeSummary: "t1 → t3 via detector (same failure signature twice)",
		routeLines:   []string{"trigger: repeated failure signature", "ruled out: t2 (same family as t1)", "upper bound fits: plan"}})
	m.tr.add(&entry{kind: kindTodo, rank: 2, todos: []tools.TodoItem{
		{Text: "reproduce the flake under -count=20", Status: tools.TodoDone},
		{Text: "close the wait/reap race in the runner", Status: tools.TodoActive},
		{Text: "rerun the suite and the race detector", Status: tools.TodoPending}}})
	m.tr.add(&entry{kind: kindTool, rank: 2, tool: toolEntry{
		name: "edit", desc: "internal/execution/runner.go", done: true, took: 40 * time.Millisecond,
		detail: "1 replacement"}})
	m.tr.add(&entry{kind: kindAssistant, rank: 2, text: "The race: `Wait` returns when the direct child exits, " +
		"but the kill walks the process group afterwards, so a grandchild can outlive the assertion.\n\n" +
		"- `runner.go` now reaps the group before `Wait` returns\n" +
		"- the test polls process *state*, not the pid table\n\nRunning the suite again to confirm."})
	m.tr.add(&entry{kind: kindNotice, level: "watch", rank: -1, text: "watch: go test ./internal/execution/ went green"})

	m.app.tier = m.app.config.Tiers[2]
	m.app.loop.Target = m.app.tier.Target
	m.tierLine = m.app.tierLine()
	m.costLine = "plan"
	m.ctxWindow = 262144
	m.callTokens = 89000
	m.moves = []int{2}
	m.sessionAt = time.Now().Add(-12 * time.Minute)
	m.samples = []float64{12, 18, 31, 44, 39, 52, 61, 48, 55, 42}
	m.tr.scrollToBottom()
}

func writeFrame(t *testing.T, dir, name, frame string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name+".ans"), []byte(frame), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestCaptureFrames(t *testing.T) {
	dir := os.Getenv("SB_FRAMES")
	if dir == "" {
		t.Skip("SB_FRAMES not set")
	}
	lipgloss.SetColorProfile(termenv.ANSI256)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}

	// Opening frame: the banner stating the ladder, idle composer, status bar.
	m := captureModel(t, darkTheme())
	m.mode = permission.ModeDefault
	m.addBanner(m.app.loop.Session, false)
	writeFrame(t, dir, "opening", m.View())

	// Mid-session, busy: the full transcript vocabulary plus the working line.
	m = captureModel(t, darkTheme())
	playSession(m)
	m.busy = true
	m.started = time.Now().Add(-14 * time.Second)
	writeFrame(t, dir, "session", m.View())

	// The same moment at rest with the command menu open.
	m.busy = false
	m.ta.SetValue("/re")
	writeFrame(t, dir, "suggestions", m.View())

	// A permission ask, sandbox absent: the amber frame.
	m.ta.SetValue("")
	m.dlg = newPermissionDialog(
		permission.Request{Tool: "exec", Effect: permission.EffectExecute,
			Argv: []string{"go", "test", "./internal/execution/", "-count=20"}},
		permission.Outcome{SandboxAbsent: true},
		make(chan permission.Response, 1))
	writeFrame(t, dir, "permission", m.View())

	// Light theme, same session.
	lm := captureModel(t, lightTheme())
	playSession(lm)
	writeFrame(t, dir, "session-light", lm.View())

	joined := strings.Join([]string{m.View()}, "")
	if joined == "" {
		t.Fatal("empty frame")
	}
}
