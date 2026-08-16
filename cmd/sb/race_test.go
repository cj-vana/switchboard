package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/cj-vana/switchboard/internal/agent"
	"github.com/cj-vana/switchboard/internal/catalog"
	"github.com/cj-vana/switchboard/internal/config"
	"github.com/cj-vana/switchboard/internal/execution"
	"github.com/cj-vana/switchboard/internal/permission"
	"github.com/cj-vana/switchboard/internal/provider"
	"github.com/cj-vana/switchboard/internal/session"
	"github.com/cj-vana/switchboard/internal/tools"
)

// racedProvider scripts model calls the way internal/agent's tests do: each
// turn is a canned event list, and running out of turns is an error rather
// than an invented answer.
type racedProvider struct {
	turns []racedTurn
	calls int
}

type racedTurn struct{ events []provider.Event }

func racedText(text string) racedTurn {
	return racedTurn{events: []provider.Event{
		{Type: provider.EventTextDelta, Index: 0, Text: text},
		{Type: provider.EventDone, StopReason: provider.StopEndTurn, Usage: provider.Usage{InputTokens: 10, OutputTokens: 5}},
	}}
}

func racedToolCall(name, input string) racedTurn {
	return racedTurn{events: []provider.Event{
		{Type: provider.EventToolUse, Index: 0, ToolUse: &provider.ToolUse{ID: "call_1", Name: name, Input: json.RawMessage(input)}},
		{Type: provider.EventDone, StopReason: provider.StopToolUse, Usage: provider.Usage{InputTokens: 10, OutputTokens: 5}},
	}}
}

func (p *racedProvider) Name() string { return "scripted" }
func (p *racedProvider) Stream(context.Context, provider.RouteTarget, provider.Request) (provider.EventStream, error) {
	if p.calls >= len(p.turns) {
		return nil, errors.New("scripted provider ran out of turns")
	}
	turn := p.turns[p.calls]
	p.calls++
	return &racedStream{events: turn.events}, nil
}
func (p *racedProvider) CountTokens(context.Context, provider.RouteTarget, provider.Request) (provider.TokenEstimate, error) {
	return provider.TokenEstimate{}, nil
}
func (p *racedProvider) Probe(context.Context, provider.RouteTarget) (provider.ProbeResult, error) {
	return provider.ProbeResult{Reachable: true, ModelPresent: true}, nil
}

type racedStream struct {
	events []provider.Event
	i      int
}

func (s *racedStream) Next() (provider.Event, error) {
	if s.i < len(s.events) {
		ev := s.events[s.i]
		s.i++
		return ev, nil
	}
	return provider.Event{}, io.EOF
}
func (s *racedStream) Close() error { return nil }

// raceModel is testModel with what a race additionally needs: a session
// store the arms can fork in, a second tier, and a seeded conversation so
// the fork has a prefix.
func raceModel(t *testing.T) *tuiModel {
	t.Helper()
	store, err := session.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	workspace := t.TempDir()
	targetA := provider.RouteTarget{Provider: "scripted", Surface: "local", ModelID: "small"}
	targetB := provider.RouteTarget{Provider: "scripted", Surface: "local", ModelID: "large"}
	sess, err := store.Create(workspace, targetA.ID(), "test")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { sess.Close() })
	if err := sess.AppendMessage(provider.UserText("earlier question")); err != nil {
		t.Fatal(err)
	}
	if err := sess.AppendMessage(provider.Message{Role: provider.RoleAssistant,
		Content: []provider.Block{provider.Text{Text: "earlier answer"}}}); err != nil {
		t.Fatal(err)
	}

	cfg := &config.Config{Tiers: []config.Tier{
		{ID: "t1", Label: "light", Target: targetA},
		{ID: "t2", Label: "deep", Target: targetB},
	}}
	registry, err := tools.NewRegistry(workspace, execution.Capability{})
	if err != nil {
		t.Fatal(err)
	}
	loop := &agent.Loop{
		Session: sess,
		Target:  targetA,
		Tools:   registry,
		Perms:   permission.NewEngine(permission.ModeDefault, execution.Capability{}),
		System:  []provider.Block{provider.Text{Text: "system under test"}},
	}
	app := &tuiApp{
		loop:      loop,
		store:     store,
		config:    cfg,
		catalog:   &catalog.Catalog{Revision: "test"},
		tier:      cfg.Tiers[0],
		workspace: workspace,
	}
	m := newTUIModel(app, darkTheme(), newMarkdown(80, true), newTextarea())
	m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	return m
}

func TestParseRaceArgs(t *testing.T) {
	m := raceModel(t)
	cases := []struct {
		args       string
		a, b       string
		prompt     string
		wantsError bool
	}{
		{args: "t2 fix the flaky test", a: "t1", b: "t2", prompt: "fix the flaky test"},
		{args: "t1 t2 fix the flaky test", a: "t1", b: "t2", prompt: "fix the flaky test"},
		{args: "t2 t1 which is better", a: "t2", b: "t1", prompt: "which is better"},
		{args: "t2", wantsError: true},
		{args: "t1 t2", wantsError: true},
		{args: "", wantsError: true},
		{args: "t9 do a thing", wantsError: true},
	}
	for _, c := range cases {
		a, b, prompt, err := parseRaceArgs(m.app, c.args)
		if c.wantsError {
			if err == nil {
				t.Errorf("parse %q: expected an error, got %s vs %s %q", c.args, a.ID, b.ID, prompt)
			}
			continue
		}
		if err != nil {
			t.Errorf("parse %q: %v", c.args, err)
			continue
		}
		if a.ID != c.a || b.ID != c.b || prompt != c.prompt {
			t.Errorf("parse %q: got %s vs %s %q, want %s vs %s %q", c.args, a.ID, b.ID, prompt, c.a, c.b, c.prompt)
		}
	}
}

// The read-only rule for arms is enforced by a plan-mode engine, not by the
// asker or the mode the session runs in: a write the model asks for is
// denied with the reason, whatever bytes the shared system prompt claims
// about the session's own mode.
func TestRaceArmForksThePrefixAndRefusesMutation(t *testing.T) {
	m := raceModel(t)
	before := m.app.loop.Session.State()

	client := &racedProvider{turns: []racedTurn{
		racedToolCall("write", `{"path":"landed.txt","content":"from the race"}`),
		racedText("could not write, answering anyway"),
	}}
	arm, err := assembleRaceArm(m.app, m.app.config.Tiers[1], client, agent.NopObserver{})
	if err != nil {
		t.Fatal(err)
	}
	defer arm.sess.Close()

	if got := arm.sess.State().Target; got != string(m.app.config.Tiers[1].Target.ID()) {
		t.Errorf("arm session recorded target %q, want the rung under trial", got)
	}
	if got := len(arm.sess.State().Messages); got != len(before.Messages) {
		t.Fatalf("arm holds %d messages, want the full prefix of %d", got, len(before.Messages))
	}

	if err := arm.loop.TurnMessage(context.Background(), provider.UserText("try to write a file")); err != nil {
		t.Fatalf("arm turn failed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(m.app.workspace, "landed.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Error("a race arm wrote to the workspace")
	}
	var denial string
	for _, msg := range arm.sess.State().Messages {
		for _, b := range msg.Content {
			if res, ok := b.(provider.ToolResult); ok {
				denial = res.Content
			}
		}
	}
	if !strings.Contains(denial, "read-only") {
		t.Errorf("the model was not told why the write was refused: %q", denial)
	}
	if got := len(m.app.loop.Session.State().Messages); got != len(before.Messages) {
		t.Errorf("the race arm's turn reached the primary session: %d messages, was %d", got, len(before.Messages))
	}
}

// The concurrent path, end to end: two arms actually racing on their own
// goroutines, events arriving as messages, both rails resolving, and the
// verdict dialog opening with the full vocabulary. The channel is buffered
// and the test body is its only reader; the deadline turns a wiring
// mistake into seconds, not a hung suite.
func TestRaceRunsBothArmsToTheVerdict(t *testing.T) {
	m := raceModel(t)
	msgs := make(chan tea.Msg, 64)
	send := func(v tea.Msg) { msgs <- v }

	armA, err := assembleRaceArm(m.app, m.app.config.Tiers[0],
		&racedProvider{turns: []racedTurn{racedText("answer from the light rung")}},
		&raceObserver{arm: 0, send: send})
	if err != nil {
		t.Fatal(err)
	}
	armB, err := assembleRaceArm(m.app, m.app.config.Tiers[1],
		&racedProvider{turns: []racedTurn{racedText("answer from the deep rung")}},
		&raceObserver{arm: 1, send: send})
	if err != nil {
		t.Fatal(err)
	}
	run := &raceRun{typed: "which is better", arms: [2]*raceArm{armA, armB}, send: send}
	run.labels = [2]string{"a · t1 light", "b · t2 deep"}
	run.rails[0] = m.tr.add(&entry{kind: kindInfo, text: run.railLine(0)})
	run.rails[1] = m.tr.add(&entry{kind: kindInfo, text: run.railLine(1)})
	m.race = run
	m.busy = true

	m.launchRace(run, provider.UserText("which is better"))
	deadline := time.After(10 * time.Second)
	for m.dlg == nil {
		select {
		case v := <-msgs:
			m.Update(v)
		case <-deadline:
			t.Fatal("the race did not reach a verdict in time")
		}
	}

	d, ok := m.dlg.(*raceDialog)
	if !ok {
		t.Fatalf("the verdict is a %T, want the race dialog", m.dlg)
	}
	if len(d.ids) != 4 {
		t.Errorf("two completed arms offer %d options, want keep-a, keep-b, tie, and neither", len(d.ids))
	}
	for _, rail := range run.rails {
		if strings.Contains(rail.text, "running") {
			t.Errorf("a finished arm's rail still says running: %q", rail.text)
		}
	}
	var transcript strings.Builder
	for _, e := range m.tr.entries {
		transcript.WriteString(e.text + "\n")
	}
	for _, want := range []string{"answer from the light rung", "answer from the deep rung"} {
		if !strings.Contains(transcript.String(), want) {
			t.Errorf("a finished answer never rendered: %q", want)
		}
	}

	// Resolve through the dialog itself, the way a keypress would.
	m.dlg = nil
	d.resolve("a")
	if m.app.tier.ID != "t1" {
		t.Errorf("keeping a landed on %s, want t1", m.app.tier.ID)
	}
	if m.busy || m.race != nil {
		t.Error("the verdict did not release the session")
	}
}

// The verdict is the product: the picked branch becomes the session, the
// record rides it, and the road not taken stays on disk, labelled.
func TestFinishRaceSwapsOntoTheWinnerAndRecords(t *testing.T) {
	m := raceModel(t)
	origPath := m.app.loop.Session.Path()

	armA, err := assembleRaceArm(m.app, m.app.config.Tiers[0], &racedProvider{}, agent.NopObserver{})
	if err != nil {
		t.Fatal(err)
	}
	armB, err := assembleRaceArm(m.app, m.app.config.Tiers[1], &racedProvider{}, agent.NopObserver{})
	if err != nil {
		t.Fatal(err)
	}
	armA.status, armB.status = "completed", "completed"
	run := &raceRun{typed: "the raced prompt", arms: [2]*raceArm{armA, armB}}
	m.race = run
	m.busy = true

	winnerPath := armB.sess.Path()
	loserPath := armA.sess.Path()
	// The swap applies inside finishRace, synchronously: the model must
	// already be on the winner when this returns, gap-free.
	m.finishRace(run, "b", "b")

	if m.busy || m.race != nil {
		t.Error("the race did not release the session")
	}
	if got := m.app.loop.Session.Path(); got != winnerPath {
		t.Errorf("session is %s, want the winning branch %s", got, winnerPath)
	}
	if m.app.tier.ID != "t2" {
		t.Errorf("active tier is %s, want the winning rung t2", m.app.tier.ID)
	}
	winnerLog, err := os.ReadFile(winnerPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(winnerLog), `"type":"race"`) {
		t.Error("the winning branch's log holds no race record")
	}
	if !strings.Contains(string(winnerLog), `"outcome":"b"`) {
		t.Error("the race record does not carry the verdict")
	}
	loserLog, err := os.ReadFile(loserPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(loserLog), "not kept") {
		t.Error("the losing branch's log does not say it lost")
	}
	if _, err := os.ReadFile(origPath); err != nil {
		t.Errorf("the pre-race session left the disk: %v", err)
	}
	if len(m.raceLog) == 0 || !strings.Contains(m.raceLog[0], "kept t2") {
		t.Errorf("/why has no race line: %v", m.raceLog)
	}
}

// A tie is a preference for the cheaper rung: both sufficed, so the ladder
// order — cheapest first — decides which branch carries on.
func TestRaceTieKeepsTheCheaperRung(t *testing.T) {
	m := raceModel(t)

	armA, err := assembleRaceArm(m.app, m.app.config.Tiers[1], &racedProvider{}, agent.NopObserver{})
	if err != nil {
		t.Fatal(err)
	}
	armB, err := assembleRaceArm(m.app, m.app.config.Tiers[0], &racedProvider{}, agent.NopObserver{})
	if err != nil {
		t.Fatal(err)
	}
	armA.status, armB.status = "completed", "completed"
	run := &raceRun{typed: "tie prompt", arms: [2]*raceArm{armA, armB}}
	m.race = run
	m.busy = true

	d := newRaceDialog(m, run)
	d.resolve("tie")
	// Arm A raced t2, arm B raced t1; the tie keeps t1, the cheaper rung.
	if m.app.tier.ID != "t1" {
		t.Errorf("tie kept %s, want the cheaper t1", m.app.tier.ID)
	}
	winnerLog, err := os.ReadFile(armB.sess.Path())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(winnerLog), `"outcome":"tie"`) {
		t.Error("the kept branch's record does not say the race was a tie")
	}
}

// §15 applied twice over: the arms run at once, so both upper bounds have
// to fit under the ceiling at once.
func TestRacePreflightCoversBothArms(t *testing.T) {
	cat, target := pricedTarget(t)
	tierA := config.Tier{ID: "t1", Target: target}
	tierB := config.Tier{ID: "t2", Target: target}
	before := session.State{}
	opening := provider.UserText("a race under a ceiling")

	bs := &budgetState{}
	if reason, blocked := racePreflight(bs, cat, before, nil, nil, opening, tierA, tierB); blocked {
		t.Fatalf("no ceiling set, but the race was refused: %s", reason)
	}

	bs.set(1 * catalog.MicroUSD)
	reason, blocked := racePreflight(bs, cat, before, nil, nil, opening, tierA, tierB)
	if !blocked {
		t.Fatal("a one-micro-dollar ceiling let a two-arm race through")
	}
	if !strings.Contains(reason, "/budget") {
		t.Errorf("refusal %q does not say how to raise the ceiling", reason)
	}
}

// Nothing kept: the record lands on the session that continues — the one
// the race started from — and both branches close labelled.
func TestFinishRaceAbandonedRecordsOnTheOriginal(t *testing.T) {
	m := raceModel(t)

	armA, err := assembleRaceArm(m.app, m.app.config.Tiers[0], &racedProvider{}, agent.NopObserver{})
	if err != nil {
		t.Fatal(err)
	}
	armB, err := assembleRaceArm(m.app, m.app.config.Tiers[1], &racedProvider{}, agent.NopObserver{})
	if err != nil {
		t.Fatal(err)
	}
	armA.status, armB.status = "completed", "completed"
	run := &raceRun{typed: "abandoned prompt", arms: [2]*raceArm{armA, armB}}
	m.race = run
	m.busy = true

	origPath := m.app.loop.Session.Path()
	m.finishRace(run, "", "abandoned")
	if got := m.app.loop.Session.Path(); got != origPath {
		t.Fatal("abandoning a race swapped sessions")
	}
	log, err := os.ReadFile(m.app.loop.Session.Path())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(log), `"outcome":"abandoned"`) {
		t.Error("the pre-race session's log holds no abandoned race record")
	}
	if m.busy || m.race != nil {
		t.Error("an abandoned race did not release the session")
	}
}
