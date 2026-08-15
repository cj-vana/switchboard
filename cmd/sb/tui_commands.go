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
	"github.com/cj-vana/switchboard/internal/permission"
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
		{name: "tier", usage: "<id>", desc: "switch tier (bare /t2 works too)", run: cmdTier},
		{name: "tiers", desc: "show the configured ladder", busySafe: true, run: cmdTiers},
		{name: "mode", usage: "[plan|default|acceptEdits|bypass]", desc: "show or change the permission mode", run: cmdMode},
		{name: "cost", aliases: []string{"usage"}, desc: "tokens and cost for this session", busySafe: true, run: cmdCost},
		{name: "session", desc: "session id, target, and message count", busySafe: true, run: cmdSession},
		{name: "sandbox", desc: "what isolation this host provides", busySafe: true, run: cmdSandbox},
		{name: "diff", desc: "review uncommitted changes", busySafe: true, run: cmdDiff},
		{name: "copy", usage: "[n]", desc: "copy the last (or nth-latest) response", busySafe: true, run: cmdCopy},
		{name: "theme", usage: "[dark|light]", desc: "switch the color theme", run: cmdTheme},
		{name: "update", desc: "check for and install a newer switchboard", busySafe: true, run: cmdUpdate},
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
keys
  enter            send                  alt+enter / ctrl+j   newline
  tab              complete a command    ↑↓                   history / suggestions
  shift+tab        cycle permission mode ctrl+t              tier picker
  ctrl+o           expand the last route or tool entry
  esc              interrupt the turn    ctrl+c ctrl+c       exit
  pgup/pgdn        scroll                mouse wheel         scroll`)
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
