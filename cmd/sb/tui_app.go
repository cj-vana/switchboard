package main

import (
	"context"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/cj-vana/switchboard/internal/advisor"
	"github.com/cj-vana/switchboard/internal/agent"
	"github.com/cj-vana/switchboard/internal/catalog"
	"github.com/cj-vana/switchboard/internal/checkpoint"
	"github.com/cj-vana/switchboard/internal/config"
	"github.com/cj-vana/switchboard/internal/delegate"
	"github.com/cj-vana/switchboard/internal/execution"
	"github.com/cj-vana/switchboard/internal/permission"
	"github.com/cj-vana/switchboard/internal/prefix"
	"github.com/cj-vana/switchboard/internal/provider"
	route "github.com/cj-vana/switchboard/internal/router"
	"github.com/cj-vana/switchboard/internal/session"
	"github.com/cj-vana/switchboard/internal/skills"
	"github.com/cj-vana/switchboard/internal/tools"
	"github.com/cj-vana/switchboard/internal/trust"
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

	// trust is the standing record of which checkouts may run what they
	// declare. Nil when the store could not open; trustErr says why.
	trust    *trust.Store
	trustErr string

	// mcp holds the session's connected servers, for /mcp and shutdown.
	mcp *mcpState

	// undo is the per-turn file checkpoint recorder, for /undo.
	undo *checkpoint.Recorder

	// agents are the named subagent definitions the session discovered, and
	// agentNotes what their loading had to say; both for /agents.
	agents     []delegate.Agent
	agentNotes []string

	// skills are the loaded skill definitions, for /skills; the tool serving
	// them was registered at assembly.
	skills []skills.Skill

	// budget is the shared dollar ceiling, for /budget and the escalation
	// guard; the loop reads the same state before every call.
	budget *budgetState

	// advisor, when non-nil, wraps the watcher as the loop's observer and
	// feeds the loop's injection point (tui_advisor.go). Nil is off.
	advisor *advisor.Advisor

	// watchSt holds the /watch verifier and its per-turn accounting
	// (tui_watch.go). The struct is always present; an unarmed watch
	// contributes nothing at the injection seam.
	watchSt *watchState

	// onboarded marks a session opened straight out of the first-run
	// wizard: the banner gets one extra line for the things a new user
	// will not find alone, once, because the second session is not a
	// first impression.
	onboarded bool

	obs *tuiObserver
	p   *tea.Program
}

// displayPath renders an absolute path workspace-relative, the way the
// tools' own messages do.
func (a *tuiApp) displayPath(abs string) string {
	if rel, err := filepath.Rel(a.workspace, abs); err == nil && !strings.HasPrefix(rel, "..") {
		return rel
	}
	return abs
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
	// A quality trigger may override a cost preference and never a hard
	// ceiling (§8.3): a destination whose upper bound does not fit is
	// refused with the reason, and the primary stays put.
	if a.budget != nil {
		state := a.loop.Session.State()
		tokens := prefix.RequestTokens(provider.Request{
			System: a.loop.System, Tools: a.loop.Tools.Definitions(), Messages: state.Messages,
		})
		if reason, blocked := budgetBlocksMove(a.budget, a.catalog, a.config.Tiers[rank],
			catalog.Money(state.CostMicroUSD), tokens); blocked {
			a.p.Send(noticeMsg{level: "warn", text: "staying on " + a.tier.ID + ": " + reason})
			return
		}
	}
	probed, client, note, err := a.providers.probeTierFallback(context.Background(), a.config.Tiers[rank])
	if err != nil {
		a.p.Send(noticeMsg{level: "warn", text: "staying on " + a.tier.ID + ": " + err.Error()})
		return
	}
	if note != "" {
		a.p.Send(noticeMsg{level: "warn", text: note})
		a.loop.Session.AppendNote("warn", note)
	}
	a.tier = probed
	a.loop.Target = probed.Target
	a.loop.Provider = client
	a.loop.Cache = cacheFor(probed.Target, a.catalog)
	a.p.Send(tierNowMsg{line: "now on " + a.tierLine(), rank: a.rankOf(probed)})
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
	// The advisor survives the rebuild by wrapping whatever replaced its
	// inner observer; dropping it silently on a tier switch would turn it off
	// without anyone saying so.
	if a.advisor != nil {
		a.advisor.SetInner(a.watcher)
		a.loop.Observer = a.advisor
	}
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
		probed, client, note, err := a.providers.probeTierFallback(context.Background(), tier)
		return tierSwitchMsg{tier: probed, client: client, note: note, err: err}
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

// forkSession branches the current session at a message boundary into a new
// log and continues there (§12). The original is read, never written; the
// fork's prefix is byte-identical to it, so a provider still holding that
// prefix warm serves the fork warm. Files are not rewound — /undo is what
// restores files, and it keeps working across the swap because turns changed
// the workspace, whichever log they live in now.
func (a *tuiApp) forkSession(id string, keepMessages int, dropped int) tea.Cmd {
	return func() tea.Msg {
		sess, err := a.store.Fork(id, keepMessages)
		if err != nil {
			return sessionSwapMsg{err: err}
		}
		note := fmt.Sprintf("forked from %s; the original is untouched, /resume %s returns to it", id, id)
		if dropped > 0 {
			note = fmt.Sprintf("forked from %s, less its last %d user turns; the original is untouched, /resume %s returns to it", id, dropped, id)
		}
		return sessionSwapMsg{sess: sess, tier: a.tier, client: a.loop.Provider, note: note}
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
