// Command sb is Switchboard's terminal entry point.
//
// This is the phase-0 REPL: a plain line-oriented shell over the agent library,
// built to exercise the loop and nothing more. The Bubble Tea interface arrives
// in phase 3, after the routing thesis has been measured, so that neither the
// measurement nor the decision to continue depends on it (§19.2).
package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/cjvana/switchboard/internal/agent"
	"github.com/cjvana/switchboard/internal/execution"
	"github.com/cjvana/switchboard/internal/permission"
	"github.com/cjvana/switchboard/internal/provider"
	"github.com/cjvana/switchboard/internal/provider/ollama"
	"github.com/cjvana/switchboard/internal/session"
	"github.com/cjvana/switchboard/internal/tools"
)

func main() {
	if err := run(); err != nil {
		if errors.Is(err, context.Canceled) {
			os.Exit(130)
		}
		fmt.Fprintln(os.Stderr, "sb: "+err.Error())
		os.Exit(1)
	}
}

type options struct {
	model     string
	host      string
	mode      string
	think     string
	workspace string
	prompt    string
	resume    string
	cont      bool
	list      bool
}

func run() error {
	var opts options
	flag.StringVar(&opts.model, "model", os.Getenv("SB_MODEL"), "Ollama model to bind, for example qwen3.5:9b-mlx")
	flag.StringVar(&opts.host, "host", "", "Ollama base URL (default $OLLAMA_HOST or http://localhost:11434)")
	flag.StringVar(&opts.mode, "mode", "default", "permission mode: plan, default, acceptEdits, or bypass")
	flag.StringVar(&opts.think, "think", "", "reasoning effort: low, medium, high, or max")
	flag.StringVar(&opts.workspace, "workspace", "", "workspace root (default: current directory)")
	flag.StringVar(&opts.prompt, "p", "", "run a single prompt and exit")
	flag.StringVar(&opts.resume, "resume", "", "resume a session by id")
	flag.BoolVar(&opts.cont, "continue", false, "resume the most recent session for this workspace")
	flag.BoolVar(&opts.list, "sessions", false, "list sessions for this workspace and exit")
	flag.Parse()

	ctx := context.Background()

	workspace := opts.workspace
	if workspace == "" {
		cwd, err := os.Getwd()
		if err != nil {
			return err
		}
		workspace = cwd
	}
	workspace, err := filepath.Abs(workspace)
	if err != nil {
		return err
	}

	store, err := session.DefaultStore()
	if err != nil {
		return err
	}
	if opts.list {
		return listSessions(store, workspace)
	}

	mode, err := permission.ParseMode(opts.mode)
	if err != nil {
		return err
	}

	client := ollama.New(ollama.WithBaseURL(opts.host))

	sess, target, resumed, err := openSession(ctx, store, client, workspace, &opts)
	if err != nil {
		return err
	}
	defer sess.Close()

	capability := execution.Detect()

	registry, err := tools.NewRegistry(workspace, capability)
	if err != nil {
		return err
	}

	out := newRenderer(os.Stdout)
	in := bufio.NewReader(os.Stdin)

	loop := &agent.Loop{
		Provider: client,
		Target:   target,
		Tools:    registry,
		Perms:    permission.NewEngine(mode, capability),
		Asker:    &terminalAsker{in: in, out: out},
		Session:  sess,
		Observer: out,
		System:   agent.SystemPrompt(workspace, mode, capability),
	}

	r := &repl{loop: loop, out: out, in: in, capability: capability, workspace: workspace}
	r.banner(sess, target, resumed)

	if opts.prompt != "" {
		return r.once(ctx, opts.prompt)
	}
	return r.interactive(ctx)
}

// openSession resolves the session and the route target together, because a
// resumed session names the model it was recorded with and a new one is named
// by the target.
func openSession(ctx context.Context, store *session.Store, client *ollama.Client, workspace string, opts *options) (*session.Session, provider.RouteTarget, bool, error) {
	var (
		sess *session.Session
		err  error
	)
	switch {
	case opts.resume != "":
		sess, err = store.Open(opts.resume)
	case opts.cont:
		sess, err = store.Latest(workspace)
		if errors.Is(err, session.ErrNoSessions) {
			err = fmt.Errorf("no session to continue in %s", workspace)
		}
	default:
		target, buildErr := buildTarget(ctx, client, opts.model, opts.think)
		if buildErr != nil {
			return nil, provider.RouteTarget{}, false, buildErr
		}
		sess, err = store.Create(workspace, target.ID())
		return sess, target, false, err
	}

	if err != nil {
		return nil, provider.RouteTarget{}, false, err
	}
	adoptRecordedModel(sess, opts)

	target, err := buildTarget(ctx, client, opts.model, opts.think)
	if err != nil {
		sess.Close()
		return nil, provider.RouteTarget{}, false, err
	}
	return sess, target, true, nil
}

// adoptRecordedModel keeps a resumed session on the model it was recorded with
// unless the user asked for a different one.
//
// It reads the model back out of the target ID, which is serviceable but not
// where this belongs: the versioned target catalog in §4 owns target identity,
// and this parsing goes away with it in phase 1.
func adoptRecordedModel(sess *session.Session, opts *options) {
	if opts.model != "" {
		return
	}
	parts := strings.SplitN(sess.State().Target, "/", 3)
	if len(parts) < 3 {
		return
	}
	model, _, _ := strings.Cut(parts[2], "+")
	opts.model = model
}

func buildTarget(ctx context.Context, client *ollama.Client, model, think string) (provider.RouteTarget, error) {
	if model == "" {
		return provider.RouteTarget{}, noModelError(ctx, client)
	}

	target := ollama.Target(model)
	if think != "" {
		target.Params.Reasoning = &provider.Reasoning{Enabled: true, Effort: think}
	}

	probe, err := client.Probe(ctx, target)
	if err != nil {
		return provider.RouteTarget{}, err
	}
	if !probe.Reachable {
		return provider.RouteTarget{}, fmt.Errorf("no Ollama server responded: %s", probe.Detail)
	}
	if !probe.ModelPresent {
		return provider.RouteTarget{}, fmt.Errorf("%s\nrun: ollama pull %s", probe.Detail, model)
	}
	if probe.Tools == provider.ToolsNone {
		return provider.RouteTarget{}, fmt.Errorf(
			"%s does not support tool calling, so it cannot drive the agent loop", model)
	}
	return target, nil
}

func noModelError(ctx context.Context, client *ollama.Client) error {
	models, err := client.Models(ctx)
	if err != nil {
		return fmt.Errorf("no model selected, and the Ollama server could not be reached: %w", err)
	}
	if len(models) == 0 {
		return errors.New("no model selected, and this Ollama server has none pulled")
	}
	return fmt.Errorf("no model selected. Pass -model or set SB_MODEL. Available:\n  %s",
		strings.Join(models, "\n  "))
}

func listSessions(store *session.Store, workspace string) error {
	infos, err := store.List(workspace)
	if err != nil {
		return err
	}
	if len(infos) == 0 {
		fmt.Printf("no sessions recorded for %s\n", workspace)
		return nil
	}
	for _, info := range infos {
		fmt.Printf("%s  %s  %d bytes\n", info.ID, info.Modified.Local().Format("2006-01-02 15:04:05"), info.Size)
	}
	return nil
}
