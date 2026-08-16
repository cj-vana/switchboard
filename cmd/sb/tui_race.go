package main

// /race: one prompt, two rungs, both answers on screen, the user picks
// which branch the session continues on. The mechanics live in race.go;
// what belongs here is the surface — parsing, the rails, the pick dialog,
// and the swap onto the winner. The escalation policy and the advisor sit
// this turn out: an arm that moved rungs mid-race would no longer be the
// rung under trial, so both arms run pinned by construction, with no
// watcher wired to move them.

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/cj-vana/switchboard/internal/agent"
	"github.com/cj-vana/switchboard/internal/catalog"
	"github.com/cj-vana/switchboard/internal/config"
	"github.com/cj-vana/switchboard/internal/credential"
	"github.com/cj-vana/switchboard/internal/permission"
	"github.com/cj-vana/switchboard/internal/provider"
	"github.com/cj-vana/switchboard/internal/session"
	"github.com/cj-vana/switchboard/internal/tools"
)

type raceProbeMsg struct {
	prompt string // as typed; expansion happens when the race starts
	a, b   config.Tier
	ca, cb provider.Provider
	na, nb string // fallback substitution notes, rendered before content is sent
	err    error
}

type raceToolMsg struct {
	arm  int
	name string
}
type raceUsageMsg struct {
	arm int
	u   session.Usage
}
type raceNoticeMsg struct {
	arm         int
	level, text string
}
type raceArmDoneMsg struct {
	arm int
	err error
}

// raceRun is a race in flight. send is how the arm goroutines reach the
// program; tests point it at a collector instead of a tea.Program.
type raceRun struct {
	typed string // what the user wrote, for the record and the transcript
	arms  [2]*raceArm

	cancel    context.CancelFunc
	cancelled bool
	done      [2]bool

	rails  [2]*entry
	tools  [2]int
	in     [2]int
	out    [2]int
	labels [2]string

	send func(tea.Msg)
}

// raceObserver forwards one arm's loop events into the program, labelled.
// Text and thinking deltas are deliberately dropped: two branches streaming
// into one transcript would interleave into noise, so the rails carry
// progress and the finished answers render whole, once each.
type raceObserver struct {
	arm  int
	send func(tea.Msg)
}

func (o *raceObserver) ThinkingDelta(string) {}
func (o *raceObserver) TextDelta(string)     {}
func (o *raceObserver) ToolStart(name string, _ permission.Request) {
	o.send(raceToolMsg{arm: o.arm, name: name})
}
func (o *raceObserver) ToolEnd(string, tools.Result, time.Duration) {}
func (o *raceObserver) Notice(level, text string) {
	o.send(raceNoticeMsg{arm: o.arm, level: level, text: text})
}
func (o *raceObserver) TurnUsage(u session.Usage) {
	o.send(raceUsageMsg{arm: o.arm, u: u})
}

// parseRaceArgs reads "/race tA tB prompt" or "/race tB prompt", the second
// form racing the active tier against tB. The prompt keeps its spacing;
// only the tier tokens are cut off the front.
func parseRaceArgs(app *tuiApp, args string) (config.Tier, config.Tier, string, error) {
	usage := errors.New("usage: /race <tier> [tier] <prompt> — one prompt on two rungs at once; you pick which continues")
	first, rest, _ := strings.Cut(strings.TrimSpace(args), " ")
	if first == "" {
		return config.Tier{}, config.Tier{}, "", usage
	}
	a, ok := app.config.Tier(first)
	if !ok {
		return config.Tier{}, config.Tier{}, "", fmt.Errorf("no tier %s is configured; try /tiers", first)
	}
	second, tail, _ := strings.Cut(strings.TrimSpace(rest), " ")
	if b, ok := app.config.Tier(second); ok {
		if prompt := strings.TrimSpace(tail); prompt != "" {
			return a, b, prompt, nil
		}
		return config.Tier{}, config.Tier{}, "", usage
	}
	// One tier named: the sitting rung takes the other lane.
	if prompt := strings.TrimSpace(rest); prompt != "" {
		return app.tier, a, prompt, nil
	}
	return config.Tier{}, config.Tier{}, "", usage
}

func cmdRace(m *tuiModel, args string) tea.Cmd {
	a, b, prompt, err := parseRaceArgs(m.app, args)
	if err != nil {
		return noticeCmd("error", err.Error())
	}
	if a.ID == b.ID {
		return noticeCmd("error", "a race needs two different rungs; "+a.ID+" against itself measures nothing")
	}
	return func() tea.Msg {
		probedA, ca, na, err := m.app.providers.probeTierFallback(context.Background(), a)
		if err != nil {
			return raceProbeMsg{err: fmt.Errorf("%s cannot race: %w", a.ID, err)}
		}
		probedB, cb, nb, err := m.app.providers.probeTierFallback(context.Background(), b)
		if err != nil {
			return raceProbeMsg{err: fmt.Errorf("%s cannot race: %w", b.ID, err)}
		}
		return raceProbeMsg{prompt: prompt, a: probedA, b: probedB, ca: ca, cb: cb, na: na, nb: nb}
	}
}

// targetTakesImages is the §4 evidence order for vision: a live probe that
// attested image input, then a catalog entry carrying it from its own
// verification. No evidence means no attach.
func (m *tuiModel) targetTakesImages(target provider.RouteTarget) bool {
	if attested, _ := m.app.providers.probedVision(target); attested {
		return true
	}
	info, _, ok := m.app.catalog.Lookup(target)
	return ok && info.Vision
}

// onRaceProbe has both clients answering; assemble the arms and start.
func (m *tuiModel) onRaceProbe(msg raceProbeMsg) tea.Cmd {
	if msg.err != nil {
		m.addNotice("error", msg.err.Error())
		return nil
	}
	if m.busy {
		m.addNotice("error", "a turn started while the race was probing; try again when it finishes")
		return nil
	}
	// Fallback may have substituted either lane's target, so the degenerate
	// check runs against what will actually serve, not what was named.
	if msg.a.Target.ID() == msg.b.Target.ID() {
		m.addNotice("error", "both rungs resolve to "+string(msg.a.Target.ID())+"; a race against the same target measures nothing")
		return nil
	}

	m.addUser(msg.prompt)
	expanded, images := m.expandMentions(msg.prompt)
	prompt := m.adviceContext(m.shellContext(expanded))
	if len(images) > 0 {
		for _, tier := range []config.Tier{msg.a, msg.b} {
			if !m.targetTakesImages(tier.Target) {
				m.addNotice("error", string(tier.Target.ID())+" has no evidence it takes images, and a race is only fair if both arms see the same prompt; drop the image mention or race rungs that both take one")
				return nil
			}
		}
	}
	// The same outbound gate as a plain turn, doubled in consequence: a key
	// in a race prompt would land in two branch logs and two providers.
	if leaks := credential.ScanPrompt(prompt); len(leaks) > 0 {
		return m.openSecretGate(leaks, prompt, func(p string) tea.Cmd {
			return m.startRaceArms(msg, p, images)
		})
	}
	return m.startRaceArms(msg, prompt, images)
}

// startRaceArms is onRaceProbe past the gates that can still stop it:
// preflight, fork, wire, launch.
func (m *tuiModel) startRaceArms(msg raceProbeMsg, prompt string, images []provider.Image) tea.Cmd {
	opening := provider.UserText(prompt)
	for _, img := range images {
		opening.Content = append(opening.Content, img)
	}

	before := m.app.loop.Session.State()
	if reason, blocked := racePreflight(m.app.budget, m.app.catalog, before,
		m.app.loop.System, m.app.loop.Tools.Definitions(), opening, msg.a, msg.b); blocked {
		m.addNotice("error", "no race: "+reason)
		return nil
	}

	send := func(v tea.Msg) { m.app.p.Send(v) }
	armA, err := assembleRaceArm(m.app, msg.a, msg.ca, &raceObserver{arm: 0, send: send})
	if err != nil {
		m.addNotice("error", "race setup failed: "+err.Error())
		return nil
	}
	armB, err := assembleRaceArm(m.app, msg.b, msg.cb, &raceObserver{arm: 1, send: send})
	if err != nil {
		armA.sess.Close()
		m.addNotice("error", "race setup failed: "+err.Error())
		return nil
	}
	// A substitution renders before content is sent and is recorded on the
	// session it serves (§5.4).
	arms := [2]*raceArm{armA, armB}
	for i, note := range []string{msg.na, msg.nb} {
		if note != "" {
			m.addNotice("warn", note)
			arms[i].sess.AppendNote("warn", note)
		}
	}
	raceGates(m.app.budget, m.app.catalog, before, armA, armB)

	run := &raceRun{
		typed: msg.prompt,
		arms:  arms,
		send:  send,
	}
	run.labels[0] = "a · " + raceTierLabel(armA.tier)
	run.labels[1] = "b · " + raceTierLabel(armB.tier)
	m.addNotice("", fmt.Sprintf("racing %s against %s — both branches read-only; you pick which continues",
		raceTierLabel(armA.tier), raceTierLabel(armB.tier)))
	for i, arm := range run.arms {
		run.rails[i] = m.tr.add(&entry{kind: kindInfo, text: run.railLine(i), rank: m.app.rankOf(arm.tier)})
	}
	m.tr.scrollToBottom()

	m.race = run
	m.busy = true
	m.started = time.Now()
	m.launchRace(run, opening)
	return m.spin.Tick
}

func raceTierLabel(t config.Tier) string {
	if t.Label != "" {
		return t.ID + " " + t.Label
	}
	return t.ID
}

// launchRace starts both arms. Each runs a plain TurnMessage on its own
// goroutine against its own forked session; everything they report arrives
// as messages through run.send, so the model stays the only writer of UI
// state.
func (m *tuiModel) launchRace(run *raceRun, opening provider.Message) {
	ctx, cancel := context.WithCancel(context.Background())
	run.cancel = cancel
	for i, arm := range run.arms {
		go func(i int, arm *raceArm) {
			arm.started = time.Now()
			err := arm.loop.TurnMessage(ctx, opening)
			arm.wall = time.Since(arm.started)
			run.send(raceArmDoneMsg{arm: i, err: err})
		}(i, arm)
	}
}

func (run *raceRun) railLine(i int) string {
	state := "running"
	if run.done[i] {
		state = run.arms[i].status
		if run.arms[i].wall > 0 {
			state += " " + run.arms[i].wall.Round(time.Second).String()
		}
	}
	line := fmt.Sprintf("%s — %s", run.labels[i], state)
	if run.tools[i] > 0 {
		line += fmt.Sprintf(" · %d tool calls", run.tools[i])
	}
	if run.in[i]+run.out[i] > 0 {
		line += fmt.Sprintf(" · ↓%s ↑%s", compact(run.in[i]), compact(run.out[i]))
	}
	return line
}

func (m *tuiModel) refreshRail(i int) {
	if m.race == nil || m.race.rails[i] == nil {
		return
	}
	m.race.rails[i].text = m.race.railLine(i)
	if idx := m.tr.indexOf(m.race.rails[i]); idx >= 0 {
		m.tr.invalidate(idx)
	}
}

func (m *tuiModel) onRaceTool(msg raceToolMsg) {
	if m.race == nil {
		return
	}
	m.race.tools[msg.arm]++
	m.refreshRail(msg.arm)
}

func (m *tuiModel) onRaceUsage(msg raceUsageMsg) {
	if m.race == nil {
		return
	}
	m.race.in[msg.arm] += msg.u.Usage.InputTokens + msg.u.Usage.CacheWriteTokens
	m.race.out[msg.arm] += msg.u.Usage.OutputTokens
	m.refreshRail(msg.arm)
}

func (m *tuiModel) onRaceNotice(msg raceNoticeMsg) {
	if m.race == nil {
		return
	}
	m.addNotice(msg.level, m.race.labels[msg.arm]+": "+msg.text)
}

func (m *tuiModel) onRaceArmDone(msg raceArmDoneMsg) tea.Cmd {
	run := m.race
	if run == nil {
		return nil
	}
	arm := run.arms[msg.arm]
	switch {
	case msg.err == nil:
		arm.status = "completed"
	case errors.Is(msg.err, context.Canceled):
		arm.status = "cancelled"
	case errors.Is(msg.err, agent.ErrRoundLimit):
		arm.status = "round_limit"
	default:
		arm.status = "error"
		arm.failure = msg.err.Error()
	}
	run.done[msg.arm] = true
	m.refreshRail(msg.arm)
	if !run.done[0] || !run.done[1] {
		return nil
	}
	return m.onRaceFinished(run)
}

// onRaceFinished has both arms answered, one way or another. Completed
// answers render whole; then the user judges, unless there is nothing left
// to judge.
func (m *tuiModel) onRaceFinished(run *raceRun) tea.Cmd {
	completed := 0
	for i, arm := range run.arms {
		if arm.status != "completed" {
			why := arm.status
			if arm.failure != "" {
				why += ": " + arm.failure
			}
			m.addNotice("warn", run.labels[i]+" has no answer ("+why+")")
			continue
		}
		completed++
		m.addInfo(run.labels[i])
		if text := lastAssistantText(arm.sess.State().Messages); text != "" {
			e := m.tr.add(&entry{kind: kindAssistant, text: text, rank: m.app.rankOf(arm.tier)})
			m.tr.finalize(e)
		}
	}
	m.tr.scrollToBottom()

	if completed == 0 {
		outcome := "incomparable"
		if run.cancelled {
			outcome = "abandoned"
		}
		return m.finishRace(run, "", outcome)
	}
	m.dlg = newRaceDialog(m, run)
	return nil
}

// finishRace resolves the verdict: record, notes, closes, and — when a
// branch was kept — the swap onto it. pick names the kept arm ("a", "b"),
// or is empty when nothing continues; outcome is the Race vocabulary.
func (m *tuiModel) finishRace(run *raceRun, pick, outcome string) tea.Cmd {
	m.race = nil
	m.busy = false

	var kept *raceArm
	switch pick {
	case "a":
		kept = run.arms[0]
	case "b":
		kept = run.arms[1]
	}

	keptTier := ""
	if kept != nil {
		keptTier = kept.tier.ID
	}
	// The record redacts unconditionally: it is a summary, not the
	// transcript, and a key pasted into the /race prompt must not ride a
	// summary into the log after the gate scrubbed it from what was sent.
	prompt := credential.Redact(run.typed, credential.ScanPrompt(run.typed))
	record := raceRecord(prompt, run.arms[0], run.arms[1], outcome, keptTier)

	line := fmt.Sprintf("race %s vs %s: %s", run.arms[0].tier.ID, run.arms[1].tier.ID, outcome)
	if kept != nil {
		line += ", kept " + kept.tier.ID
	}
	m.raceLog = append(m.raceLog, line)

	if kept == nil {
		// Nothing continues: the record lands on the session that does, and
		// both branch logs close labelled, still resumable.
		if err := m.app.loop.Session.AppendRace(record); err != nil {
			m.addNotice("warn", "the race record was not saved: "+err.Error())
		}
		for _, arm := range run.arms {
			arm.sess.AppendNote("info", "race: this branch was not kept ("+outcome+")")
			arm.sess.Close()
		}
		m.addNotice("", "race over, nothing kept; the session continues where it was")
		if len(m.queue) > 0 {
			next := m.queue[0]
			m.queue = m.queue[1:]
			return m.startTurn(next, "")
		}
		return nil
	}

	// The record rides the branch that continues, appended before the swap
	// so it is durable whatever happens next.
	if err := kept.sess.AppendRace(record); err != nil {
		m.addNotice("warn", "the race record was not saved: "+err.Error())
	}
	origID := m.app.loop.Session.ID()
	var loser *raceArm
	for _, arm := range run.arms {
		if arm != kept {
			loser = arm
		}
	}
	loser.sess.AppendNote("info", "race: this branch was not kept; the "+kept.tier.ID+" branch continued")
	loserID := loser.sess.ID()
	loser.sess.Close()

	note := fmt.Sprintf("continuing on the %s branch; /resume %s returns to the pre-race session, /resume %s to the other answer",
		kept.tier.ID, origID, loserID)
	// The swap applies here, on the UI goroutine, not through a command: a
	// command leaves a gap where the session is idle but still the old one,
	// and a prompt submitted into that gap would open a turn on a log about
	// to be closed.
	return m.onSessionSwap(sessionSwapMsg{sess: kept.sess, tier: kept.tier, client: kept.client, note: note})
}

// raceDialog is the verdict. Its options depend on what survived: two
// completed answers offer the full §8.4 vocabulary — either pick, the tie
// that keeps the cheaper rung, or neither — while a lone survivor can only
// be kept or declined, and the record calls that incomparable rather than
// a preference, because a comparison with one side is not one.
type raceDialog struct {
	m   *tuiModel
	run *raceRun
	ids []string
	lbl []string
	sel int
}

func newRaceDialog(m *tuiModel, run *raceRun) *raceDialog {
	d := &raceDialog{m: m, run: run}
	a, b := run.arms[0], run.arms[1]
	if a.status == "completed" {
		d.add("a", "keep "+run.labels[0]+"  "+raceCostLabel(m, a))
	}
	if b.status == "completed" {
		d.add("b", "keep "+run.labels[1]+"  "+raceCostLabel(m, b))
	}
	if a.status == "completed" && b.status == "completed" {
		cheaper := a
		if raceRank(m, b.tier) < raceRank(m, a.tier) {
			cheaper = b
		}
		d.add("tie", "tie — both suffice, keep the cheaper ("+cheaper.tier.ID+")")
	}
	d.add("drop", "neither; stay where the session was")
	return d
}

// raceRank orders a tie: the ladder position is the cost order by
// construction, and a tier off the ladder — a resumed ad-hoc target —
// sorts last rather than first, because rankOf's -1 would otherwise crown
// the one rung whose cost the ladder says nothing about.
func raceRank(m *tuiModel, tier config.Tier) int {
	if r := m.app.rankOf(tier); r >= 0 {
		return r
	}
	return len(m.app.config.Tiers)
}

func (d *raceDialog) add(id, label string) {
	d.ids = append(d.ids, id)
	d.lbl = append(d.lbl, label)
}

// raceCostLabel prices one arm for the pick, three meterings kept apart
// (§4): local consumed nothing scarce, plan consumed quota, and only
// dollars print as dollars.
func raceCostLabel(m *tuiModel, arm *raceArm) string {
	rec := arm.record()
	info, _, ok := m.app.catalog.Lookup(arm.tier.Target)
	switch {
	case !ok:
		return "unpriced"
	case info.Metering == catalog.Local:
		return "local"
	case info.Metering == catalog.Plan:
		return "plan quota"
	default:
		return catalog.Money(rec.CostMicroUSD).String()
	}
}

func (d *raceDialog) update(key tea.KeyMsg, _ *theme) (bool, tea.Cmd) {
	switch key.String() {
	case "esc":
		return true, d.resolve("drop")
	case "up", "k":
		if d.sel > 0 {
			d.sel--
		}
	case "down", "j":
		if d.sel < len(d.ids)-1 {
			d.sel++
		}
	case "enter":
		return true, d.resolve(d.ids[d.sel])
	}
	return false, nil
}

func (d *raceDialog) resolve(id string) tea.Cmd {
	run := d.run
	a, b := run.arms[0], run.arms[1]
	bothCompleted := a.status == "completed" && b.status == "completed"
	switch id {
	case "a", "b":
		outcome := id
		if !bothCompleted {
			outcome = "incomparable"
		}
		return d.m.finishRace(run, id, outcome)
	case "tie":
		pick := "a"
		if raceRank(d.m, b.tier) < raceRank(d.m, a.tier) {
			pick = "b"
		}
		return d.m.finishRace(run, pick, "tie")
	default:
		outcome := "abandoned"
		if !bothCompleted {
			outcome = "incomparable"
		}
		return d.m.finishRace(run, "", outcome)
	}
}

func (d *raceDialog) view(width int, th *theme) string {
	var b strings.Builder
	b.WriteString(th.bold.Render(" which answer does the session keep?") + "\n")
	b.WriteString(th.dim.Render(" the pick is recorded as routing evidence; a tie says the cheaper rung sufficed") + "\n\n")
	for i, label := range d.lbl {
		if i == d.sel {
			b.WriteString(th.accent.Render(" ▌ ") + th.bold.Render(label) + "\n")
		} else {
			b.WriteString(th.dim.Render("   "+label) + "\n")
		}
	}
	b.WriteString(th.faint.Render(" ↑↓ choose · enter keep · esc neither"))
	return b.String()
}
