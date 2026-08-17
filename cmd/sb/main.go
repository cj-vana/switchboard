// Command sb is Switchboard's terminal entry point.
//
// Interactive sessions open the Bubble Tea TUI; the phase-0 line-oriented REPL
// remains behind -repl and is what the phase gates and single-prompt (-p) runs
// use, because both need a scriptable surface.
package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/cj-vana/switchboard/internal/agent"
	"github.com/cj-vana/switchboard/internal/catalog"
	"github.com/cj-vana/switchboard/internal/checkpoint"
	"github.com/cj-vana/switchboard/internal/config"
	"github.com/cj-vana/switchboard/internal/execution"
	"github.com/cj-vana/switchboard/internal/permission"
	"github.com/cj-vana/switchboard/internal/provider"
	"github.com/cj-vana/switchboard/internal/provider/ollama"
	route "github.com/cj-vana/switchboard/internal/router"
	"github.com/cj-vana/switchboard/internal/session"
	"github.com/cj-vana/switchboard/internal/tools"
	"github.com/cj-vana/switchboard/internal/trust"
)

func main() {
	sweepOldBinary()
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
	output    string
	resume    string
	cont      bool
	list      bool
	showTiers bool
	repl      bool
	version   bool

	// allowSecrets widens the outbound credential gate for a scripted run,
	// the way -mode widens permissions: deliberately, on the command line,
	// never by default.
	allowSecrets bool
}

func run() error {
	// A subcommand is dispatched before flag parsing, because `sb auth login`
	// takes a credential rather than flags and must not be reachable in a form
	// that puts one on the command line.
	if len(os.Args) > 1 && os.Args[1] == "completion" {
		if len(os.Args) > 3 {
			return fmt.Errorf("sb completion takes one shell; %q is extra", os.Args[3])
		}
		shell := ""
		if len(os.Args) > 2 {
			shell = os.Args[2]
		}
		return runCompletionCLI(os.Stdout, shell)
	}
	if len(os.Args) > 1 && os.Args[1] == "auth" {
		cfg, err := config.Load()
		if err != nil {
			return err
		}
		return runAuth(context.Background(), os.Args[2:], cfg)
	}
	if len(os.Args) > 1 && os.Args[1] == "update" {
		if len(os.Args) > 2 {
			return fmt.Errorf("sb update takes no argument; %q was ignored by nothing", os.Args[2])
		}
		cfg, err := config.Load()
		if err != nil {
			return err
		}
		return runUpdateCLI(context.Background(), cfg)
	}
	if len(os.Args) > 1 && os.Args[1] == "doctor" {
		if len(os.Args) > 2 {
			return fmt.Errorf("sb doctor takes no argument; %q is not one", os.Args[2])
		}
		cwd, err := os.Getwd()
		if err != nil {
			return err
		}
		cfg, err := config.Load()
		if err != nil {
			return err
		}
		cat, err := catalog.Load()
		if err != nil {
			return err
		}
		return runDoctorCLI(context.Background(), os.Stdout, cfg, cat, newProviders("", cfg), cwd)
	}
	if len(os.Args) > 1 && os.Args[1] == "cost" {
		if len(os.Args) > 2 {
			return fmt.Errorf("sb cost takes no argument; %q is not one", os.Args[2])
		}
		cwd, err := os.Getwd()
		if err != nil {
			return err
		}
		store, err := session.DefaultStore()
		if err != nil {
			return err
		}
		cat, err := catalog.Load()
		if err != nil {
			return err
		}
		return runCostCLI(os.Stdout, store, cat, cwd)
	}
	if len(os.Args) > 1 && os.Args[1] == "find" {
		cwd, err := os.Getwd()
		if err != nil {
			return err
		}
		store, err := session.DefaultStore()
		if err != nil {
			return err
		}
		return runFindCLI(os.Stdout, store, cwd, strings.Join(os.Args[2:], " "))
	}
	if len(os.Args) > 1 && os.Args[1] == "stats" {
		cwd, err := os.Getwd()
		if err != nil {
			return err
		}
		cfg, err := config.Load()
		if err != nil {
			return err
		}
		store, err := session.DefaultStore()
		if err != nil {
			return err
		}
		cat, err := catalog.Load()
		if err != nil {
			return err
		}
		scope := ""
		if len(os.Args) > 2 {
			scope = os.Args[2]
		}
		return runStatsCLI(os.Stdout, store, cat, cfg, cwd, scope)
	}
	if len(os.Args) > 1 && os.Args[1] == "races" {
		cwd, err := os.Getwd()
		if err != nil {
			return err
		}
		store, err := session.DefaultStore()
		if err != nil {
			return err
		}
		if len(os.Args) > 2 {
			if os.Args[2] != "all" {
				return fmt.Errorf("sb races takes no argument, or all; %q is neither", os.Args[2])
			}
			return runRacesAllCLI(os.Stdout, store)
		}
		return runRacesCLI(os.Stdout, store, cwd)
	}
	if len(os.Args) > 1 && os.Args[1] == "blame" {
		if len(os.Args) < 3 {
			return fmt.Errorf("sb blame takes the file to annotate")
		}
		if len(os.Args) > 3 {
			return fmt.Errorf("sb blame takes one file; %q is extra", os.Args[3])
		}
		cwd, err := os.Getwd()
		if err != nil {
			return err
		}
		store, err := session.DefaultStore()
		if err != nil {
			return err
		}
		return runBlameCLI(os.Stdout, store, cwd, os.Args[2])
	}

	var opts options
	// The flag package prints flags alone; the subcommands live in the
	// same list the shell completions are pinned to, so -h, tab, and the
	// dispatch cannot name three different tools.
	flag.Usage = func() {
		fmt.Fprintf(flag.CommandLine.Output(),
			"usage: sb [flags]            an interactive session in this workspace\n"+
				"       sb <subcommand>       %s\n\nflags:\n",
			strings.Join(completionSubcommands, ", "))
		flag.PrintDefaults()
	}
	flag.StringVar(&opts.model, "model", os.Getenv("SB_MODEL"), "Ollama model to bind directly, bypassing the configured tiers")
	flag.StringVar(&opts.tier, "tier", "", "tier to start on, for example t2 (default: the lowest configured tier)")
	flag.StringVar(&opts.host, "host", "", "Ollama base URL (default $OLLAMA_HOST or http://localhost:11434)")
	flag.StringVar(&opts.mode, "mode", "default", "permission mode: plan, default, acceptEdits, or bypass")
	flag.StringVar(&opts.think, "think", "", "reasoning effort: low, medium, high, or max")
	flag.StringVar(&opts.workspace, "workspace", "", "workspace root (default: current directory)")
	flag.StringVar(&opts.prompt, "p", "", "run a single prompt and exit; piped stdin is attached to it")
	flag.StringVar(&opts.output, "output", "text", "what a -p run prints: text, or json for one machine-readable result line")
	flag.StringVar(&opts.resume, "resume", "", "resume a session by id")
	flag.BoolVar(&opts.cont, "continue", false, "resume the most recent session for this workspace")
	flag.BoolVar(&opts.list, "sessions", false, "list sessions for this workspace and exit")
	flag.BoolVar(&opts.showTiers, "tiers", false, "list the configured tiers and exit")
	flag.BoolVar(&opts.repl, "repl", false, "use the line-oriented REPL instead of the TUI")
	flag.BoolVar(&opts.version, "version", false, "print the version and exit")
	flag.BoolVar(&opts.allowSecrets, "allow-secrets", false, "send a -p prompt even when it contains something key-shaped")
	flag.Parse()

	if opts.version {
		v := currentVersion()
		if v == "" {
			v = "dev"
		}
		fmt.Println("sb " + v)
		return nil
	}

	switch opts.output {
	case "", "text":
	case "json":
		if opts.prompt == "" {
			return errors.New("-output json reports one completed prompt, so it needs -p")
		}
	default:
		return fmt.Errorf("unknown output format %q: text or json", opts.output)
	}

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

	// An empty ladder on an interactive terminal is the first run, not an
	// error: walk through binding t1 before anything needs a target. Every
	// non-interactive path still gets the explanatory error from resolveTier,
	// because a wizard on a pipe would hang whatever is driving it.
	onboarded := false
	if len(cfg.Tiers) == 0 && opts.model == "" && opts.resume == "" && !opts.cont &&
		!opts.repl && opts.prompt == "" && isTerminal(os.Stdin) && isTerminal(os.Stdout) {
		if err := runOnboarding(reg, cat, cfg); err != nil {
			return err
		}
		onboarded = true
	}

	var chosen route.Decision
	sess, tier, client, resumed, fallbackNote, err := openSession(ctx, store, reg, cfg, cat, workspace, &opts, &chosen)
	if err != nil {
		return err
	}
	defer sess.Close()

	capability := execution.Detect()

	registry, err := tools.NewRegistry(workspace, capability)
	if err != nil {
		return err
	}

	// The undo recorder captures prior file states before write and edit
	// mutate them, scoped per turn by the loop.
	undoRec := checkpoint.NewRecorder()
	registry.SetCheckpoints(undoRec)

	// Structural search joins the suite only where the machine has the
	// binary: looked up once, so the frozen zone never changes mid-session.
	addStructuralSearch(registry)

	// Skills load the same way named agents do — both directories, no trust
	// gate, nothing executing at read time — and with none found the tool is
	// absent, keeping the schemas byte-identical.
	skillList, skillNotes := addSkills(registry, workspace)

	// The trust store is opened before MCP assembly because it is what
	// decides whether a repository's declared servers may start.
	trustStore, trustErr := trust.Open()
	mcpEnv, mcpRules := connectMCP(ctx, workspace, trustStore, registry)
	defer mcpEnv.Close()

	hookSet, hookNotes := loadHooks(workspace, trustStore)
	for _, n := range hookNotes {
		mcpEnv.add(n)
	}
	for _, n := range skillNotes {
		mcpEnv.add(n)
	}

	// Precise symbol lookup joins the suite when a module, a server, and a
	// trust grant line up; the note says which one did not.
	lspServer, lspNote := setupLSP(workspace, trustStore, registry)
	if lspServer != nil {
		defer lspServer.Close()
	}
	if lspNote != "" {
		mcpEnv.add(mcpNote{"", lspNote})
	}

	// A fallback substitution renders before any content is sent and is
	// recorded on the session (§5.4): the user must know which server this
	// conversation is actually going to.
	if fallbackNote != "" {
		mcpEnv.add(mcpNote{"warn", fallbackNote})
		if err := sess.AppendNote("warn", fallbackNote); err != nil {
			return err
		}
	}

	// §6 is only live if something wires it. The loop assembles a request from
	// the session by default, so without this the zones, the breakpoint
	// manager, and the tracker are all present and never consulted.
	cache := cacheFor(tier.Target, cat)

	loop := &agent.Loop{
		Provider:    client,
		Target:      tier.Target,
		Tools:       registry,
		Perms:       permission.NewEngine(mode, capability, mcpRules...),
		Session:     sess,
		Catalog:     cat,
		Cache:       cache,
		System:      agent.SystemPrompt(workspace, mode, capability),
		Hooks:       hookSet,
		Checkpoints: undoRec,
	}

	// The ceiling gates the loop before each call, whatever surface drives
	// it; /budget adjusts the shared state mid-session.
	budget := &budgetState{}
	budget.set(cfg.Budget)
	loop.Budget = primaryGate(budget, loop, cat)

	// The delegate tool joins after the loop exists because its subagents
	// share the loop's permission engine and asker; it still lands before the
	// first request, which is what the frozen zone requires.
	agents, agentNotes, err := registerDelegate(registry, cfg, cat, reg, loop, hookSet, capability, workspace, undoRec, budget, skillList)
	if err != nil {
		mcpEnv.add(mcpNote{"warn", "delegate unavailable: " + err.Error()})
	}
	for _, n := range agentNotes {
		mcpEnv.add(mcpNote{"warn", n})
	}

	// The sticky primary starts wherever routing landed, and the watcher feeds
	// it what happens inside a turn. Without that connection the escalation
	// policy is built and never consulted.
	startRank := 0
	for i, t := range cfg.Tiers {
		if t.ID == tier.ID {
			startRank = i
		}
	}
	sticky := route.NewSticky(route.Policy{}, startRank)
	if opts.tier != "" || opts.model != "" {
		sticky.Pin(startRank)
	}

	var routeDec *route.Decision
	if chosen.Source != "" {
		routeDec = &chosen
	}

	// The TUI is the default interactive surface; the REPL remains for
	// scripting, for gates, and for terminals that are not terminals. A single
	// -p prompt keeps the plain renderer either way.
	if !opts.repl && opts.prompt == "" && isTerminal(os.Stdin) && isTerminal(os.Stdout) {
		updateCheck := cfg.UpdateCheck && os.Getenv("SB_NO_UPDATE_CHECK") == ""
		return runTUI(loop, store, cfg, cat, capability, workspace, tier, reg, sticky, routeDec, sess, resumed, updateCheck, trustStore, trustErr, mcpEnv, undoRec, agents, agentNotes, budget, skillList, onboarded)
	}

	// With -output json, stdout carries exactly one JSON line and nothing
	// else; the transcript still renders, on stderr, so a person watching a
	// scripted run sees the work happen.
	outDest := os.Stdout
	if opts.output == "json" {
		outDest = os.Stderr
	}
	out := newRenderer(outDest)
	in := bufio.NewReader(os.Stdin)

	// Piped stdin under -p is content, not a control channel: it rides into
	// the prompt as an attachment, and the asker is left unset so anything
	// needing approval is refused with a reason rather than answered by
	// whatever bytes happened to be next in the pipe. Widening -mode is the
	// deliberate way to let a scripted run do more.
	piped := opts.prompt != "" && !isTerminal(os.Stdin)
	if piped {
		data, err := io.ReadAll(os.Stdin)
		if err != nil {
			return fmt.Errorf("reading piped stdin: %w", err)
		}
		opts.prompt = attachPipedInput(opts.prompt, data)
	} else {
		loop.Asker = &terminalAsker{in: in, out: out}
	}
	loop.Observer = out
	subagentForward.set(out)

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
		budget:     budget,
	}
	r.route = routeDec
	r.sticky = sticky
	loop.Observer = newWatcher(out, sticky, len(cfg.Tiers)-1, r.moveTo)
	r.watcher = loop.Observer.(*watcher)

	r.banner(sess, resumed)
	// The REPL drains what buffered and attaches no live target: the
	// renderer is driven from the loop's goroutine, and a client's read
	// loop writing to it concurrently would race. Later notes buffer,
	// capped, and are simply not the REPL's concern.
	for _, n := range mcpEnv.attach(nil) {
		out.Notice(n.level, n.text)
	}

	if opts.prompt != "" {
		// The gate has no one to ask on this surface, so a key-shaped string
		// refuses the run outright; -allow-secrets is the deliberate widening,
		// the way -mode is for permissions.
		if err := refuseLeakedSecrets(opts.prompt, opts.allowSecrets); err != nil {
			return err
		}
		err := r.once(ctx, opts.prompt)
		if opts.output == "json" {
			rep := buildHeadlessReport(loop.Session.State(), cat, r.tier, err)
			if wErr := writeHeadlessReport(os.Stdout, rep); wErr != nil {
				return wErr
			}
		}
		return err
	}
	return r.interactive(ctx)
}

func isTerminal(f *os.File) bool {
	info, err := f.Stat()
	return err == nil && info.Mode()&os.ModeCharDevice != 0
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
	chosen *route.Decision,
) (*session.Session, config.Tier, provider.Provider, bool, string, error) {
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
		tier, client, note, buildErr := resolveTier(ctx, reg, cfg, cat, opts, "", chosen)
		if buildErr != nil {
			return nil, config.Tier{}, nil, false, "", buildErr
		}
		sess, err = store.Create(workspace, tier.Target.ID(), cat.Revision)
		return sess, tier, client, false, note, err
	}

	if err != nil {
		return nil, config.Tier{}, nil, false, "", err
	}

	tier, client, note, err := resolveTier(ctx, reg, cfg, cat, opts, sess.State().Target, chosen)
	if err != nil {
		sess.Close()
		return nil, config.Tier{}, nil, false, "", err
	}
	return sess, tier, client, true, note, nil
}

// resolveTier picks the starting target. An explicit model wins, then an
// explicit tier, then the target a resumed session recorded, then the bottom of
// the ladder.
func resolveTier(ctx context.Context, reg *providers, cfg *config.Config, cat *catalog.Catalog, opts *options, recorded string, chosen *route.Decision) (config.Tier, provider.Provider, string, error) {
	switch {
	case opts.model != "":
		target := ollama.Target(opts.model)
		applyEffort(&target, opts.think)
		return reg.probeTierFallback(ctx, config.Tier{ID: "-model", Label: "ad hoc", Target: target})

	case opts.tier != "":
		tier, ok := cfg.Tier(opts.tier)
		if !ok {
			return config.Tier{}, nil, "", fmt.Errorf("no tier %s is configured; run sb -tiers to see the ladder", opts.tier)
		}
		applyEffort(&tier.Target, opts.think)
		return reg.probeTierFallback(ctx, tier)

	case recorded != "":
		// A resumed session stays on the target it was recorded with unless the
		// user asked otherwise, so replaying it means what it meant.
		if tier, ok := tierForTarget(cfg, recorded); ok {
			return reg.probeTierFallback(ctx, tier)
		}
		target, err := parseRecordedTarget(recorded)
		if err != nil {
			return config.Tier{}, nil, "", err
		}
		applyEffort(&target, opts.think)
		return reg.probeTierFallback(ctx, config.Tier{ID: "-resumed", Label: "resumed", Target: target})
	}

	if len(cfg.Tiers) == 0 {
		return config.Tier{}, nil, "", noTargetError(ctx, reg.ollama, cfg)
	}

	// Nothing was pinned, so the router picks. With no prompt yet this is the
	// opening choice only, which §8.3 says is worth less than the mid-task
	// adjustments; it exists so the ladder is entered deliberately rather than
	// by taking the bottom rung on principle.
	decision, err := route.Heuristic{}.Route(route.Input{
		Prompt:       opts.prompt,
		Candidates:   candidatesFor(cfg, cat, nil, 0),
		Requirements: route.Requirements{NeedsTools: true},
		// The session opens with nothing spent, so the whole ceiling is what
		// a rung's upper bound is checked against (§15).
		Budgets: route.Budgets{MaxCost: cfg.Budget},
	})
	if err != nil {
		return config.Tier{}, nil, "", err
	}
	*chosen = decision

	tier, ok := cfg.Tier(decision.Tier)
	if !ok {
		return config.Tier{}, nil, "", fmt.Errorf("the router chose %q, which is not on the ladder", decision.Tier)
	}
	applyEffort(&tier.Target, opts.think)
	return reg.probeTierFallback(ctx, tier)
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
		// The same first-words label the /resume picker shows; the read
		// stops at the head of each log and takes no lock.
		line := fmt.Sprintf("%s  %s  %d bytes", info.ID, info.Modified.Local().Format("2006-01-02 15:04:05"), info.Size)
		if opening := openingLabel(info.Path); opening != "" {
			line += "  " + opening
		}
		fmt.Println(line)
	}
	return nil
}
