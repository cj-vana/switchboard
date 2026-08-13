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
	"github.com/cjvana/switchboard/internal/execution"
	"github.com/cjvana/switchboard/internal/permission"
	"github.com/cjvana/switchboard/internal/provider"
	"github.com/cjvana/switchboard/internal/session"
)

type repl struct {
	loop       *agent.Loop
	out        *renderer
	in         *bufio.Reader
	capability execution.Capability
	workspace  string
}

func (r *repl) banner(sess *session.Session, target provider.RouteTarget, resumed bool) {
	state := sess.State()

	r.out.line(r.out.style(bold, "switchboard") + " " + r.out.style(dim, string(target.ID())))
	r.out.line(r.out.style(dim, "  workspace  "+r.workspace))
	r.out.line(r.out.style(dim, "  mode       "+string(r.loop.Perms.Mode())))
	r.out.line(r.out.style(dim, "  sandbox    "+r.capability.Summary()))

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
			if done := r.command(input); done {
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
func (r *repl) command(input string) bool {
	name, rest, _ := strings.Cut(strings.TrimPrefix(input, "/"), " ")
	rest = strings.TrimSpace(rest)

	switch name {
	case "exit", "quit":
		return true

	case "help":
		r.out.line("  /mode [plan|default|acceptEdits|bypass]   show or change the permission mode")
		r.out.line("  /cost                                     tokens used in this session")
		r.out.line("  /session                                  session id, target, and message count")
		r.out.line("  /sandbox                                  what isolation this host provides")
		r.out.line("  /exit                                     leave")

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

// summary reports tokens rather than money. Ollama is free to run locally, and
// pricing belongs to the target catalog and cost model in phase 1, which is
// where an estimate can name the band and catalog revision it came from.
func (r *repl) summary() {
	state := r.loop.Session.State()
	r.out.line(r.out.style(dim, fmt.Sprintf("  %d model calls, %d tokens in, %d tokens out",
		state.Calls, state.Usage.InputTokens, state.Usage.OutputTokens)))
	r.out.flush()
}
