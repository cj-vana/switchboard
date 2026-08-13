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

	"github.com/cjvana/switchboard/internal/agent"
	"github.com/cjvana/switchboard/internal/catalog"
	"github.com/cjvana/switchboard/internal/config"
	"github.com/cjvana/switchboard/internal/execution"
	"github.com/cjvana/switchboard/internal/permission"
	"github.com/cjvana/switchboard/internal/session"
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
}

func (r *repl) banner(sess *session.Session, resumed bool) {
	state := sess.State()

	r.out.line(r.out.style(bold, "switchboard") + " " + r.out.style(dim, r.tierLine()))
	r.out.line(r.out.style(dim, "  workspace  "+r.workspace))
	r.out.line(r.out.style(dim, "  mode       "+string(r.loop.Perms.Mode())))
	r.out.line(r.out.style(dim, "  sandbox    "+r.capability.Summary()))
	r.out.line(r.out.style(dim, "  catalog    "+r.catalog.Revision+" ("+r.catalog.Source+")"))

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

	err := r.loop.Turn(turnCtx, input)
	r.out.endTurn()
	return err
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

	probed, client, err := r.providers.probeTier(ctx, tier)
	if err != nil {
		r.out.Notice("error", err.Error())
		return
	}

	r.tier = probed
	r.loop.Target = probed.Target
	// A tier may cross providers, so the adapter moves with the target.
	r.loop.Provider = client
	r.out.line("  now on " + r.tierLine())

	// Cache state is scoped to a target, so a switch abandons whatever was warm
	// on the old one. The breakpoint manager and cost estimator that would put
	// a number on that arrive in phase 2a; until then the fact is stated rather
	// than priced.
	if info, _, ok := r.catalog.Lookup(probed.Target); ok && !info.Free() {
		r.out.line(r.out.style(dim, "  a target switch leaves the previous target's cache behind; "+
			"what that costs is not modelled yet"))
	}
}

// summary reports tokens and, where the catalog can price them, dollars. The
// figure is an estimate against a named catalog revision and a reconciliation
// aid, never a substitute for the provider's invoice (§15).
func (r *repl) summary() {
	state := r.loop.Session.State()

	line := fmt.Sprintf("  %d model calls, %d tokens in, %d tokens out",
		state.Calls, state.Usage.InputTokens, state.Usage.OutputTokens)
	if state.Usage.CacheReadTokens > 0 || state.Usage.CacheWriteTokens > 0 {
		line += fmt.Sprintf(", %d cache read, %d cache write",
			state.Usage.CacheReadTokens, state.Usage.CacheWriteTokens)
	}
	r.out.line(r.out.style(dim, line))

	info, _, ok := r.catalog.Lookup(r.loop.Target)
	switch {
	case !ok:
		r.out.line(r.out.style(dim, "  no catalog entry for this target, so nothing was priced"))
	case info.Free():
		r.out.line(r.out.style(dim, "  runs locally, so there is nothing to bill"))
	default:
		r.out.line(r.out.style(dim, fmt.Sprintf("  estimated %s against catalog %s",
			catalog.Money(state.CostMicroUSD), state.CatalogRevision)))
	}
	r.out.flush()
}
