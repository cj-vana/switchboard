package main

import (
	"context"
	"io"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/switchboard-code/switchboard/internal/agent"
	"github.com/switchboard-code/switchboard/internal/catalog"
	"github.com/switchboard-code/switchboard/internal/config"
	"github.com/switchboard-code/switchboard/internal/provider"
)

type blockingSummaryProvider struct {
	started chan struct{}
	release chan struct{}
}

func (*blockingSummaryProvider) Name() string { return "blocking-summary" }
func (p *blockingSummaryProvider) Stream(context.Context, provider.RouteTarget, provider.Request) (provider.EventStream, error) {
	return &blockingSummaryStream{started: p.started, release: p.release}, nil
}
func (*blockingSummaryProvider) CountTokens(context.Context, provider.RouteTarget, provider.Request) (provider.TokenEstimate, error) {
	return provider.TokenEstimate{}, nil
}
func (*blockingSummaryProvider) Probe(context.Context, provider.RouteTarget) (provider.ProbeResult, error) {
	return provider.ProbeResult{Reachable: true, ModelPresent: true}, nil
}

type blockingSummaryStream struct {
	started chan struct{}
	release chan struct{}
	step    int
}

func (s *blockingSummaryStream) Next() (provider.Event, error) {
	switch s.step {
	case 0:
		s.step++
		close(s.started)
		<-s.release
		return provider.Event{Type: provider.EventTextDelta, Text: "Reusable procedure.\n\nFollow the established steps."}, nil
	case 1:
		s.step++
		return provider.Event{Type: provider.EventDone, StopReason: provider.StopEndTurn}, nil
	default:
		return provider.Event{}, io.EOF
	}
}

func (*blockingSummaryStream) Close() error { return nil }

func TestShouldAutoCompactTriggersAtTheThreshold(t *testing.T) {
	m := testModel(t)
	m.app.config.CompactAuto = true
	m.app.config.CompactAtPercent = 85
	m.ctxWindow = 100_000

	cases := []struct {
		callTokens int
		want       bool
		why        string
	}{
		{84_000, false, "below the threshold"},
		{85_000, true, "at the threshold"},
		{99_000, true, "nearly full"},
		{0, false, "no usage observed yet"},
	}
	for _, c := range cases {
		m.callTokens = c.callTokens
		if got := m.shouldAutoCompact(); got != c.want {
			t.Errorf("%s (%d of %d): got %v, want %v", c.why, c.callTokens, m.ctxWindow, got, c.want)
		}
	}

	m.callTokens = 99_000
	m.app.config.CompactAuto = false
	if m.shouldAutoCompact() {
		t.Error("auto off must mean off, however full the window")
	}

	m.app.config.CompactAuto = true
	m.ctxWindow = 0
	if m.shouldAutoCompact() {
		t.Error("a target with no known window has no threshold to cross")
	}
}

func TestCompactSettingsPersist(t *testing.T) {
	m := testModel(t)
	m.app.config.Path = filepath.Join(t.TempDir(), config.FileName)
	m.app.config.CompactAuto = true

	if cmd, handled := compactSettings(m, "auto off"); !handled {
		t.Fatal("auto off was not handled as a setting")
	} else if n := cmd().(noticeMsg); n.level == "error" {
		t.Fatalf("auto off failed: %s", n.text)
	}
	if cmd, handled := compactSettings(m, "at 70"); !handled {
		t.Fatal("at 70 was not handled as a setting")
	} else if n := cmd().(noticeMsg); n.level == "error" {
		t.Fatalf("at 70 failed: %s", n.text)
	}

	saved, err := config.LoadFile(m.app.config.Path)
	if err != nil {
		t.Fatal(err)
	}
	if saved.CompactAuto {
		t.Error("auto off did not survive the rewrite")
	}
	if saved.CompactAtPercent != 70 {
		t.Errorf("threshold %d did not survive, want 70", saved.CompactAtPercent)
	}

	if cmd, handled := compactSettings(m, "at 30"); !handled || !strings.Contains(cmd().(noticeMsg).text, "usage") {
		t.Error("a threshold outside 50–95 must be refused with usage")
	}

	// Guidance is not a setting: it flows through to the summarizer.
	if _, handled := compactSettings(m, "focus on the migration"); handled {
		t.Error("guidance text was swallowed by the settings parser")
	}
}

func TestSummarizerSlotResolution(t *testing.T) {
	m := testModel(t)
	app := m.app

	// No slot: the current tier does its own summarizing.
	tier, fromSlot, err := summarizerFor(app)
	if err != nil || fromSlot || tier.ID != app.tier.ID {
		t.Fatalf("no slot should mean the current tier: %+v fromSlot=%v err=%v", tier, fromSlot, err)
	}

	// A tier alias resolves through the ladder.
	app.config.Slots = map[string]string{"summarizer": "t1"}
	tier, fromSlot, err = summarizerFor(app)
	if err != nil || !fromSlot || tier.ID != "t1" {
		t.Fatalf("alias t1 did not resolve: %+v fromSlot=%v err=%v", tier, fromSlot, err)
	}

	// A direct reference builds an ad-hoc tier.
	app.config.Slots["summarizer"] = "kimi/kimi-for-coding-highspeed"
	tier, fromSlot, err = summarizerFor(app)
	if err != nil || !fromSlot || tier.Target.Provider != "kimi" || tier.Target.ModelID != "kimi-for-coding-highspeed" {
		t.Fatalf("direct ref did not resolve: %+v err=%v", tier, err)
	}

	// A reference that would not load must not summarize either.
	app.config.Slots["summarizer"] = "not-a-target"
	if _, _, err = summarizerFor(app); err == nil {
		t.Fatal("an unparseable slot should be an error, not a silent fallback")
	}
}

// A manual /compact against an unreachable summarizer slot refuses and leaves
// the session alone; the user asked for the slot's quality, not whatever is
// nearest.
func TestManualCompactRefusesUnreachableSummarizer(t *testing.T) {
	m := testModel(t)
	m.app.providers = newProviders("http://127.0.0.1:1", m.app.config)
	m.app.config.Slots = map[string]string{"summarizer": "ollama/absent-model"}
	if err := m.app.loop.Session.AppendMessage(provider.UserText("hello")); err != nil {
		t.Fatal(err)
	}

	cmd := compactCmd(m, "", false)
	if cmd == nil {
		t.Fatal("compact produced no command")
	}
	msg, ok := cmd().(noticeMsg)
	if !ok || msg.level != "error" {
		t.Fatalf("expected an error notice, got %#v", msg)
	}
	if !strings.Contains(msg.text, "session unchanged") {
		t.Fatalf("the refusal must say the session is intact: %q", msg.text)
	}
}

func TestCompactOwnsSessionUntilSwapAndQueuesPrompts(t *testing.T) {
	m := testModel(t)
	appendTurn(t, m, "question", "answer")
	sourceID := m.app.loop.Session.ID()
	client := &blockingSummaryProvider{started: make(chan struct{}), release: make(chan struct{})}
	released := false
	defer func() {
		if !released {
			close(client.release)
		}
	}()
	m.app.budget = &budgetState{}
	m.app.loop.Bind(agent.Binding{Provider: client, Target: m.app.tier.Target, Cache: m.app.loop.Binding().Cache})

	cmd := compactCmd(m, "", false)
	if cmd == nil || !m.busy || !m.operationActive {
		t.Fatalf("compact did not claim the session before launch: cmd=%v busy=%v operation=%v", cmd != nil, m.busy, m.operationActive)
	}
	result := make(chan tea.Msg, 1)
	go func() { result <- cmd() }()
	select {
	case <-client.started:
	case <-time.After(5 * time.Second):
		t.Fatal("compact provider did not start")
	}

	if next := m.enqueue("queued while compacting", ""); next != nil || len(m.queue) != 1 {
		t.Fatalf("prompt crossed the compact barrier: cmd=%v queue=%v", next != nil, m.queue)
	}
	if overlap := cmdFork(m, ""); overlap == nil {
		t.Fatal("overlapping fork returned nothing")
	} else if notice, ok := overlap().(noticeMsg); !ok || !strings.Contains(notice.text, "already running") {
		t.Fatalf("overlapping fork was not refused: %#v", notice)
	}

	close(client.release)
	released = true
	var swap sessionSwapMsg
	select {
	case got := <-result:
		var ok bool
		swap, ok = got.(sessionSwapMsg)
		if !ok || swap.err != nil {
			t.Fatalf("compact result = %#v", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("compact did not finish")
	}
	defer swap.sess.Close()

	next := m.onSessionSwap(swap)
	if m.app.loop.Session.ID() == sourceID {
		t.Fatal("successful compact did not install its session")
	}
	if next == nil || !m.busy || !m.turnPlanning || len(m.queue) != 0 {
		t.Fatalf("queued prompt did not start after, and only after, swap: next=%v busy=%v planning=%v queue=%v",
			next != nil, m.busy, m.turnPlanning, m.queue)
	}
}

func TestCancelledLearnKeepsForkBlockedUntilBudgetAttemptSettles(t *testing.T) {
	m := testModel(t)
	appendTurn(t, m, "question", "answer")
	m.app.workspace = t.TempDir()
	cat, err := catalog.LoadBundled()
	if err != nil {
		t.Fatal(err)
	}
	var paid catalog.ModelInfo
	for _, candidate := range cat.Entries() {
		if candidate.Metering.String() == catalog.PerToken.String() && !candidate.Free() && candidate.MaxOutput > 0 {
			paid = candidate
			break
		}
	}
	if paid.Provider == "" {
		t.Fatal("bundled catalog has no paid per-token model")
	}
	target := provider.RouteTarget{Provider: paid.Provider, Surface: paid.Surface, ModelID: paid.ProviderModelID}
	client := &blockingSummaryProvider{started: make(chan struct{}), release: make(chan struct{})}
	released := false
	defer func() {
		if !released {
			close(client.release)
		}
	}()
	m.app.catalog = cat
	m.app.budget = &budgetState{}
	m.app.tier.Target = target
	m.app.loop.Bind(agent.Binding{Provider: client, Target: target, Cache: nil})

	cmd := cmdLearn(m, "ledger-barrier-test")
	if cmd == nil || !m.operationActive || !m.busy {
		t.Fatal("learn did not claim exclusive ownership")
	}
	result := make(chan tea.Msg, 1)
	go func() { result <- cmd() }()
	select {
	case <-client.started:
	case <-time.After(5 * time.Second):
		t.Fatal("learn provider did not start")
	}
	if reserve := m.app.loop.Session.State().RetryReserveMicroUSD; reserve <= 0 {
		t.Fatalf("provider started without a durable pending attempt: reserve=%d", reserve)
	}

	m.interrupt()
	if !m.operationActive || !m.operationCancelling || !m.busy {
		t.Fatal("escape released learn before its metered call settled")
	}
	if fork := cmdFork(m, ""); fork == nil {
		t.Fatal("fork returned nothing while learn was cancelling")
	} else if notice, ok := fork().(noticeMsg); !ok || !strings.Contains(notice.text, "already running") {
		t.Fatalf("fork crossed a cancelling learn: %#v", notice)
	}

	close(client.release)
	released = true
	var completion noticeMsg
	select {
	case got := <-result:
		var ok bool
		completion, ok = got.(noticeMsg)
		if !ok {
			t.Fatalf("learn completion = %#v", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("learn did not settle after provider completion")
	}
	if reserve := m.app.loop.Session.State().RetryReserveMicroUSD; reserve != 0 {
		t.Fatalf("successful learn left retry reserve %d", reserve)
	}
	m.Update(completion)
	if m.operationActive || m.busy {
		t.Fatal("learn did not release ownership after settlement")
	}
}

// The preview says what compaction would do before it does it: the
// conversation is what a summary replaces, the frozen zone rides
// unchanged, and the alternative is named.
func TestCompactPreviewStatesTheTrade(t *testing.T) {
	m := testModel(t)
	m.app.loop.Session.AppendMessage(provider.UserText("a prompt long enough to count"))
	cmdCompact(m, "preview")
	joined := strings.Join(m.tr.flat, "\n")
	for _, want := range []string{"would summarize 1 messages", "ride unchanged", "/fork"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("preview missing %q:\n%s", want, joined)
		}
	}
	if len(m.app.loop.Session.State().Messages) != 1 {
		t.Fatal("a preview must not touch the session")
	}
}
