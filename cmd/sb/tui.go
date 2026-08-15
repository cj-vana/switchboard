package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/spinner"
	"github.com/charmbracelet/bubbles/textarea"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/cj-vana/switchboard/internal/agent"
	"github.com/cj-vana/switchboard/internal/catalog"
	"github.com/cj-vana/switchboard/internal/config"
	"github.com/cj-vana/switchboard/internal/execution"
	"github.com/cj-vana/switchboard/internal/permission"
	"github.com/cj-vana/switchboard/internal/provider"
	route "github.com/cj-vana/switchboard/internal/router"
	"github.com/cj-vana/switchboard/internal/session"
	"github.com/cj-vana/switchboard/internal/tools"
)

// Messages flowing from the loop goroutine (and async commands) into the
// program. The loop never touches the model; everything arrives through these.
type deltaMsg struct {
	thinking bool
	text     string
}
type toolStartMsg struct {
	name string
	req  permission.Request
}
type toolEndMsg struct {
	name string
	res  tools.Result
	took time.Duration
}
type noticeMsg struct {
	level, text string
}
type usageMsg struct{ u session.Usage }
type askMsg struct {
	req     permission.Request
	out     permission.Outcome
	respond chan permission.Response
}
type turnDoneMsg struct {
	err   error
	after session.State
}
type tierNowMsg struct{ line string }
type tierSwitchMsg struct {
	tier   config.Tier
	client provider.Provider
	silent bool // a /tN override restoring what it borrowed, not a user switch
	err    error
}
type sessionSwapMsg struct {
	sess   *session.Session
	tier   config.Tier
	client provider.Provider
	fresh  bool
	err    error
}
type overrideProbeMsg struct {
	prompt string
	tier   config.Tier
	client provider.Provider
	err    error
}
type updateCheckMsg struct {
	latest string
	err    error
}
type updateAppliedMsg struct{ version string }
type copyMsg struct {
	n   int
	err error
}
type disarmQuitMsg struct{}

func noticeCmd(level, text string) tea.Cmd {
	return func() tea.Msg { return noticeMsg{level: level, text: text} }
}

type tuiModel struct {
	app  *tuiApp
	tr   *transcript
	ta   textarea.Model
	spin spinner.Model
	th   *theme
	md   *markdown

	width, height int

	busy        bool
	started     time.Time
	turnCancel  context.CancelFunc
	turnPrompt  string
	turnStarted config.Tier
	turnBefore  session.State
	queue       []string

	// Status-line state. The model renders from its own copies rather than
	// reading loop fields, because the loop's goroutine can be mutating them.
	tierLine        string
	mode            permission.Mode
	costLine        string
	turnIn, turnOut int
	callTokens      int
	ctxWindow       int
	updateAvail     string

	history   []string
	histIdx   int
	sugSel    int
	sugClosed bool

	// pendingShell holds ! command transcripts awaiting the next turn, and
	// the mention fields back @path completion (tui_mentions.go).
	pendingShell  []string
	mentionSel    int
	mentionList   []string
	mentionListAt time.Time

	// routeLog records every tier move this session, for /why. The transcript
	// scrolls; the question "how did I end up on t3" should not.
	routeLog []string

	// Reverse history search (tui_history.go).
	histSearch bool
	histQuery  string
	histMatch  int

	// custom holds the markdown-file commands loaded at startup
	// (tui_custom.go).
	custom []customCommand

	dlg  dialog
	full *diffView

	pendingAsk  chan permission.Response
	restoreTier *config.Tier
	quitArmed   bool
	quitting    bool

	turnCtx    context.Context
	initialCmd tea.Cmd
}

// runTUI is the Bubble Tea front end: same wiring as the REPL, with the
// observer and asker pointed at the program instead of stdin/stdout.
func runTUI(
	loop *agent.Loop,
	store *session.Store,
	cfg *config.Config,
	cat *catalog.Catalog,
	capability execution.Capability,
	workspace string,
	tier config.Tier,
	reg *providers,
	sticky *route.Sticky,
	routeDec *route.Decision,
	sess *session.Session,
	resumed bool,
	updateCheck bool,
) error {
	// Background detection uses COLORFGBG rather than an OSC query: querying
	// the terminal races Bubble Tea for stdin and, on a terminal that does not
	// answer, stalls the first paint (§14's 50ms). Absent the variable, dark
	// is the default. A theme chosen with /theme is saved to the config and
	// beats detection: the user said, the terminal only hinted.
	dark := detectDark()
	switch cfg.Theme {
	case "dark":
		dark = true
	case "light":
		dark = false
	}
	th := themeFor(dark)
	md := newMarkdown(100, dark)
	ta := newTextarea()

	obs := &tuiObserver{}
	app := &tuiApp{
		loop:       loop,
		store:      store,
		config:     cfg,
		catalog:    cat,
		tier:       tier,
		providers:  reg,
		capability: capability,
		workspace:  workspace,
		route:      routeDec,
		sticky:     sticky,
		obs:        obs,
	}

	m := newTUIModel(app, th, md, ta)

	p := tea.NewProgram(m, tea.WithAltScreen(), tea.WithMouseCellMotion())
	obs.p = p
	app.p = p
	app.watcher = newWatcher(obs, sticky, len(cfg.Tiers)-1, app.moveTo)
	loop.Observer = app.watcher
	loop.Asker = &tuiAsker{p: p}

	for _, l := range app.bannerLines(sess, resumed) {
		m.addInfo(l)
	}
	if routeDec != nil {
		m.addRoute(routeSummary(*routeDec), describeRoute(*routeDec))
	}
	if resumed {
		m.replayHistory(sess.State())
	}

	var initial []tea.Cmd
	if updateCheck {
		initial = append(initial, startupUpdate(cfg))
	}
	// An advisor slot in the config is the standing request to watch every
	// session; /advisor off remains the per-session override.
	if _, bound := cfg.Slots["advisor"]; bound {
		initial = append(initial, startAdvisor(app))
	}
	// The tab's title answers "which terminal was that" for a user with six
	// of them: this workspace, this tier.
	initial = append(initial, tea.SetWindowTitle("sb · "+filepath.Base(workspace)+" · "+tier.ID))
	m.initialCmd = tea.Batch(initial...)

	_, err := p.Run()
	return err
}

// detectDark reads COLORFGBG, whose last field is the background color index.
func detectDark() bool {
	fgbg := os.Getenv("COLORFGBG")
	if fgbg == "" {
		return true
	}
	last := fgbg
	if i := strings.LastIndex(fgbg, ";"); i >= 0 {
		last = fgbg[i+1:]
	}
	n, err := strconv.Atoi(last)
	if err != nil {
		return true
	}
	// 0-6 and 8 are dark backgrounds; 7 and 9-15 are light.
	return n < 7 || n == 8
}

func themeFor(dark bool) *theme {
	if dark {
		return darkTheme()
	}
	return lightTheme()
}

// newTextarea builds the prompt box: enter submits, newline is a modifier
// chord, and the box grows with its content.
func newTextarea() textarea.Model {
	ta := textarea.New()
	ta.Prompt = "› "
	ta.ShowLineNumbers = false
	ta.CharLimit = 0
	ta.SetHeight(1)
	ta.SetWidth(98)
	ta.KeyMap.InsertNewline.SetKeys("ctrl+j", "alt+enter")
	ta.Focus()
	return ta
}

// newTUIModel assembles the model around an app. It is separate from runTUI so
// tests can drive the model without a terminal.
func newTUIModel(app *tuiApp, th *theme, md *markdown, ta textarea.Model) *tuiModel {
	m := &tuiModel{
		app:      app,
		th:       th,
		md:       md,
		ta:       ta,
		spin:     spinner.New(spinner.WithSpinner(spinner.Dot)),
		tierLine: app.tierLine(),
		mode:     app.loop.Perms.Mode(),
		history:  loadHistory(app.workspace),
		custom:   loadCustomCommands(app.workspace),
	}
	m.histIdx = len(m.history)
	m.tr = newTranscript(100, th, md)
	m.refreshCost(app.loop.Session.State())
	m.refreshCtxWindow()
	return m
}

// initialCmd is set by runTUI before the program starts; Init hands it to tea.
func (m *tuiModel) Init() tea.Cmd { return m.initialCmd }

func (m *tuiModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.tr.setWidth(msg.Width)
		m.ta.SetWidth(msg.Width - 1)
		return m, nil

	case tea.MouseMsg:
		if m.full != nil {
			m.full.mouse(msg)
			return m, nil
		}
		switch msg.Button {
		case tea.MouseButtonWheelUp:
			m.tr.scrollBy(3)
		case tea.MouseButtonWheelDown:
			m.tr.scrollBy(-3)
		}
		return m, nil

	case tea.KeyMsg:
		return m, m.key(msg)

	case spinner.TickMsg:
		if m.busy {
			var cmd tea.Cmd
			m.spin, cmd = m.spin.Update(msg)
			return m, cmd
		}
		return m, nil

	case deltaMsg:
		m.onDelta(msg)
		return m, nil

	case toolStartMsg:
		m.onToolStart(msg)
		return m, nil

	case toolEndMsg:
		m.onToolEnd(msg)
		return m, nil

	case noticeMsg:
		m.addNotice(msg.level, msg.text)
		return m, nil

	case usageMsg:
		m.turnIn += msg.u.Usage.InputTokens + msg.u.Usage.CacheWriteTokens
		m.turnOut += msg.u.Usage.OutputTokens
		m.callTokens = msg.u.Usage.InputTokens + msg.u.Usage.CacheReadTokens + msg.u.Usage.CacheWriteTokens
		return m, nil

	case askMsg:
		m.pendingAsk = msg.respond
		m.dlg = newPermissionDialog(msg.req, msg.out, msg.respond)
		return m, nil

	case pickerMsg:
		m.dlg = &pickerDialog{title: msg.title, items: msg.items, onPick: msg.action}
		return m, nil

	case secretPromptMsg:
		m.dlg = newSecretDialog(msg.ref, msg.storeName, func(value string) tea.Cmd {
			return storeSecretCmd(msg.ref, msg.writer, msg.storeName, value)
		})
		return m, nil

	case turnDoneMsg:
		return m, m.onTurnDone(msg)

	case tierNowMsg:
		m.addInfo(msg.line)
		m.routeLog = append(m.routeLog, msg.line)
		m.tierLine = m.app.tierLine()
		m.refreshCtxWindow()
		return m, nil

	case tierSwitchMsg:
		return m, m.onTierSwitch(msg)

	case overrideProbeMsg:
		return m, m.onOverrideProbe(msg)

	case sessionSwapMsg:
		return m, m.onSessionSwap(msg)

	case updateCheckMsg:
		if msg.err == nil && msg.latest != "" {
			m.updateAvail = msg.latest
			m.addNotice("", "switchboard "+msg.latest+" is available; run /update to install it")
		}
		return m, nil

	case updateAppliedMsg:
		m.updateAvail = msg.version + " ready"
		m.addNotice("", "updated to "+msg.version+" in the background; the next start runs it")
		return m, nil

	case copyMsg:
		if msg.err != nil {
			m.addNotice("error", "copy failed: "+msg.err.Error())
		} else {
			m.addNotice("", "copied response "+itoa(msg.n)+" to the clipboard")
		}
		return m, nil

	case diffLoadedMsg:
		if msg.err != nil {
			m.addNotice("error", "diff failed: "+msg.err.Error())
			return m, nil
		}
		m.full = &diffView{lines: msg.lines}
		return m, nil

	case shellDoneMsg:
		m.onShellDone(msg)
		return m, nil

	case editorDoneMsg:
		m.onEditorDone(msg)
		return m, nil

	case adviceMsg:
		m.addNotice("advisor", msg.text)
		return m, nil

	case expandedCustomMsg:
		return m, m.enqueue(msg.prompt, "")

	case advisorReadyMsg:
		m.onAdvisorReady(msg)
		return m, nil

	case disarmQuitMsg:
		m.quitArmed = false
		return m, nil
	}
	return m, nil
}

// key routes one keypress. Dialogs and the fullscreen diff get first claim;
// what remains goes to the input area.
func (m *tuiModel) key(msg tea.KeyMsg) tea.Cmd {
	if m.full != nil {
		if m.full.key(msg) {
			m.full = nil
		}
		return nil
	}
	if m.dlg != nil {
		done, cmd := m.dlg.update(msg, m.th)
		if done {
			m.dlg = nil
			m.pendingAsk = nil
		}
		return cmd
	}
	if m.histSearch {
		m.historySearchKey(msg)
		return nil
	}

	switch msg.String() {
	case "ctrl+c":
		return m.interrupt()
	case "ctrl+r":
		m.startHistorySearch()
		return nil
	case "esc":
		if m.busy {
			return m.interrupt()
		}
		if m.suggestionsVisible() || m.mentionsVisible() {
			// One dismissal covers both popups, and the next enter submits
			// what was typed instead of accepting a completion.
			m.sugClosed = true
			return nil
		}
		return nil
	case "shift+tab":
		return m.cycleMode()
	case "ctrl+t":
		return m.openTierPicker()
	case "ctrl+p":
		return m.openPalette()
	case "ctrl+g":
		return m.openEditor()
	case "ctrl+o":
		if i := m.tr.lastExpandable(); i >= 0 {
			e := m.tr.entries[i]
			e.expanded = !e.expanded
			m.tr.invalidate(i)
		}
		return nil
	case "pgup":
		m.tr.scrollBy(m.pageSize())
		return nil
	case "pgdown":
		m.tr.scrollBy(-m.pageSize())
		return nil
	case "ctrl+u":
		m.tr.scrollBy(m.pageSize() / 2)
		return nil
	case "ctrl+d":
		m.tr.scrollBy(-m.pageSize() / 2)
		return nil
	}

	if m.suggestionsVisible() {
		switch msg.String() {
		case "up":
			m.sugSel--
			if m.sugSel < 0 {
				m.sugSel = len(m.suggestions()) - 1
			}
			return nil
		case "down":
			m.sugSel = (m.sugSel + 1) % len(m.suggestions())
			return nil
		case "tab":
			m.acceptSuggestion()
			return nil
		case "enter":
			if !m.exactCommand() {
				m.acceptSuggestion()
				return nil
			}
			return m.submit()
		}
	}

	if m.mentionsVisible() {
		switch msg.String() {
		case "up":
			m.mentionSel--
			if m.mentionSel < 0 {
				m.mentionSel = len(m.mentionMatches()) - 1
			}
			return nil
		case "down":
			m.mentionSel = (m.mentionSel + 1) % len(m.mentionMatches())
			return nil
		case "tab", "enter":
			m.acceptMention()
			return nil
		}
	}

	switch msg.String() {
	case "enter":
		// A trailing backslash is a line continuation, the one multiline
		// route that works in every terminal ever made.
		if v := m.ta.Value(); strings.HasSuffix(v, "\\") {
			m.ta.SetValue(strings.TrimSuffix(v, "\\") + "\n")
			m.ta.CursorEnd()
			m.growInput()
			return nil
		}
		return m.submit()
	case "up":
		if !strings.Contains(m.ta.Value(), "\n") {
			m.historyMove(-1)
			return nil
		}
	case "down":
		if !strings.Contains(m.ta.Value(), "\n") {
			m.historyMove(1)
			return nil
		}
	}

	var cmd tea.Cmd
	m.ta, cmd = m.ta.Update(msg)
	m.sugClosed = false
	m.sugSel = 0
	m.growInput()
	return cmd
}

func (m *tuiModel) pageSize() int {
	h := m.height - 8
	if h < 4 {
		return 4
	}
	return h
}

// interrupt cancels a running turn; at the prompt it clears the input, and a
// second ctrl-c leaves.
func (m *tuiModel) interrupt() tea.Cmd {
	if m.busy && m.turnCancel != nil {
		m.turnCancel()
		m.addNotice("", "cancelling the turn; the session stays resumable")
		return nil
	}
	if m.ta.Value() != "" {
		m.ta.Reset()
		m.growInput()
		m.quitArmed = false
		return nil
	}
	if m.quitArmed {
		if m.pendingAsk != nil {
			m.pendingAsk <- permission.Response{}
		}
		m.quitting = true
		return tea.Quit
	}
	m.quitArmed = true
	m.addNotice("", "ctrl-c again to exit")
	return tea.Tick(2*time.Second, func(time.Time) tea.Msg { return disarmQuitMsg{} })
}

// submit dispatches the input: a slash command, a tier-prefixed prompt
// (/t3 <prompt> runs this one prompt on t3), or a plain turn.
func (m *tuiModel) submit() tea.Cmd {
	v := strings.TrimSpace(m.ta.Value())
	if v == "" {
		return nil
	}
	m.ta.Reset()
	m.growInput()
	m.sugClosed = false
	m.sugSel = 0
	// Consecutive duplicates collapse: resubmitting "go on" five times is one
	// history entry, not five up-arrow presses of the same thing.
	if len(m.history) == 0 || m.history[len(m.history)-1] != v {
		m.history = append(m.history, v)
		appendHistory(m.app.workspace, v)
	}
	m.histIdx = len(m.history)
	m.quitArmed = false

	if strings.HasPrefix(v, "!") {
		return m.runShell(strings.TrimSpace(strings.TrimPrefix(v, "!")))
	}
	if strings.HasPrefix(v, "/") {
		return m.runSlash(v)
	}
	return m.enqueue(v, "")
}

// enqueue starts the turn now or lines it up behind the running one.
func (m *tuiModel) enqueue(prompt, override string) tea.Cmd {
	if m.busy {
		m.queue = append(m.queue, prompt)
		m.addNotice("", "queued; it runs when the current turn finishes")
		return nil
	}
	return m.startTurn(prompt, override)
}

func (m *tuiModel) startTurn(prompt, override string) tea.Cmd {
	if override != "" {
		tier, ok := m.app.config.Tier(override)
		if !ok {
			return noticeCmd("error", "no tier "+override+" is configured; try /tiers")
		}
		return func() tea.Msg {
			probed, client, err := m.app.providers.probeTier(context.Background(), tier)
			return overrideProbeMsg{prompt: prompt, tier: probed, client: client, err: err}
		}
	}

	// The transcript shows what was typed; the model gets that plus what the
	// @mentions attach and what recent ! commands produced. Expansion happens
	// here, not at submit, so a queued prompt reads its files when it runs.
	m.addUser(prompt)
	prompt = m.adviceContext(m.shellContext(m.expandMentions(prompt)))
	m.beginTurn(prompt)
	go m.runTurn(m.turnCtx, prompt)
	return m.spin.Tick
}

// onOverrideProbe rebinds to the named tier for one turn, remembering what to
// restore when it ends.
func (m *tuiModel) onOverrideProbe(msg overrideProbeMsg) tea.Cmd {
	if msg.err != nil {
		m.addNotice("error", msg.err.Error())
		return nil
	}
	prev := m.app.tier
	m.restoreTier = &prev
	m.app.tier = msg.tier
	m.app.loop.Target = msg.tier.Target
	m.app.loop.Provider = msg.client
	m.app.loop.Cache = cacheFor(msg.tier.Target, m.app.catalog)
	m.tierLine = m.app.tierLine()
	m.refreshCtxWindow()

	m.addUser(msg.prompt)
	m.beginTurn(msg.prompt)
	go m.runTurn(m.turnCtx, msg.prompt)
	return m.spin.Tick
}

func (m *tuiModel) beginTurn(prompt string) {
	m.busy = true
	m.started = time.Now()
	m.turnIn, m.turnOut = 0, 0
	m.turnPrompt = prompt
	m.turnStarted = m.app.tier
	m.turnBefore = m.app.loop.Session.State()
	m.tr.scrollToBottom()
	ctx, cancel := context.WithCancel(context.Background())
	m.turnCancel = cancel
	m.turnCtx = ctx
}

// runTurn drives one turn on its own goroutine. Everything it reports arrives
// as messages; the session stays the only thing it writes.
func (m *tuiModel) runTurn(ctx context.Context, prompt string) {
	m.app.watcher.StartTurn()
	if m.app.advisor != nil {
		m.app.advisor.StartTurn(prompt)
	}
	err := m.app.loop.Turn(ctx, prompt)

	after := m.app.loop.Session.State()
	if rerr := appendRouteRecord(m.app.loop.Session, prompt, m.turnStarted, m.app.tier, m.turnBefore, after, m.started, err, m.app.route, m.app.sticky); rerr != nil {
		m.app.p.Send(noticeMsg{level: "warn", text: "the routing record for this turn was not saved: " + rerr.Error()})
	}
	m.app.p.Send(turnDoneMsg{err: err, after: after})
}

func (m *tuiModel) onTurnDone(msg turnDoneMsg) tea.Cmd {
	m.busy = false
	m.turnCancel = nil
	m.turnCtx = nil
	m.tr.finalizeAll()
	m.refreshCost(msg.after)
	m.app.route = nil // the opening decision describes the opening choice only

	switch {
	case errors.Is(msg.err, context.Canceled):
		m.addNotice("", "turn cancelled; the session is intact and can continue")
	case errors.Is(msg.err, agent.ErrRoundLimit):
		// The loop already said why.
	case msg.err != nil:
		m.addNotice("error", msg.err.Error())
	}

	// A /tN override borrows a tier for one turn. Restoring over a mid-turn
	// escalation would undo the policy's move, so only restore when the target
	// is still the borrowed one.
	if m.restoreTier != nil {
		restore := *m.restoreTier
		m.restoreTier = nil
		if m.app.tier.ID == m.turnStarted.ID {
			return m.restoreCmd(restore)
		}
	}

	// Auto-compaction runs ahead of the queue: a queued prompt sent into a
	// nearly-full window would inherit the failure this exists to prevent,
	// and the queue survives the swap (onSessionSwap drains it).
	if m.shouldAutoCompact() {
		pct := m.callTokens * 100 / m.ctxWindow
		m.addNotice("", fmt.Sprintf("context at %d%% of %s tokens; compacting automatically (/compact auto off disables this)",
			pct, compact(m.ctxWindow)))
		return cmdCompact(m, "")
	}

	if len(m.queue) > 0 {
		next := m.queue[0]
		m.queue = m.queue[1:]
		return m.startTurn(next, "")
	}
	return nil
}

// shouldAutoCompact decides at turn end. callTokens is the size of the last
// request the provider actually saw — input plus cache reads and writes —
// which is the honest measure of occupancy, and the reason this waits for a
// turn boundary rather than trusting a mid-turn estimate the estimator is
// known to undercount (docs/estimator.md).
func (m *tuiModel) shouldAutoCompact() bool {
	cfg := m.app.config
	if !cfg.CompactAuto || m.ctxWindow <= 0 || m.callTokens <= 0 {
		return false
	}
	at := cfg.CompactAtPercent
	if at == 0 {
		at = 85
	}
	return m.callTokens >= m.ctxWindow*at/100
}

// restoreCmd returns to the tier a /tN override borrowed, once the turn that
// borrowed it is done.
func (m *tuiModel) restoreCmd(tier config.Tier) tea.Cmd {
	return func() tea.Msg {
		probed, client, err := m.app.providers.probeTier(context.Background(), tier)
		return tierSwitchMsg{tier: probed, client: client, err: err, silent: true}
	}
}

func (m *tuiModel) onTierSwitch(msg tierSwitchMsg) tea.Cmd {
	if msg.err != nil {
		m.addNotice("error", msg.err.Error())
		return nil
	}
	m.app.bind(msg.tier, msg.client, true)
	m.tierLine = m.app.tierLine()
	m.refreshCtxWindow()
	if !msg.silent {
		m.addInfo("now on " + m.tierLine)
		m.routeLog = append(m.routeLog, "you switched to "+msg.tier.ID)
		m.cacheSwitchNote(msg.tier)
		return nil
	}
	// A silent switch is a /tN override restoring what it borrowed; a queued
	// prompt waits for the restore so it runs on the tier the user is on.
	if len(m.queue) > 0 {
		next := m.queue[0]
		m.queue = m.queue[1:]
		return m.startTurn(next, "")
	}
	return nil
}

// cacheSwitchNote says plainly what a switch abandons: cache state is scoped
// to a target, so whatever was warm on the old one stays with it.
func (m *tuiModel) cacheSwitchNote(tier config.Tier) {
	if info, _, ok := m.app.catalog.Lookup(tier.Target); ok && !info.Free() {
		m.addInfo("a target switch leaves the previous target's cache behind; what that costs is not modelled yet")
	}
}

func (m *tuiModel) onSessionSwap(msg sessionSwapMsg) tea.Cmd {
	if msg.err != nil {
		m.addNotice("error", msg.err.Error())
		return nil
	}
	old := m.app.loop.Session
	m.app.loop.Session = msg.sess
	m.app.bind(msg.tier, msg.client, false)
	if old != nil && old != msg.sess {
		old.Close()
	}
	m.tr.reset()
	for _, l := range m.app.bannerLines(msg.sess, !msg.fresh) {
		m.addInfo(l)
	}
	if !msg.fresh {
		m.replayHistory(msg.sess.State())
	}
	m.tierLine = m.app.tierLine()
	m.mode = m.app.loop.Perms.Mode()
	m.refreshCost(msg.sess.State())
	m.refreshCtxWindow()
	// The old session's occupancy does not describe the new one, and leaving
	// it would re-trigger the auto-compaction that produced this swap.
	m.callTokens = 0

	// Prompts queued behind the turn that triggered an auto-compaction run
	// now, in the fresh context they were waiting for.
	if len(m.queue) > 0 && !m.busy {
		next := m.queue[0]
		m.queue = m.queue[1:]
		return m.startTurn(next, "")
	}
	return nil
}

// replayHistory draws a resumed session's recorded conversation, so picking up
// a session looks like continuing it rather than opening an empty window.
func (m *tuiModel) replayHistory(state session.State) {
	for _, msg := range state.Messages {
		for _, b := range msg.Content {
			switch b := b.(type) {
			case provider.Text:
				switch msg.Role {
				case provider.RoleUser:
					m.addUser(b.Text)
				case provider.RoleAssistant:
					e := m.tr.add(&entry{kind: kindAssistant, text: b.Text})
					m.tr.finalize(e)
				}
			case provider.Thinking:
				if b.Text != "" {
					e := m.tr.add(&entry{kind: kindThinking, text: b.Text})
					m.tr.finalize(e)
				}
			case provider.ToolUse:
				m.tr.add(&entry{kind: kindTool, tool: toolEntry{name: b.Name, done: true}})
			}
		}
	}
	m.tr.scrollToBottom()
}

// --- transcript events -----------------------------------------------------

func (m *tuiModel) onDelta(msg deltaMsg) {
	last := m.tr.last()
	want := kindAssistant
	if msg.thinking {
		want = kindThinking
	}
	if last != nil && last.live && last.kind == want {
		m.tr.appendText(len(m.tr.entries)-1, msg.text)
		return
	}
	// A new block closes the one before it: the completed text now renders
	// through glamour once instead of on every remaining delta.
	m.tr.finalize(last)
	m.tr.add(&entry{kind: want, text: msg.text, live: true})
}

func (m *tuiModel) onToolStart(msg toolStartMsg) {
	m.tr.finalize(m.tr.last())
	desc := msg.req.Path
	if msg.req.Effect == permission.EffectExecute {
		desc = tools.Describe(msg.req.Argv, msg.req.Shell)
	}
	m.tr.add(&entry{kind: kindTool, tool: toolEntry{name: msg.name, desc: desc}})
}

func (m *tuiModel) onToolEnd(msg toolEndMsg) {
	if last := m.tr.last(); last != nil && last.kind == kindTool && !last.tool.done {
		last.tool.done = true
		last.tool.failed = msg.res.IsError
		last.tool.took = msg.took
		last.tool.detail = msg.res.Content
		m.tr.invalidate(len(m.tr.entries) - 1)
	}
}

func (m *tuiModel) addUser(text string) {
	m.tr.add(&entry{kind: kindUser, text: text})
	m.tr.add(&entry{kind: kindInfo, text: ""})
	m.tr.scrollToBottom()
}

func (m *tuiModel) addNotice(level, text string) {
	m.tr.finalize(m.tr.last())
	m.tr.add(&entry{kind: kindNotice, level: level, text: text})
}

func (m *tuiModel) addInfo(text string) {
	m.tr.add(&entry{kind: kindInfo, text: text})
}

func (m *tuiModel) addRoute(summary string, lines []string) {
	m.tr.add(&entry{kind: kindRoute, routeSummary: summary, routeLines: lines})
}

func routeSummary(d route.Decision) string {
	return fmt.Sprintf("%s via %s (%s)", d.Tier, d.Source, d.Rationale)
}

// --- status state ----------------------------------------------------------

func (m *tuiModel) refreshCost(state session.State) {
	info, _, ok := m.app.catalog.Lookup(m.app.loop.Target)
	switch {
	case !ok:
		m.costLine = "unpriced"
	case info.Free():
		m.costLine = "local"
	default:
		m.costLine = catalog.Money(state.CostMicroUSD).String()
	}
}

func (m *tuiModel) refreshCtxWindow() {
	if info, _, ok := m.app.catalog.Lookup(m.app.loop.Target); ok {
		m.ctxWindow = info.ContextWindow
		return
	}
	m.ctxWindow = 0
}

// --- view ------------------------------------------------------------------

func (m *tuiModel) View() string {
	if m.quitting {
		return ""
	}
	if m.full != nil {
		return m.full.view(m.width, m.height, m.th)
	}

	inputZone := m.inputZoneView()
	chrome := 1 // status line
	if m.busy {
		chrome++
	}
	transH := m.height - lipgloss.Height(inputZone) - chrome
	if transH < 1 {
		transH = 1
	}

	parts := []string{m.tr.view(transH), inputZone}
	if m.busy {
		parts = append(parts, m.workingLine())
	}
	parts = append(parts, m.statusLine())
	return lipgloss.JoinVertical(lipgloss.Left, parts...)
}

func (m *tuiModel) inputZoneView() string {
	if m.dlg != nil {
		return m.dlg.view(m.width, m.th)
	}
	var parts []string
	switch {
	case m.histSearch:
		parts = append(parts, m.historySearchView())
	case m.suggestionsView() != "":
		parts = append(parts, m.suggestionsView())
	case m.mentionsVisible():
		parts = append(parts, m.mentionsView())
	}
	parts = append(parts, m.ta.View())
	return lipgloss.JoinVertical(lipgloss.Left, parts...)
}

// workingLine is the row that appears under the input while a turn runs.
func (m *tuiModel) workingLine() string {
	elapsed := time.Since(m.started).Round(time.Second)
	line := fmt.Sprintf(" %s working… %s", m.spin.View(), m.th.dim.Render(elapsed.String()))
	if m.turnIn+m.turnOut > 0 {
		line += m.th.dim.Render(fmt.Sprintf(" · ↓%s ↑%s tokens", compact(m.turnIn), compact(m.turnOut)))
	}
	line += m.th.faint.Render(" · esc to interrupt")
	if len(m.queue) > 0 {
		line += m.th.faint.Render(fmt.Sprintf(" · %d queued", len(m.queue)))
	}
	return line
}

func itoa(n int) string { return fmt.Sprint(n) }

// compact renders token counts in the status line: 1234 becomes 1.2k.
func compact(n int) string {
	switch {
	case n >= 1_000_000:
		return fmt.Sprintf("%.1fM", float64(n)/1_000_000)
	case n >= 10_000:
		return fmt.Sprintf("%dk", n/1000)
	case n >= 1000:
		return fmt.Sprintf("%.1fk", float64(n)/1000)
	default:
		return fmt.Sprint(n)
	}
}
