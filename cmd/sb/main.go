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
	"github.com/cjvana/switchboard/internal/catalog"
	"github.com/cjvana/switchboard/internal/config"
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
	tier      string
	host      string
	mode      string
	think     string
	workspace string
	prompt    string
	resume    string
	cont      bool
	list      bool
	showTiers bool
}

func run() error {
	// A subcommand is dispatched before flag parsing, because `sb auth login`
	// takes a credential rather than flags and must not be reachable in a form
	// that puts one on the command line.
	if len(os.Args) > 1 && os.Args[1] == "auth" {
		cfg, err := config.Load()
		if err != nil {
			return err
		}
		return runAuth(context.Background(), os.Args[2:], cfg)
	}

	var opts options
	flag.StringVar(&opts.model, "model", os.Getenv("SB_MODEL"), "Ollama model to bind directly, bypassing the configured tiers")
	flag.StringVar(&opts.tier, "tier", "", "tier to start on, for example t2 (default: the lowest configured tier)")
	flag.StringVar(&opts.host, "host", "", "Ollama base URL (default $OLLAMA_HOST or http://localhost:11434)")
	flag.StringVar(&opts.mode, "mode", "default", "permission mode: plan, default, acceptEdits, or bypass")
	flag.StringVar(&opts.think, "think", "", "reasoning effort: low, medium, high, or max")
	flag.StringVar(&opts.workspace, "workspace", "", "workspace root (default: current directory)")
	flag.StringVar(&opts.prompt, "p", "", "run a single prompt and exit")
	flag.StringVar(&opts.resume, "resume", "", "resume a session by id")
	flag.BoolVar(&opts.cont, "continue", false, "resume the most recent session for this workspace")
	flag.BoolVar(&opts.list, "sessions", false, "list sessions for this workspace and exit")
	flag.BoolVar(&opts.showTiers, "tiers", false, "list the configured tiers and exit")
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

	cat, err := catalog.Load()
	if err != nil {
		return err
	}
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	if opts.showTiers {
		return listTiers(cfg, cat)
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

	reg := newProviders(opts.host, cfg)

	sess, tier, client, resumed, err := openSession(ctx, store, reg, cfg, cat, workspace, &opts)
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
		Target:   tier.Target,
		Tools:    registry,
		Perms:    permission.NewEngine(mode, capability),
		Asker:    &terminalAsker{in: in, out: out},
		Session:  sess,
		Observer: out,
		Catalog:  cat,
		System:   agent.SystemPrompt(workspace, mode, capability),
	}

	r := &repl{
		loop:       loop,
		out:        out,
		in:         in,
		capability: capability,
		workspace:  workspace,
		config:     cfg,
		catalog:    cat,
		tier:       tier,
		providers:  reg,
	}
	r.banner(sess, resumed)

	if opts.prompt != "" {
		return r.once(ctx, opts.prompt)
	}
	return r.interactive(ctx)
}

// openSession resolves the session and the starting tier together, because a
// resumed session names the target it was recorded with and a new one is named
// by the tier.
func openSession(
	ctx context.Context,
	store *session.Store,
	reg *providers,
	cfg *config.Config,
	cat *catalog.Catalog,
	workspace string,
	opts *options,
) (*session.Session, config.Tier, provider.Provider, bool, error) {
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
		tier, client, buildErr := resolveTier(ctx, reg, cfg, opts, "")
		if buildErr != nil {
			return nil, config.Tier{}, nil, false, buildErr
		}
		sess, err = store.Create(workspace, tier.Target.ID(), cat.Revision)
		return sess, tier, client, false, err
	}

	if err != nil {
		return nil, config.Tier{}, nil, false, err
	}

	tier, client, err := resolveTier(ctx, reg, cfg, opts, sess.State().Target)
	if err != nil {
		sess.Close()
		return nil, config.Tier{}, nil, false, err
	}
	return sess, tier, client, true, nil
}

// resolveTier picks the starting target. An explicit model wins, then an
// explicit tier, then the target a resumed session recorded, then the bottom of
// the ladder.
func resolveTier(ctx context.Context, reg *providers, cfg *config.Config, opts *options, recorded string) (config.Tier, provider.Provider, error) {
	switch {
	case opts.model != "":
		target := ollama.Target(opts.model)
		applyEffort(&target, opts.think)
		return reg.probeTier(ctx, config.Tier{ID: "-model", Label: "ad hoc", Target: target})

	case opts.tier != "":
		tier, ok := cfg.Tier(opts.tier)
		if !ok {
			return config.Tier{}, nil, fmt.Errorf("no tier %s is configured; run sb -tiers to see the ladder", opts.tier)
		}
		applyEffort(&tier.Target, opts.think)
		return reg.probeTier(ctx, tier)

	case recorded != "":
		// A resumed session stays on the target it was recorded with unless the
		// user asked otherwise, so replaying it means what it meant.
		if tier, ok := tierForTarget(cfg, recorded); ok {
			return reg.probeTier(ctx, tier)
		}
		target, err := parseRecordedTarget(recorded)
		if err != nil {
			return config.Tier{}, nil, err
		}
		applyEffort(&target, opts.think)
		return reg.probeTier(ctx, config.Tier{ID: "-resumed", Label: "resumed", Target: target})
	}

	tier, ok := cfg.Default()
	if !ok {
		return config.Tier{}, nil, noTargetError(ctx, reg.ollama, cfg)
	}
	applyEffort(&tier.Target, opts.think)
	return reg.probeTier(ctx, tier)
}

func applyEffort(target *provider.RouteTarget, effort string) {
	if effort != "" {
		target.Params.Reasoning = &provider.Reasoning{Enabled: true, Effort: effort}
	}
}

func tierForTarget(cfg *config.Config, recorded string) (config.Tier, bool) {
	for _, t := range cfg.Tiers {
		if string(t.Target.ID()) == recorded {
			return t, true
		}
	}
	return config.Tier{}, false
}

// parseRecordedTarget reads a target back out of a session record. The catalog
// owns target identity, so this is deliberately narrow: it recovers what was
// recorded rather than inventing a target the user never configured.
func parseRecordedTarget(recorded string) (provider.RouteTarget, error) {
	parts := strings.SplitN(recorded, "/", 3)
	if len(parts) < 3 {
		return provider.RouteTarget{}, fmt.Errorf("session recorded an unreadable target %q", recorded)
	}
	model, _, _ := strings.Cut(parts[2], "+")
	return provider.RouteTarget{Provider: parts[0], Surface: parts[1], ModelID: model}, nil
}

func noTargetError(ctx context.Context, client *ollama.Client, cfg *config.Config) error {
	models, err := client.Models(ctx)
	if err != nil {
		return fmt.Errorf("no tiers configured and no model given, and the Ollama server could not be reached: %w", err)
	}

	var b strings.Builder
	b.WriteString("no tiers configured and no -model given.\n")
	if cfg.Path != "" {
		fmt.Fprintf(&b, "\nConfigure a ladder in %s:\n\n", cfg.Path)
		b.WriteString("  [tiers.t1]\n  label = \"light\"\n  model = \"ollama/<model>\"\n\n")
		b.WriteString("  [tiers.t2]\n  label = \"deep\"\n  model = \"ollama/<model>\"\n")
	}
	if len(models) > 0 {
		fmt.Fprintf(&b, "\nModels this server has pulled:\n  %s", strings.Join(models, "\n  "))
	}
	return errors.New(b.String())
}

func listTiers(cfg *config.Config, cat *catalog.Catalog) error {
	if len(cfg.Tiers) == 0 {
		fmt.Printf("no tiers configured in %s\n", cfg.Path)
		return nil
	}
	fmt.Printf("catalog %s (%s)\n\n", cat.Revision, cat.Source)
	for _, t := range cfg.Tiers {
		fmt.Println(t)
		info, confidence, ok := cat.Lookup(t.Target)
		if !ok {
			fmt.Println("      no catalog entry")
			continue
		}
		fmt.Printf("      %s", describePricing(info))
		if confidence == catalog.Prior {
			fmt.Print("  (surface default, not verified for this model)")
		}
		fmt.Println()
	}
	return nil
}

func describePricing(info catalog.ModelInfo) string {
	switch info.Metering {
	case catalog.Local:
		return "runs locally, nothing meters it"
	case catalog.Plan:
		// Not the same as free. Nothing here models quota yet, so the honest
		// answer names what is actually finite rather than reporting zero.
		return "billed as a plan, not per token; quota rather than cost is the limit"
	}
	if info.Free() {
		return "no per-token cost recorded"
	}
	band, ok := info.Band(0)
	if !ok {
		return "no price band"
	}
	return fmt.Sprintf("%s in / %s out per MTok", band.InputPerMTok, band.OutputPerMTok)
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
