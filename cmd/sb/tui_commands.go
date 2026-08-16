package main

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/atotto/clipboard"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/cj-vana/switchboard/internal/catalog"
	"github.com/cj-vana/switchboard/internal/config"
	"github.com/cj-vana/switchboard/internal/mcp"
	"github.com/cj-vana/switchboard/internal/permission"
	"github.com/cj-vana/switchboard/internal/provider"
)

// commandItem is one slash command. busySafe commands may run while a turn is
// in flight; everything else waits, because touching the session mid-turn
// would race the loop.
type commandItem struct {
	name     string
	aliases  []string
	usage    string
	desc     string
	busySafe bool
	run      func(m *tuiModel, args string) tea.Cmd
}

func commands() []commandItem {
	return []commandItem{
		{name: "help", desc: "show commands and keybindings", busySafe: true, run: cmdHelp},
		{name: "exit", aliases: []string{"quit"}, desc: "leave", busySafe: true, run: cmdExit},
		{name: "clear", aliases: []string{"new", "reset"}, desc: "start a fresh session", run: cmdClear},
		{name: "resume", usage: "[id]", desc: "pick up an earlier session", run: cmdResume},
		{name: "fork", usage: "[n]", desc: "branch this session, less its last n user turns", run: cmdFork},
		{name: "tier", usage: "<id>", desc: "switch tier (bare /t2 works too)", run: cmdTier},
		{name: "tiers", desc: "show the configured ladder", busySafe: true, run: cmdTiers},
		{name: "why", desc: "how this tier was chosen, and what the others would have cost", busySafe: true, run: cmdWhy},
		{name: "advisor", usage: "[on|off|status]", desc: "a second model that watches and advises", busySafe: true, run: cmdAdvisor},
		{name: "mode", usage: "[plan|default|acceptEdits|bypass]", desc: "show or change the permission mode", run: cmdMode},
		{name: "cost", aliases: []string{"usage"}, desc: "tokens and cost for this session", busySafe: true, run: cmdCost},
		{name: "budget", usage: "[amount|off]", desc: "a dollar ceiling the session must stay under", busySafe: true, run: cmdBudget},
		{name: "compact", usage: "[guidance|auto|at]", desc: "summarize into a fresh context; auto-compacts near the window", run: cmdCompact},
		{name: "context", desc: "how much of the window is in use", busySafe: true, run: cmdContext},
		{name: "init", desc: "write an AGENTS.md for this repository", run: cmdInit},
		{name: "export", usage: "[file]", desc: "save the conversation as markdown", busySafe: true, run: cmdExport},
		{name: "session", desc: "session id, target, and message count", busySafe: true, run: cmdSession},
		{name: "sandbox", desc: "what isolation this host provides", busySafe: true, run: cmdSandbox},
		{name: "trust", usage: "[grant|revoke|list]", desc: "let this workspace run what it declares (MCP servers, hooks)", busySafe: true, run: cmdTrust},
		{name: "mcp", desc: "connected MCP servers and their tools", busySafe: true, run: cmdMCP},
		{name: "hooks", desc: "commands that run around each tool call", busySafe: true, run: cmdHooks},
		{name: "agents", desc: "named subagents the model can delegate to", busySafe: true, run: cmdAgents},
		{name: "diff", desc: "review uncommitted changes", busySafe: true, run: cmdDiff},
		{name: "undo", usage: "[list]", desc: "take back the last turn's file changes", run: cmdUndo},
		{name: "copy", usage: "[n]", desc: "copy the last (or nth-latest) response", busySafe: true, run: cmdCopy},
		{name: "setup", desc: "connect providers: keys, local server, an existing codex login", run: cmdSetup},
		{name: "models", desc: "browse models and bind tiers", run: cmdModels},
		{name: "think", aliases: []string{"effort"}, usage: "[level]", desc: "reasoning effort for the active model, this session", run: cmdThink},
		{name: "login", usage: "[provider[/surface]]", desc: "store an API key in the OS keychain", busySafe: true, run: cmdLogin},
		{name: "logout", usage: "[provider[/surface]]", desc: "remove a stored API key", busySafe: true, run: cmdLogout},
		{name: "theme", usage: "[dark|light]", desc: "switch the color theme", run: cmdTheme},
		{name: "update", usage: "[channel|auto …]", desc: "install a newer switchboard, or set the update posture", busySafe: true, run: cmdUpdate},
	}
}

// matchingCommands filters the registry plus the dynamic tier-switching
// entries by prefix, for the autocomplete list.
func matchingCommands(prefix string, cfg *config.Config) []commandItem {
	var out []commandItem
	for _, c := range commands() {
		if strings.HasPrefix(c.name, prefix) {
			out = append(out, c)
			continue
		}
		for _, a := range c.aliases {
			if strings.HasPrefix(a, prefix) {
				out = append(out, c)
				break
			}
		}
	}
	for _, t := range cfg.Tiers {
		if strings.HasPrefix(t.ID, prefix) {
			out = append(out, commandItem{name: t.ID, desc: "switch to tier " + t.ID + "; /" + t.ID + " <prompt> runs one prompt there"})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].name < out[j].name })
	return out
}

func cmdHelp(m *tuiModel, _ string) tea.Cmd {
	var b strings.Builder
	b.WriteString("commands\n")
	for _, c := range commands() {
		name := "  /" + c.name
		if c.usage != "" {
			name += " " + c.usage
		}
		fmt.Fprintf(&b, "%s%s%s\n", name, strings.Repeat(" ", max(46-len(name), 2)), c.desc)
	}
	if tiers := m.app.config.Tiers; len(tiers) > 0 {
		var ids []string
		for _, t := range tiers {
			ids = append(ids, "/"+t.ID)
		}
		fmt.Fprintf(&b, "  %s%sswitch tier; /<tier> <prompt> runs one prompt there\n",
			strings.Join(ids, ", "), strings.Repeat(" ", 2))
	}
	b.WriteString(`
input
  @path            attach a file (tab completes)   !cmd    run a shell command yourself
  \ then enter     continue the line               alt+enter / ctrl+j   newline

keys
  enter            send                  tab                complete
  ↑↓               history / choose      ctrl+r             search prompt history
  shift+tab        cycle permission mode ctrl+t             tier picker
  ctrl+p           command palette       ctrl+g             edit the prompt in $EDITOR
  ctrl+o           expand the last route or tool entry
  esc              interrupt the turn    ctrl+c ctrl+c      exit
  pgup/pgdn        scroll                mouse wheel        scroll`)
	m.addInfo(b.String())
	return nil
}

func cmdExit(m *tuiModel, _ string) tea.Cmd {
	if m.pendingAsk != nil {
		m.pendingAsk <- permission.Response{}
	}
	m.quitting = true
	return tea.Quit
}

func cmdClear(m *tuiModel, _ string) tea.Cmd { return m.app.clearSession() }

func cmdResume(m *tuiModel, args string) tea.Cmd {
	if args != "" {
		return m.app.reopen(args)
	}
	infos, err := m.app.store.List(m.app.workspace)
	if err != nil {
		return noticeCmd("error", err.Error())
	}
	if len(infos) == 0 {
		return noticeCmd("", "no sessions recorded for "+m.app.workspace)
	}
	items := make([]pickerItem, 0, len(infos))
	for _, info := range infos {
		items = append(items, pickerItem{
			id:      info.ID,
			label:   info.ID,
			desc:    info.Modified.Local().Format("2006-01-02 15:04:05"),
			current: m.app.loop.Session != nil && info.ID == m.app.loop.Session.ID(),
		})
	}
	m.dlg = &pickerDialog{
		title:  "resume a session",
		items:  items,
		onPick: func(id string) tea.Cmd { return m.app.reopen(id) },
	}
	return nil
}

// cmdFork branches the session at a turn boundary (§12). Bare /fork branches
// at the tip — a safe point to explore from — and /fork n leaves the last n
// user turns behind, which is the cache-honest form of "go back two turns":
// the original log is never rewritten, and the fork's prefix stays warm on
// the provider because it is byte-identical to what was already sent.
func cmdFork(m *tuiModel, args string) tea.Cmd {
	state := m.app.loop.Session.State()
	if len(state.Messages) == 0 {
		return noticeCmd("", "nothing to fork; the session is empty")
	}

	n := 0
	if args = strings.TrimSpace(args); args != "" {
		v, err := strconv.Atoi(args)
		if err != nil || v < 0 {
			return noticeCmd("error", "/fork takes how many user turns to leave behind, e.g. /fork 2")
		}
		n = v
	}

	keep := len(state.Messages)
	if n > 0 {
		var userAt []int
		for i, msg := range state.Messages {
			if msg.Role == provider.RoleUser {
				userAt = append(userAt, i)
			}
		}
		if n >= len(userAt) {
			return noticeCmd("error", fmt.Sprintf(
				"the session has %d user turns; dropping %d would leave nothing, and /clear is how an empty session starts", len(userAt), n))
		}
		keep = userAt[len(userAt)-n]
	}
	return m.app.forkSession(m.app.loop.Session.ID(), keep, n)
}

func cmdTier(m *tuiModel, args string) tea.Cmd {
	if args == "" {
		return m.openTierPicker()
	}
	return m.app.switchTier(args)
}

func cmdTiers(m *tuiModel, _ string) tea.Cmd {
	if len(m.app.config.Tiers) == 0 {
		return noticeCmd("", "no tiers configured in "+m.app.config.Path)
	}
	var b strings.Builder
	for _, t := range m.app.config.Tiers {
		marker := "  "
		if t.ID == m.app.tier.ID {
			marker = "* "
		}
		b.WriteString(marker + t.String() + "\n")
		info, confidence, ok := m.app.catalog.Lookup(t.Target)
		if !ok {
			b.WriteString("      no catalog entry\n")
			continue
		}
		b.WriteString("      " + describePricing(info))
		if confidence == catalog.Prior {
			b.WriteString("  (surface default, not verified for this model)")
		}
		b.WriteString("\n")
	}
	m.addInfo(strings.TrimRight(b.String(), "\n"))
	return nil
}

func cmdMode(m *tuiModel, args string) tea.Cmd {
	if args != "" {
		mode, err := permission.ParseMode(args)
		if err != nil {
			return noticeCmd("error", err.Error())
		}
		return m.setMode(mode)
	}
	descs := map[permission.Mode]string{
		permission.ModePlan:        "read-only: no writes, no commands",
		permission.ModeDefault:     "writes and commands ask first",
		permission.ModeAcceptEdits: "edits apply, commands ask first",
		permission.ModeBypass:      "no prompts, inside a verified sandbox",
	}
	var items []pickerItem
	for _, mode := range []permission.Mode{permission.ModePlan, permission.ModeDefault, permission.ModeAcceptEdits, permission.ModeBypass} {
		items = append(items, pickerItem{
			id:      string(mode),
			label:   string(mode),
			desc:    descs[mode],
			current: m.mode == mode,
		})
	}
	m.dlg = &pickerDialog{
		title: "permission mode",
		items: items,
		onPick: func(id string) tea.Cmd {
			mode, err := permission.ParseMode(id)
			if err != nil {
				return noticeCmd("error", err.Error())
			}
			return m.setMode(mode)
		},
	}
	return nil
}

func cmdCost(m *tuiModel, _ string) tea.Cmd {
	state := m.app.loop.Session.State()
	m.refreshCost(state)
	m.addInfo(strings.Join(summaryLines(state, m.app.catalog, m.app.loop.Target), "\n"))
	return nil
}

// cmdBudget shows or sets the session's dollar ceiling. Setting persists the
// way /theme does. It stays busy-safe on purpose: the loop's gate reads the
// shared state before every call, so lowering the ceiling mid-turn is how a
// runaway turn gets stopped without waiting for it.
func cmdBudget(m *tuiModel, args string) tea.Cmd {
	args = strings.TrimSpace(args)
	bs := m.app.budget
	if bs == nil {
		return noticeCmd("error", "no budget state is wired for this session")
	}
	switch args {
	case "":
		ceiling := bs.get()
		if ceiling == 0 {
			m.addInfo("  no ceiling set\n" +
				"  /budget 2.50 caps what this session may spend: the router refuses rungs whose\n" +
				"  upper bound could cross it, escalation cannot move onto one, and the loop stops\n" +
				"  before the call that would. /budget off clears it. The setting persists.")
			return nil
		}
		state := m.app.loop.Session.State()
		spent := catalog.Money(state.CostMicroUSD)
		var b strings.Builder
		fmt.Fprintf(&b, "  ceiling  %s\n  spent    %s\n  left     %s", ceiling, spent, ceiling-spent)
		info, _, ok := m.app.catalog.Lookup(m.app.loop.Target)
		switch {
		case !ok:
			b.WriteString("\n  the active target has no catalog entry, so its calls are unpriced and pass the gate")
		case info.Metering == catalog.Local:
			b.WriteString("\n  the active rung runs locally; the ceiling governs the rungs that bill dollars")
		case info.Metering == catalog.Plan:
			b.WriteString("\n  the active rung bills quota, not dollars; the ceiling governs the rungs that bill dollars")
		}
		b.WriteString("\n  a delegate errand counts its own log and this one against the same ceiling while it runs")
		m.addInfo(b.String())
		return nil
	case "off":
		bs.set(0)
		m.app.config.Budget = 0
		m.refreshCost(m.app.loop.Session.State())
		if err := m.app.config.Save(); err != nil {
			return noticeCmd("warn", "ceiling cleared for this session, but not saved: "+err.Error())
		}
		return noticeCmd("", "ceiling cleared")
	default:
		var money catalog.Money
		if err := money.UnmarshalText([]byte(args)); err != nil || money <= 0 {
			return noticeCmd("error", "/budget takes a dollar amount like 2.50, or off")
		}
		bs.set(money)
		m.app.config.Budget = money
		m.refreshCost(m.app.loop.Session.State())
		if err := m.app.config.Save(); err != nil {
			return noticeCmd("warn", fmt.Sprintf("ceiling %s set for this session, but not saved: %v", money, err))
		}
		return noticeCmd("", fmt.Sprintf("ceiling %s set; the session stops before the call that would cross it", money))
	}
}

func cmdSession(m *tuiModel, _ string) tea.Cmd {
	state := m.app.loop.Session.State()
	m.addInfo(fmt.Sprintf("  %s\n  target   %s\n  catalog  %s\n  messages %d\n  log      %s",
		state.ID, state.Target, state.CatalogRevision, len(state.Messages), m.app.loop.Session.Path()))
	return nil
}

func cmdSandbox(m *tuiModel, _ string) tea.Cmd {
	cap := m.app.capability
	m.addInfo(fmt.Sprintf("  platform  %s\n  mechanism %s\n  %s", cap.Platform, cap.Mechanism, cap.Summary()))
	return nil
}

func cmdDiff(m *tuiModel, _ string) tea.Cmd {
	return openDiff(m.app.workspace, m.th.dark)
}

// cmdMCP reports the session's external tooling: which servers connected,
// what each brought, and which died since. It reads live state rather than
// startup state, because a server that crashed an hour ago is the thing the
// user is here to find out.
func cmdMCP(m *tuiModel, _ string) tea.Cmd {
	st := m.app.mcp
	var clients []*mcp.Client
	if st != nil {
		clients = st.clientList()
	}
	if len(clients) == 0 {
		m.addInfo("  no MCP servers connected\n" +
			"  declare them in ~/.switchboard/mcp.toml, or in this repository's .switchboard/mcp.toml behind /trust grant:\n\n" +
			"    [mcp.github]\n" +
			"    command = \"github-mcp-server\"\n" +
			"    args = [\"stdio\"]\n\n" +
			"  a url key instead of command reaches a Streamable HTTP server")
		return nil
	}
	var b strings.Builder
	for _, c := range clients {
		fmt.Fprintf(&b, "  %s  %s", c.Name(), c.ServerLine())
		if err := c.Err(); err != nil {
			fmt.Fprintf(&b, "\n    dead: %v", err)
		}
		for _, t := range c.Tools() {
			fmt.Fprintf(&b, "\n    %s", mcp.Namespaced(c.Name(), t.Name))
		}
		b.WriteString("\n")
	}
	m.addInfo(strings.TrimRight(b.String(), "\n"))
	return nil
}

// cmdUndo takes back the most recent turn's write and edit effects. It is
// not busy-safe on purpose: undoing under a turn still capturing into its
// own scope would restore a state the model is mid-way through changing.
func cmdUndo(m *tuiModel, args string) tea.Cmd {
	rec := m.app.undo
	if rec == nil {
		return noticeCmd("error", "undo is unavailable in this session")
	}
	if strings.TrimSpace(args) == "list" {
		turns := rec.Turns()
		if len(turns) == 0 {
			m.addInfo("  no turns have changed files")
			return nil
		}
		var b strings.Builder
		for i, t := range turns {
			partial := ""
			if t.Partial {
				partial = "  (partial: some files were over the snapshot cap)"
			}
			fmt.Fprintf(&b, "  %2d  %d file(s)  %s%s\n", len(turns)-i, t.Files, t.Label, partial)
		}
		b.WriteString("  /undo takes back the most recent; repeat to walk further")
		m.addInfo(strings.TrimRight(b.String(), "\n"))
		return nil
	}

	restored, removed, skipped, failed, label, err := rec.Undo()
	if err != nil {
		return noticeCmd("", err.Error())
	}
	// Restored files changed under the model's feet; the stale check must
	// force a re-read before the next write.
	m.app.loop.Tools.ForgetVersions(append(append([]string(nil), restored...), removed...))
	m.app.loop.Session.AppendNote("info", fmt.Sprintf("undo: reverted %q (%d restored, %d removed)", label, len(restored), len(removed)))

	var b strings.Builder
	fmt.Fprintf(&b, "  took back %q\n", label)
	for _, p := range restored {
		fmt.Fprintf(&b, "  restored %s\n", m.app.displayPath(p))
	}
	for _, p := range removed {
		fmt.Fprintf(&b, "  removed  %s\n", m.app.displayPath(p))
	}
	for _, p := range skipped {
		fmt.Fprintf(&b, "  not covered (over the snapshot cap): %s\n", m.app.displayPath(p))
	}
	for _, f := range failed {
		fmt.Fprintf(&b, "  failed: %s\n", f)
	}
	m.addInfo(strings.TrimRight(b.String(), "\n"))
	if len(failed) > 0 {
		return noticeCmd("warn", "some files could not be restored; see above")
	}
	return nil
}

// cmdHooks lists the loaded hooks: which event, which tools, what runs.
func cmdHooks(m *tuiModel, _ string) tea.Cmd {
	set := m.app.loop.Hooks
	if set.Empty() {
		m.addInfo("  no hooks loaded\n" +
			"  declare them in ~/.switchboard/hooks.toml, or in this repository's .switchboard/hooks.toml behind /trust grant:\n\n" +
			"    [[hooks.pre_tool]]\n" +
			"    tools = [\"exec\"]\n" +
			"    run = \"./scripts/audit.sh\"\n\n" +
			"  a pre_tool hook that exits non-zero blocks the call; a post_tool hook's output rides back on the result")
		return nil
	}
	var b strings.Builder
	for _, h := range set.Hooks() {
		scope := "every tool"
		if len(h.Tools) > 0 {
			scope = strings.Join(h.Tools, ", ")
		}
		fmt.Fprintf(&b, "  %-9s %-20s %s\n", h.Event, scope, h.Run)
	}
	m.addInfo(strings.TrimRight(b.String(), "\n"))
	return nil
}

// cmdAgents lists the named subagent definitions this session discovered,
// with each one's rung, grant, and which directory spoke.
func cmdAgents(m *tuiModel, _ string) tea.Cmd {
	if len(m.app.agents) == 0 && len(m.app.agentNotes) == 0 {
		m.addInfo("  no agents defined\n" +
			"  a markdown file per agent, in this repository's .switchboard/agents/ or in ~/.switchboard/agents/:\n\n" +
			"    ---\n" +
			"    description: reviews a diff for correctness\n" +
			"    tier: t2\n" +
			"    tools: read, grep, glob\n" +
			"    ---\n" +
			"    You review changes. Report problems; do not fix them.\n\n" +
			"  the model runs one by calling delegate with its name; a new file is picked up next session")
		return nil
	}
	var b strings.Builder
	for _, ag := range m.app.agents {
		rung := ag.Tier
		if rung == "" && len(m.app.config.Tiers) > 0 {
			rung = m.app.config.Tiers[0].ID
		}
		src := "~/.switchboard/agents"
		if !ag.FromHome {
			src = ".switchboard/agents"
		}
		fmt.Fprintf(&b, "  %-14s %-4s %s\n", ag.Name, rung, ag.Description)
		grant := "the full core suite"
		if len(ag.Tools) > 0 {
			grant = strings.Join(ag.Tools, ", ")
		}
		fmt.Fprintf(&b, "  %-14s %-4s tools: %s · from %s\n", "", "", grant, src)
	}
	for _, n := range m.app.agentNotes {
		fmt.Fprintf(&b, "  ! %s\n", n)
	}
	m.addInfo(strings.TrimRight(b.String(), "\n"))
	return nil
}

// cmdTrust shows and edits the standing grant that lets a checkout start the
// processes it declares. The wording stays concrete about what a grant
// enables, because "trust this workspace?" answered without knowing the
// stakes is the permission-prompt-as-sandbox mistake in another costume.
func cmdTrust(m *tuiModel, args string) tea.Cmd {
	s := m.app.trust
	if s == nil {
		return noticeCmd("error", "the trust store is unavailable: "+m.app.trustErr)
	}
	ws := m.app.workspace
	switch strings.TrimSpace(args) {
	case "":
		state := "not trusted: MCP servers and hooks declared in this repository's .switchboard/ stay off"
		if s.Trusted(ws) {
			state = "trusted: MCP servers and hooks declared in this repository's .switchboard/ may run"
		}
		m.addInfo(fmt.Sprintf("  %s\n  %s\n  /trust grant enables, /trust revoke withdraws; ~/.switchboard config always runs", ws, state))
	case "grant":
		if err := s.Grant(ws); err != nil {
			return noticeCmd("error", "grant failed: "+err.Error())
		}
		m.addInfo("  workspace trusted; repository-declared MCP servers and hooks start on the next run of sb")
	case "revoke":
		if err := s.Revoke(ws); err != nil {
			return noticeCmd("error", "revoke failed: "+err.Error())
		}
		m.addInfo("  trust withdrawn; repository-declared MCP servers and hooks stay off from the next run of sb")
	case "list":
		granted := s.Granted()
		if len(granted) == 0 {
			m.addInfo("  no workspaces are trusted")
			break
		}
		m.addInfo("  " + strings.Join(granted, "\n  "))
	default:
		return noticeCmd("error", "/trust takes grant, revoke, or list")
	}
	return nil
}

func cmdCopy(m *tuiModel, args string) tea.Cmd {
	n := 1
	if args != "" {
		v, err := strconv.Atoi(args)
		if err != nil || v < 1 {
			return noticeCmd("error", "/copy takes a positive number: /copy 2 copies the second-to-last response")
		}
		n = v
	}
	var texts []string
	for i := len(m.tr.entries) - 1; i >= 0; i-- {
		if m.tr.entries[i].kind == kindAssistant && m.tr.entries[i].text != "" {
			texts = append(texts, m.tr.entries[i].text)
		}
	}
	if n > len(texts) {
		return noticeCmd("error", fmt.Sprintf("only %d responses to copy", len(texts)))
	}
	text := texts[n-1]
	return func() tea.Msg {
		return copyMsg{n: n, err: clipboard.WriteAll(text)}
	}
}

func cmdTheme(m *tuiModel, args string) tea.Cmd {
	apply := func(name string) tea.Cmd {
		switch name {
		case "dark":
			m.setTheme(true)
		case "light":
			m.setTheme(false)
		default:
			return noticeCmd("error", "theme is dark or light")
		}
		m.app.config.Theme = name
		if err := m.app.config.Save(); err != nil {
			return noticeCmd("error", "theme is now "+name+", but saving it failed: "+err.Error())
		}
		return noticeCmd("", "theme is now "+name)
	}
	if args != "" {
		return apply(args)
	}
	m.dlg = &pickerDialog{
		title: "theme",
		items: []pickerItem{
			{id: "dark", label: "dark", current: m.th.dark},
			{id: "light", label: "light", current: !m.th.dark},
		},
		onPick: apply,
	}
	return nil
}

func (m *tuiModel) setTheme(dark bool) {
	m.th = themeFor(dark)
	m.md.setDark(dark)
	m.tr.setTheme(m.th)
}
