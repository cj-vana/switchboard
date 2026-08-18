package main

import (
	"context"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/switchboard-code/switchboard/internal/advisor"
	"github.com/switchboard-code/switchboard/internal/agent"
	"github.com/switchboard-code/switchboard/internal/catalog"
	"github.com/switchboard-code/switchboard/internal/checkpoint"
	"github.com/switchboard-code/switchboard/internal/config"
	"github.com/switchboard-code/switchboard/internal/delegate"
	"github.com/switchboard-code/switchboard/internal/execution"
	"github.com/switchboard-code/switchboard/internal/permission"
	"github.com/switchboard-code/switchboard/internal/prefix"
	"github.com/switchboard-code/switchboard/internal/provider"
	route "github.com/switchboard-code/switchboard/internal/router"
	"github.com/switchboard-code/switchboard/internal/session"
	"github.com/switchboard-code/switchboard/internal/skills"
	"github.com/switchboard-code/switchboard/internal/tools"
	"github.com/switchboard-code/switchboard/internal/trust"
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

	// runtimeMu makes the tier label and the loop's provider binding one
	// runtime value. The loop goroutine may move it at a tool-batch boundary;
	// the Bubble Tea goroutine owns the presentation-only tier field below.
	runtimeMu   sync.RWMutex
	runtimeTier config.Tier

	route         *route.Decision
	routeFeatures route.SessionFeatures
	sticky        *route.Sticky
	watcher       *watcher

	// trust is the standing record of which checkouts may run what they
	// declare. Nil when the store could not open; trustErr says why.
	trust    *trust.Store
	trustErr string

	// mcp holds the session's connected servers, for /mcp and shutdown.
	mcp *mcpState

	// lsp is the one lazily started language server assembled before the
	// provider tool schema froze. Its diagnostics subscription is coalesced;
	// runTUI owns cancellation and main owns server shutdown.
	lsp               lspRuntime
	lspNote           string
	lspProblems       <-chan uint64
	lspProblemsCancel func()

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
	caches *cacheSet

	// advisor, when non-nil, wraps the watcher as the loop's observer and
	// feeds the loop's injection point (tui_advisor.go). Nil is off.
	advisorMu sync.RWMutex
	advisor   *advisor.Advisor

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
type tuiObserver struct {
	p              *tea.Program
	workspaceDirty atomic.Bool
}

func (o *tuiObserver) ThinkingDelta(text string) { o.p.Send(deltaMsg{thinking: true, text: text}) }
func (o *tuiObserver) TextDelta(text string)     { o.p.Send(deltaMsg{text: text}) }
func (o *tuiObserver) ToolStart(call provider.ToolUse, req permission.Request) {
	if req.Effect == permission.EffectWrite || req.Effect == permission.EffectExecute {
		o.workspaceDirty.Store(true)
	}
	o.p.Send(toolStartMsg{id: call.ID, name: call.Name, req: req})
}
func (o *tuiObserver) ToolEnd(call provider.ToolUse, _ permission.Request, res tools.Result, took time.Duration) {
	o.p.Send(toolEndMsg{id: call.ID, name: call.Name, res: res, took: took})
}
func (o *tuiObserver) ToolBatchEnd(context.Context) {
	if o.p != nil && o.workspaceDirty.Swap(false) {
		o.p.Send(workspaceInvalidatedMsg{})
	}
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
	return tierLine(a.tier, a.loop.Binding().Target)
}

func tierLine(tier config.Tier, target provider.RouteTarget) string {
	if tier.Label != "" {
		return fmt.Sprintf("%s %s  %s", tier.ID, tier.Label, target.Display())
	}
	return fmt.Sprintf("%s  %s", tier.ID, target.Display())
}

func (a *tuiApp) runtimeSnapshot() (config.Tier, agent.Binding) {
	a.runtimeMu.RLock()
	defer a.runtimeMu.RUnlock()
	tier := a.runtimeTier
	if tier.ID == "" {
		tier = a.tier
	}
	return tier, a.loop.Binding()
}

func (a *tuiApp) bindRuntime(tier config.Tier, client provider.Provider) {
	a.runtimeMu.Lock()
	a.loop.Bind(agent.Binding{Provider: client, Target: tier.Target, Cache: a.caches.For(tier.Target, a.catalog)})
	a.runtimeTier = tier
	a.runtimeMu.Unlock()
}

func (a *tuiApp) currentAdvisor() *advisor.Advisor {
	a.advisorMu.RLock()
	defer a.advisorMu.RUnlock()
	return a.advisor
}

func (a *tuiApp) setAdvisor(value *advisor.Advisor) {
	a.advisorMu.Lock()
	a.advisor = value
	a.advisorMu.Unlock()
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
func (a *tuiApp) moveTo(ctx context.Context, rank int, why string) (func() bool, func(), bool) {
	if rank < 0 || rank >= len(a.config.Tiers) {
		return nil, nil, false
	}
	_, current := a.runtimeSnapshot()
	staying := current.Target.Display()
	probed, client, note, err := a.providers.probeTierFallbackFeasible(ctx, a.config.Tiers[rank], func(candidate config.Tier) error {
		return checkMoveFeasible(a.loop, a.catalog, a.providers, a.budget, candidate, rank)
	})
	if err != nil {
		a.p.Send(noticeMsg{level: "warn", text: "staying on " + staying + ": " + err.Error()})
		return nil, nil, false
	}
	if ctx.Err() != nil {
		return nil, nil, false
	}
	// An escalation abandons the old rung's warmth the same way a user
	// switch does, and the same honesty applies: priced before the rebind
	// discards the tracker, spoken only when there is a number to speak.
	abandoned := ""
	if current.Target.ID() != probed.Target.ID() {
		abandoned = abandonedCacheNote(current.Cache, a.catalog, time.Now())
	}
	bind := func() bool {
		if err := persistRuntimeBinding(a.loop.Session, probed, false); err != nil {
			a.p.Send(noticeMsg{level: "warn", text: "the automatic tier move was not saved: " + err.Error()})
			return false
		}
		a.bindRuntime(probed, client)
		return true
	}
	line := "now on " + tierLine(probed, probed.Target)
	after := func() {
		if note != "" {
			a.p.Send(noticeMsg{level: "warn", text: note})
			a.loop.Session.AppendNote("warn", note)
		}
		if abandoned != "" {
			// The note is a fact about this session's economics, so it goes in
			// the session's record: /export carries it where it happened, and a
			// resumed reading still sees what the move cost.
			a.loop.Session.AppendNote("info", abandoned)
		}
		a.p.Send(tierNowMsg{line: line, rank: a.rankOf(probed), tier: probed, abandoned: abandoned})
	}
	return bind, after, true
}

// bind moves the loop onto a session, tier, and client, and rebuilds the
// escalation wiring around the new rank. pin marks a deliberate user choice,
// which the sticky policy treats the way the -tier flag does at startup. The
// caller swaps sessions only while idle, so this never races a turn.
func (a *tuiApp) bind(tier config.Tier, client provider.Provider, pin bool) {
	a.tier = tier
	// A tier may cross providers, so the adapter moves with the target. So does
	// the cache: markers, minimums, and observed state all belong to a target,
	// and carrying one target's tracker onto another would attribute its cache
	// to a server that never held it.
	a.bindRuntime(tier, client)

	rank := a.rankOf(tier)
	if rank < 0 {
		rank = 0
	}
	a.sticky = route.NewSticky(route.Policy{}, rank)
	if pin {
		a.sticky.Pin(rank)
	}
	a.watcher = newWatcher(a.obs, a.sticky, len(a.config.Tiers)-1, a.moveTo)
	a.loop.SetObserver(a.watcher)
	// The advisor survives the rebuild by wrapping whatever replaced its
	// inner observer; dropping it silently on a tier switch would turn it off
	// without anyone saying so.
	if advisor := a.currentAdvisor(); advisor != nil {
		advisor.SetInner(a.watcher)
		advisor.SetMeter(advisorMeterFor(a, a.loop.Session, advisor.Target()))
		a.loop.SetObserver(advisor)
	}
}

// switchTier probes the target off the UI goroutine; the rebind happens when
// the result message arrives, while the loop is idle.
func (m *tuiModel) switchTier(id string) tea.Cmd {
	a := m.app
	tier, ok := a.config.Tier(id)
	if !ok {
		return noticeCmd("error", "no tier "+id+" is configured; try /tiers")
	}
	if tier.ID == a.tier.ID && tier.Target.ID() == a.tier.Target.ID() {
		if err := persistRuntimeBinding(a.loop.Session, a.tier, true); err != nil {
			return noticeCmd("error", "tier pin was not saved: "+err.Error())
		}
		if a.sticky != nil {
			if rank := a.rankOf(a.tier); rank >= 0 {
				a.sticky.Pin(rank)
			}
		}
		a.route = &route.Decision{
			Tier: a.tier.ID, Target: a.tier.Target.ID(), Confidence: 1,
			Source: route.SourceUserPin, Rationale: "tier selected by you", PolicyRevision: route.PolicyRevision,
		}
		return noticeCmd("", "already on "+a.tierLine())
	}
	ctx, operation, sourceID, err := m.startOperation("tier switch")
	if err != nil {
		return noticeCmd("warn", err.Error())
	}
	return func() tea.Msg {
		probed, client, note, err := a.providers.probeTierFallback(ctx, tier)
		return tierSwitchMsg{tier: probed, client: client, note: note, err: err,
			operation: operation, sourceID: sourceID}
	}
}

// reopen loads a recorded session and the target it was recorded with, the
// same way --resume does at startup.
func (a *tuiApp) reopen(ctx context.Context, operation uint64, sourceID, id string) tea.Cmd {
	return func() tea.Msg {
		if err := ctx.Err(); err != nil {
			return sessionSwapMsg{err: err, operation: operation, sourceID: sourceID}
		}
		release, err := pauseAdvisorLedger(ctx, a)
		if err != nil {
			return sessionSwapMsg{err: fmt.Errorf("waiting for the advisor ledger before resume: %w", err), operation: operation, sourceID: sourceID}
		}
		sess, err := a.store.Open(id)
		if err != nil {
			return sessionSwapMsg{err: err, release: release, operation: operation, sourceID: sourceID}
		}
		state := sess.State()
		tier, configured, matchErr := tierForSessionState(a.config, state)
		if matchErr != nil {
			sess.Close()
			return sessionSwapMsg{err: matchErr, release: release, operation: operation, sourceID: sourceID}
		}
		var probed config.Tier
		var client provider.Provider
		var note string
		if configured {
			rank := a.rankOf(tier)
			probed, client, note, err = a.providers.probeTierFallbackFeasible(ctx, tier, func(candidate config.Tier) error {
				return checkResumeFeasible(sess, a.loop, a.catalog, a.providers, a.budget, candidate, rank)
			})
		} else {
			probed, client, err = a.providers.probeTier(ctx, tier)
		}
		if err != nil {
			sess.Close()
			return sessionSwapMsg{err: err, release: release, operation: operation, sourceID: sourceID}
		}
		pinned := state.RuntimeBinding.Target != "" && state.RuntimeBinding.Pinned
		return sessionSwapMsg{sess: sess, tier: probed, client: client, note: note, warnNote: note != "", pinned: pinned,
			release: release, operation: operation, sourceID: sourceID}
	}
}

func checkResumeFeasible(sess *session.Session, loop *agent.Loop, cat *catalog.Catalog, probes *providers, budget *budgetState, tier config.Tier, rank int) error {
	state := sess.State()
	request := provider.Request{System: loop.System, Tools: loop.Tools.Definitions(), Messages: state.Messages}
	promptTokens := prefix.RequestTokens(request)
	contextTokens := prefix.RequestTokenCeiling(request)
	remaining, limited := remainingBudget(budget, state.ID,
		catalog.Money(state.RetryReserveMicroUSD), catalog.Money(state.AccountedCostMicroUSD()))
	_, err := (route.Heuristic{}).Route(route.Input{
		Candidates:   []route.Candidate{withLiveVision(candidateForTierContext(tier, rank, cat, promptTokens, contextTokens, 0), probes)},
		Requirements: route.Requirements{NeedsTools: true, NeedsVision: messagesNeedVision(state.Messages)},
		Budgets:      route.Budgets{MaxCost: remaining, MaxCostSet: limited}, Pin: tier.ID,
	})
	return err
}

// forkSession branches the current session at a message boundary into a new
// log and continues there (§12). The original is read, never written; the
// fork's prefix is byte-identical to it, so a provider still holding that
// prefix warm serves the fork warm. Files are not rewound — /undo is what
// restores files, and it keeps working across the swap because turns changed
// the workspace, whichever log they live in now.
func (a *tuiApp) forkSession(ctx context.Context, operation uint64, sourceID, id string, keepMessages int, dropped int) tea.Cmd {
	tier := a.tier
	client := a.loop.Binding().Provider
	source := a.loop.Session
	return func() tea.Msg {
		release, err := pauseAdvisorLedger(ctx, a)
		if err != nil {
			return sessionSwapMsg{err: fmt.Errorf("waiting for the advisor ledger before fork: %w", err), operation: operation, sourceID: sourceID}
		}
		sess, err := a.store.ForkSession(source, keepMessages)
		if err != nil {
			return sessionSwapMsg{err: err, release: release, operation: operation, sourceID: sourceID}
		}
		note := fmt.Sprintf("forked from %s; the original is untouched, /resume %s returns to it", id, id)
		if dropped > 0 {
			note = fmt.Sprintf("forked from %s, less its last %d user turns; the original is untouched, /resume %s returns to it", id, dropped, id)
		}
		return sessionSwapMsg{sess: sess, tier: tier, client: client, note: note, release: release, operation: operation, sourceID: sourceID,
			preserveRuntimeTarget: true}
	}
}

// clearSession starts a fresh log on the current target, keeping the client.
func (a *tuiApp) clearSession(ctx context.Context, operation uint64, sourceID string) tea.Cmd {
	tier := a.tier
	client := a.loop.Binding().Provider
	return func() tea.Msg {
		if err := ctx.Err(); err != nil {
			return sessionSwapMsg{err: err, operation: operation, sourceID: sourceID}
		}
		release, err := pauseAdvisorLedger(ctx, a)
		if err != nil {
			return sessionSwapMsg{err: fmt.Errorf("waiting for the advisor ledger before clear: %w", err), operation: operation, sourceID: sourceID}
		}
		sess, err := a.store.Create(a.workspace, tier.Target.ID(), a.catalog.Revision)
		if err != nil {
			return sessionSwapMsg{err: err, release: release, operation: operation, sourceID: sourceID}
		}
		return sessionSwapMsg{sess: sess, tier: tier, client: client, fresh: true, release: release, operation: operation, sourceID: sourceID,
			preserveRuntimeTarget: true}
	}
}

func (m *tuiModel) reopen(id string) tea.Cmd {
	ctx, generation, sourceID, err := m.startOperation("resume")
	if err != nil {
		return noticeCmd("warn", err.Error())
	}
	return m.app.reopen(ctx, generation, sourceID, id)
}

func (m *tuiModel) forkSession(id string, keepMessages, dropped int) tea.Cmd {
	ctx, generation, sourceID, err := m.startOperation("fork")
	if err != nil {
		return noticeCmd("warn", err.Error())
	}
	return m.app.forkSession(ctx, generation, sourceID, id, keepMessages, dropped)
}

func (m *tuiModel) clearSession() tea.Cmd {
	ctx, generation, sourceID, err := m.startOperation("clear")
	if err != nil {
		return noticeCmd("warn", err.Error())
	}
	return m.app.clearSession(ctx, generation, sourceID)
}
