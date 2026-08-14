package main

import (
	"context"
	"fmt"
	"slices"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/cj-vana/switchboard/internal/agent"
	"github.com/cj-vana/switchboard/internal/catalog"
	"github.com/cj-vana/switchboard/internal/config"
	"github.com/cj-vana/switchboard/internal/execution"
	"github.com/cj-vana/switchboard/internal/permission"
	"github.com/cj-vana/switchboard/internal/provider"
	route "github.com/cj-vana/switchboard/internal/router"
	"github.com/cj-vana/switchboard/internal/session"
	"github.com/cj-vana/switchboard/internal/tools"
)

// tuiApp owns the session mechanics the TUI drives: the loop, the ladder, the
// sticky policy, and session swaps. It is the TUI's counterpart to repl, and
// what it does it does the same way — a tier switch probes first, a move that
// cannot be served leaves the target alone, and §8.4's routing record is
// written raw.
type tuiApp struct {
	loop       *agent.Loop
	store      *session.Store
	config     *config.Config
	catalog    *catalog.Catalog
	tier       config.Tier
	providers  *providers
	capability execution.Capability
	workspace  string

	route   *route.Decision
	sticky  *route.Sticky
	watcher *watcher

	obs *tuiObserver
	p   *tea.Program
}

// tuiObserver is the loop's Observer, forwarding into the Bubble Tea program.
// Called from the loop's goroutine; Send queues without blocking.
type tuiObserver struct{ p *tea.Program }

func (o *tuiObserver) ThinkingDelta(text string) { o.p.Send(deltaMsg{thinking: true, text: text}) }
func (o *tuiObserver) TextDelta(text string)     { o.p.Send(deltaMsg{text: text}) }
func (o *tuiObserver) ToolStart(name string, req permission.Request) {
	o.p.Send(toolStartMsg{name: name, req: req})
}
func (o *tuiObserver) ToolEnd(name string, res tools.Result, took time.Duration) {
	o.p.Send(toolEndMsg{name: name, res: res, took: took})
}
func (o *tuiObserver) Notice(level, text string) { o.p.Send(noticeMsg{level: level, text: text}) }
func (o *tuiObserver) TurnUsage(u session.Usage) { o.p.Send(usageMsg{u: u}) }

// tuiAsker resolves a permission Ask against a dialog in the TUI. The loop
// blocks here until the user answers or the turn is cancelled; a program that
// has already quit leaves no one to answer, so nothing is approved.
type tuiAsker struct{ p *tea.Program }

func (a *tuiAsker) Ask(ctx context.Context, req permission.Request, out permission.Outcome) (permission.Response, error) {
	respond := make(chan permission.Response, 1)
	a.p.Send(askMsg{req: req, out: out, respond: respond})
	select {
	case resp := <-respond:
		return resp, nil
	case <-ctx.Done():
		return permission.Response{}, ctx.Err()
	}
}

func (a *tuiApp) tierLine() string {
	target := string(a.loop.Target.ID())
	if a.tier.Label != "" {
		return fmt.Sprintf("%s %s  %s", a.tier.ID, a.tier.Label, target)
	}
	return fmt.Sprintf("%s  %s", a.tier.ID, target)
}

func (a *tuiApp) rankOf(tier config.Tier) int {
	return slices.IndexFunc(a.config.Tiers, func(t config.Tier) bool { return t.ID == tier.ID })
}

// moveTo rebinds the loop after the escalation policy changed the primary. It
// runs on the loop's goroutine, inside a turn, so the rebind is synchronous
// with the loop and the UI hears about it through the program.
//
// A move that cannot be served leaves the target where it is: reporting a
// switch and then not making it would be worse than staying, because every
// later line would describe the wrong target.
func (a *tuiApp) moveTo(rank int, why string) {
	if rank < 0 || rank >= len(a.config.Tiers) {
		return
	}
	probed, client, err := a.providers.probeTier(context.Background(), a.config.Tiers[rank])
	if err != nil {
		a.p.Send(noticeMsg{level: "warn", text: "staying on " + a.tier.ID + ": " + err.Error()})
		return
	}
	a.tier = probed
	a.loop.Target = probed.Target
	a.loop.Provider = client
	a.loop.Cache = cacheFor(probed.Target, a.catalog)
	a.p.Send(tierNowMsg{line: "now on " + a.tierLine()})
}

// bind moves the loop onto a session, tier, and client, and rebuilds the
// escalation wiring around the new rank. pin marks a deliberate user choice,
// which the sticky policy treats the way the -tier flag does at startup. The
// caller swaps sessions only while idle, so this never races a turn.
func (a *tuiApp) bind(tier config.Tier, client provider.Provider, pin bool) {
	a.tier = tier
	a.loop.Target = tier.Target
	a.loop.Provider = client
	// A tier may cross providers, so the adapter moves with the target. So does
	// the cache: markers, minimums, and observed state all belong to a target,
	// and carrying one target's tracker onto another would attribute its cache
	// to a server that never held it.
	a.loop.Cache = cacheFor(tier.Target, a.catalog)

	rank := a.rankOf(tier)
	if rank < 0 {
		rank = 0
	}
	a.sticky = route.NewSticky(route.Policy{}, rank)
	if pin {
		a.sticky.Pin(rank)
	}
	a.watcher = newWatcher(a.obs, a.sticky, len(a.config.Tiers)-1, a.moveTo)
	a.loop.Observer = a.watcher
}

// switchTier probes the target off the UI goroutine; the rebind happens when
// the result message arrives, while the loop is idle.
func (a *tuiApp) switchTier(id string) tea.Cmd {
	tier, ok := a.config.Tier(id)
	if !ok {
		return noticeCmd("error", "no tier "+id+" is configured; try /tiers")
	}
	if tier.ID == a.tier.ID {
		return noticeCmd("", "already on "+a.tierLine())
	}
	return func() tea.Msg {
		probed, client, err := a.providers.probeTier(context.Background(), tier)
		return tierSwitchMsg{tier: probed, client: client, err: err}
	}
}

// reopen loads a recorded session and the target it was recorded with, the
// same way --resume does at startup.
func (a *tuiApp) reopen(id string) tea.Cmd {
	return func() tea.Msg {
		sess, err := a.store.Open(id)
		if err != nil {
			return sessionSwapMsg{err: err}
		}
		recorded := sess.State().Target
		var tier config.Tier
		if t, ok := tierForTarget(a.config, recorded); ok {
			tier = t
		} else {
			target, err := parseRecordedTarget(recorded)
			if err != nil {
				sess.Close()
				return sessionSwapMsg{err: err}
			}
			tier = config.Tier{ID: "-resumed", Label: "resumed", Target: target}
		}
		probed, client, err := a.providers.probeTier(context.Background(), tier)
		if err != nil {
			sess.Close()
			return sessionSwapMsg{err: err}
		}
		return sessionSwapMsg{sess: sess, tier: probed, client: client}
	}
}

// clearSession starts a fresh log on the current target, keeping the client.
func (a *tuiApp) clearSession() tea.Cmd {
	return func() tea.Msg {
		sess, err := a.store.Create(a.workspace, a.tier.Target.ID(), a.catalog.Revision)
		if err != nil {
			return sessionSwapMsg{err: err}
		}
		return sessionSwapMsg{sess: sess, tier: a.tier, client: a.loop.Provider, fresh: true}
	}
}

// bannerLines mirrors the REPL banner: what a session starts on is stated, not
// implied.
func (a *tuiApp) bannerLines(sess *session.Session, resumed bool) []string {
	state := sess.State()
	lines := []string{
		"switchboard " + a.tierLine(),
		"  workspace  " + a.workspace,
		"  mode       " + string(a.loop.Perms.Mode()),
		"  sandbox    " + a.capability.Summary(),
		"  catalog    " + a.catalog.Revision + " (" + a.catalog.Source + ")",
	}
	if resumed {
		lines = append(lines, fmt.Sprintf("  session    %s, resumed with %d messages", state.ID, len(state.Messages)))
	} else {
		lines = append(lines, "  session    "+state.ID)
	}
	if lost := sess.TruncatedBytes(); lost > 0 {
		lines = append(lines, fmt.Sprintf(
			"  recovered from an interrupted write; %d bytes at the end of the log were unreadable and were dropped", lost))
	}
	lines = append(lines, "", "  /help for commands, /exit to leave", "")
	return lines
}
