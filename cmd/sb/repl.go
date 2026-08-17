package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"time"

	"github.com/cj-vana/switchboard/internal/agent"
	"github.com/cj-vana/switchboard/internal/catalog"
	"github.com/cj-vana/switchboard/internal/config"
	"github.com/cj-vana/switchboard/internal/credential"
	"github.com/cj-vana/switchboard/internal/execution"
	"github.com/cj-vana/switchboard/internal/permission"
	"github.com/cj-vana/switchboard/internal/prefix"
	"github.com/cj-vana/switchboard/internal/provider"
	route "github.com/cj-vana/switchboard/internal/router"
	"github.com/cj-vana/switchboard/internal/session"
)

type repl struct {
	loop       *agent.Loop
	out        *renderer
	in         *bufio.Reader
	capability execution.Capability
	workspace  string

	config    *config.Config
	catalog   *catalog.Catalog
	tier      config.Tier
	providers *providers

	// route is what chose the starting target, when a router chose it. §8.1
	// renders this rather than logging it, because principle 3 requires the
	// user can see why.
	route   *route.Decision
	sticky  *route.Sticky
	watcher *watcher

	// budget is the shared ceiling the loop's gate reads; the REPL checks it
	// before an escalation the same way the TUI does.
	budget *budgetState
}

// moveTo rebinds the loop after the escalation policy changed the primary.
//
// A move that cannot be served leaves the target where it is: reporting a
// switch and then not making it would be worse than staying, because every
// later line would describe the wrong target.
func (r *repl) moveTo(rank int, why string) {
	if rank < 0 || rank >= len(r.config.Tiers) {
		return
	}
	tier := r.config.Tiers[rank]

	// Same refusal as the TUI's: an escalation never overrides the ceiling.
	if r.budget != nil {
		state := r.loop.Session.State()
		tokens := prefix.RequestTokens(provider.Request{
			System: r.loop.System, Tools: r.loop.Tools.Definitions(), Messages: state.Messages,
		})
		if reason, blocked := budgetBlocksMove(r.budget, r.catalog, tier,
			catalog.Money(state.CostMicroUSD), tokens); blocked {
			r.out.Notice("warn", "staying on "+r.tier.ID+": "+reason)
			return
		}
	}

	probed, client, note, err := r.providers.probeTierFallback(context.Background(), tier)
	if err != nil {
		r.out.Notice("warn", "staying on "+r.tier.ID+": "+err.Error())
		return
	}
	if note != "" {
		r.out.Notice("warn", note)
		r.loop.Session.AppendNote("warn", note)
	}
	r.tier = probed
	r.loop.Target = probed.Target
	r.loop.Provider = client
	r.loop.Cache = cacheFor(probed.Target, r.catalog)
	r.out.line(r.out.style(dim, "  now on "+r.tierLine()))
}

func (r *repl) banner(sess *session.Session, resumed bool) {
	state := sess.State()

	r.out.line(r.out.style(bold, "switchboard") + " " + r.out.style(dim, r.tierLine()))
	r.out.line(r.out.style(dim, "  workspace  "+r.workspace))
	r.out.line(r.out.style(dim, "  mode       "+string(r.loop.Perms.Mode())))
	r.out.line(r.out.style(dim, "  sandbox    "+r.capability.Summary()))
	r.out.line(r.out.style(dim, "  catalog    "+r.catalog.Revision+" ("+r.catalog.Source+")"))
	if r.route != nil {
		for _, line := range describeRoute(*r.route) {
			r.out.line(r.out.style(dim, line))
		}
	}

	if resumed {
		r.out.line(r.out.style(dim, fmt.Sprintf("  session    %s, resumed with %d messages",
			state.ID, len(state.Messages))))
	} else {
		r.out.line(r.out.style(dim, "  session    "+state.ID))
	}

	if lost := sess.TruncatedBytes(); lost > 0 {
		r.out.line(r.out.style(red, fmt.Sprintf(
			"  recovered from an interrupted write; %d bytes at the end of the log were unreadable and were dropped", lost)))
	}

	r.out.line("")
	r.out.line(r.out.style(dim, "  /help for commands, /exit to leave"))
	r.out.line("")
	r.out.flush()
}

func (r *repl) tierLine() string {
	target := string(r.loop.Target.ID())
	if r.tier.Label != "" {
		return fmt.Sprintf("%s %s  %s", r.tier.ID, r.tier.Label, target)
	}
	return fmt.Sprintf("%s  %s", r.tier.ID, target)
}

// once runs a single prompt. It is what makes the phase-0 exit gate scriptable:
// a turn can be started, interrupted, and resumed without a terminal.
func (r *repl) once(ctx context.Context, prompt string) error {
	err := r.turn(ctx, prompt)
	r.summary()
	return err
}

func (r *repl) interactive(ctx context.Context) error {
	for {
		r.out.w.WriteString(r.out.style(bold, "› "))
		r.out.atLineTop = false
		r.out.flush()

		input, err := r.in.ReadString('\n')
		if errors.Is(err, io.EOF) {
			r.out.line("")
			return nil
		}
		if err != nil {
			return err
		}

		input = strings.TrimSpace(input)
		if input == "" {
			continue
		}
		if strings.HasPrefix(input, "/") {
			if done := r.command(ctx, input); done {
				return nil
			}
			continue
		}

		// The outbound credential gate, on the one scripted surface that can
		// still ask: same three answers as the TUI dialog, in line.
		if leaks := credential.ScanPrompt(input); len(leaks) > 0 {
			input = r.secretGate(input, leaks)
			if input == "" {
				continue
			}
		}

		if err := r.turn(ctx, input); err != nil {
			if errors.Is(err, context.Canceled) {
				r.out.Notice("warn", "turn cancelled; the session is intact and can continue")
				continue
			}
			if errors.Is(err, agent.ErrRoundLimit) {
				continue
			}
			r.out.Notice("error", err.Error())
		}
	}
}

// secretGate holds a key-shaped prompt behind a one-line question, the
// REPL's form of the TUI's dialog. Anything that is not a deliberate
// answer drops the prompt, because the default direction for a question
// about a credential has to be the safe one — and the question itself
// names kinds and prefixes only, never the match.
func (r *repl) secretGate(input string, leaks []credential.Leak) string {
	kinds := make([]string, len(leaks))
	for i, l := range leaks {
		kinds[i] = l.String()
	}
	r.out.Notice("warn", "the prompt contains "+strings.Join(kinds, ", "))
	r.out.w.WriteString("[r]edact and send, [s]end as typed, anything else drops it: ")
	r.out.atLineTop = false
	r.out.flush()

	answer, err := r.in.ReadString('\n')
	if err != nil {
		return ""
	}
	r.out.atLineTop = true
	switch strings.ToLower(strings.TrimSpace(answer)) {
	case "r", "redact":
		return credential.Redact(input, leaks)
	case "s", "send":
		return input
	}
	r.out.Notice("", "not sent; the prompt was dropped before anything left this machine")
	return ""
}

// turn runs one message with an interrupt handler bound to it. Ctrl-C cancels
// the turn and returns to the prompt rather than killing the process, because
// the session is resumable and the work already done is worth keeping.
func (r *repl) turn(ctx context.Context, input string) error {
	turnCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt)
	defer signal.Stop(sig)

	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-sig:
			cancel()
		case <-done:
		}
	}()

	if r.watcher != nil {
		r.watcher.StartTurn()
	}

	before := r.loop.Session.State()
	startedOn := r.tier
	started := time.Now()

	err := r.loop.Turn(turnCtx, input)
	r.out.endTurn()
	r.recordRoute(input, startedOn, before, started, err)
	return err
}

// recordRoute writes §8.4's training signal for the turn that just ended.
//
// It is written from ordinary sessions rather than only from eval runs, because
// a corpus of deliberate measurements is a corpus of tasks somebody thought to
// write down, and the distribution that matters is the one the user actually
// works in.
//
// The outcome is recorded raw. §8.4 is explicit that an escalation is not a
// negative label and a clean completion is weak evidence of sufficiency and none
// of necessity, so turning any of this into a label is a decision for whoever
// trains on it, not one to bake in here.
func (r *repl) recordRoute(prompt string, startedOn config.Tier, before session.State, started time.Time, turnErr error) {
	after := r.loop.Session.State()
	err := appendRouteRecord(r.loop.Session, prompt, startedOn, r.tier, before, after, started, turnErr, r.route, r.sticky)
	if err != nil {
		r.out.Notice("warn", "the routing record for this turn was not saved: "+err.Error())
	}
}

// appendRouteRecord is the UI-independent half of recordRoute, shared with the
// TUI: it derives the record and appends it, leaving error reporting to the
// caller's surface.
func appendRouteRecord(sess *session.Session, prompt string, startedOn, endedOn config.Tier, before, after session.State, started time.Time, turnErr error, routeDec *route.Decision, sticky *route.Sticky) error {
	rec := session.Route{
		TurnDepth:    len(before.Messages),
		PromptChars:  len(prompt),
		Tier:         startedOn.ID,
		Target:       startedOn.Target.ID(),
		Source:       "manual",
		Usage:        after.Usage.Sub(before.Usage),
		CostMicroUSD: after.CostMicroUSD - before.CostMicroUSD,
		WallTimeMS:   time.Since(started).Milliseconds(),
		Outcome:      string(route.Completed),
	}
	if routeDec != nil {
		rec.Source = string(routeDec.Source)
		rec.Rationale = routeDec.Rationale
	}
	if sticky != nil && endedOn.ID != startedOn.ID {
		rec.Escalations = 1
		rec.EndedOn = endedOn.Target.ID()
	}
	switch {
	case errors.Is(turnErr, context.Canceled):
		// A cancelled turn is abandonment, which §8.4 censors rather than
		// counting against the target: the user walked away and told you
		// nothing about the choice.
		rec.Outcome = string(route.Abandoned)
	case turnErr != nil:
		rec.Outcome = string(route.Escalated)
	}

	return sess.AppendRoute(rec)
}

// command handles a slash command and reports whether the REPL should exit.
func (r *repl) command(ctx context.Context, input string) bool {
	name, rest, _ := strings.Cut(strings.TrimPrefix(input, "/"), " ")
	rest = strings.TrimSpace(rest)

	// A bare tier name switches to it, which is the shortest path to the one
	// control the user most often wants (design principle 3).
	if _, ok := r.config.Tier(name); ok {
		r.switchTier(ctx, name)
		r.out.flush()
		return false
	}

	switch name {
	case "exit", "quit":
		return true

	case "help":
		r.out.line("  /tN                                       switch to tier N, for example /t2")
		r.out.line("  /tiers                                    show the configured ladder")
		r.out.line("  /mode [plan|default|acceptEdits|bypass]   show or change the permission mode")
		r.out.line("  /cost                                     tokens and cost for this session")
		r.out.line("  /session                                  session id, target, and message count")
		r.out.line("  /sandbox                                  what isolation this host provides")
		r.out.line("  /exit                                     leave")

	case "tier":
		if rest == "" {
			r.out.line("  " + r.tierLine())
			break
		}
		r.switchTier(ctx, rest)

	case "tiers":
		if len(r.config.Tiers) == 0 {
			r.out.line("  no tiers configured in " + r.config.Path)
			break
		}
		for _, t := range r.config.Tiers {
			marker := "  "
			if t.ID == r.tier.ID {
				marker = "* "
			}
			r.out.line(marker + t.String())
		}

	case "mode":
		if rest == "" {
			r.out.line("  " + string(r.loop.Perms.Mode()))
			break
		}
		mode, err := permission.ParseMode(rest)
		if err != nil {
			r.out.Notice("error", err.Error())
			break
		}
		r.loop.Perms.SetMode(mode)
		r.out.line("  mode is now " + string(mode))
		if mode == permission.ModeBypass && !r.capability.AutomaticExecutionAllowed() {
			// Saying this once, plainly, beats letting the user discover it by
			// being prompted anyway and reading it as a bug (§19.3).
			r.out.line(r.out.style(dim, "  commands will still be approved one at a time: "+r.capability.Summary()))
		}

	case "cost":
		r.summary()

	case "session":
		state := r.loop.Session.State()
		r.out.line("  " + state.ID)
		r.out.line("  target   " + state.Target)
		r.out.line("  catalog  " + state.CatalogRevision)
		r.out.line("  messages " + fmt.Sprint(len(state.Messages)))
		r.out.line("  log      " + r.loop.Session.Path())

	case "sandbox":
		r.out.line("  platform  " + r.capability.Platform)
		r.out.line("  mechanism " + string(r.capability.Mechanism))
		r.out.line("  " + r.capability.Summary())

	default:
		r.out.Notice("error", "unknown command "+name+"; try /help")
	}

	r.out.flush()
	return false
}

func (r *repl) switchTier(ctx context.Context, id string) {
	tier, ok := r.config.Tier(id)
	if !ok {
		r.out.Notice("error", "no tier "+id+" is configured; try /tiers")
		return
	}
	if tier.ID == r.tier.ID {
		r.out.line("  already on " + r.tierLine())
		return
	}

	probed, client, note, err := r.providers.probeTierFallback(ctx, tier)
	if err != nil {
		r.out.Notice("error", err.Error())
		return
	}
	if note != "" {
		r.out.Notice("warn", note)
		r.loop.Session.AppendNote("warn", note)
	}

	r.tier = probed
	r.loop.Target = probed.Target
	// A tier may cross providers, so the adapter moves with the target. So does
	// the cache: markers, minimums, and observed state all belong to a target,
	// and carrying one target's tracker onto another would attribute its cache
	// to a server that never held it.
	abandoned := abandonedCacheNote(r.loop.Cache, r.catalog, time.Now())
	r.loop.Provider = client
	r.loop.Cache = cacheFor(probed.Target, r.catalog)
	r.out.line("  now on " + r.tierLine())

	// Cache state is scoped to a target, so a switch abandons whatever was
	// warm on the old one. When that warmth can be priced honestly the
	// modeled number is the note; otherwise the fact is stated without one.
	if abandoned != "" {
		r.out.line(r.out.style(dim, "  "+abandoned))
	} else if info, _, ok := r.catalog.Lookup(probed.Target); ok && !info.Free() {
		r.out.line(r.out.style(dim, "  a target switch leaves the previous target's cache behind"))
	}
}

// summary reports tokens and, where the catalog can price them, dollars. The
// figure is an estimate against a named catalog revision and a reconciliation
// aid, never a substitute for the provider's invoice (§15).
func (r *repl) summary() {
	for _, line := range summaryLines(r.loop.Session.State(), r.catalog, r.loop.Target) {
		r.out.line(r.out.style(dim, line))
	}
	r.out.flush()
}

// summaryLines is the UI-independent half of the cost report, shared with the
// TUI's /cost.
func summaryLines(state session.State, cat *catalog.Catalog, target provider.RouteTarget) []string {
	line := fmt.Sprintf("  %d model calls, %d tokens in, %d tokens out",
		state.Calls, state.Usage.InputTokens, state.Usage.OutputTokens)
	if state.Usage.CacheReadTokens > 0 || state.Usage.CacheWriteTokens > 0 {
		line += fmt.Sprintf(", %d cache read, %d cache write",
			state.Usage.CacheReadTokens, state.Usage.CacheWriteTokens)
	}
	lines := []string{line}

	// The three zero-dollar meterings stay distinct here for the same reason
	// they are distinct in the catalog (§4): a local model consumed nothing
	// scarce, a plan target consumed quota, and reporting either as the other
	// tells the user the wrong thing about what just ran out.
	info, _, ok := cat.Lookup(target)
	switch {
	case !ok:
		lines = append(lines, "  no catalog entry for this target, so nothing was priced")
	case info.Metering == catalog.Local:
		lines = append(lines, "  runs locally, so there is nothing to bill")
	case info.Metering == catalog.Plan:
		lines = append(lines, "  billed as a plan; quota, not dollars, is what this consumed")
	case info.Free():
		lines = append(lines, "  no per-token cost recorded for this target")
	default:
		lines = append(lines, fmt.Sprintf("  estimated %s against catalog %s",
			catalog.Money(state.CostMicroUSD), state.CatalogRevision))
	}
	return lines
}
