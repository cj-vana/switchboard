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
	"github.com/cj-vana/switchboard/internal/checkpoint"
	"github.com/cj-vana/switchboard/internal/config"
	"github.com/cj-vana/switchboard/internal/credential"
	"github.com/cj-vana/switchboard/internal/delegate"
	"github.com/cj-vana/switchboard/internal/execution"
	"github.com/cj-vana/switchboard/internal/permission"
	"github.com/cj-vana/switchboard/internal/provider"
	route "github.com/cj-vana/switchboard/internal/router"
	"github.com/cj-vana/switchboard/internal/session"
	"github.com/cj-vana/switchboard/internal/skills"
	"github.com/cj-vana/switchboard/internal/tools"
	"github.com/cj-vana/switchboard/internal/trust"
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
type tierNowMsg struct {
	line string
	rank int // destination rung, for the junction marker's heat color
}
type tierSwitchMsg struct {
	tier   config.Tier
	client provider.Provider
	silent bool   // a /tN override restoring what it borrowed, not a user switch
	note   string // a fallback substitution, rendered before content is sent
	err    error
}
type sessionSwapMsg struct {
	sess   *session.Session
	tier   config.Tier
	client provider.Provider
	fresh  bool
	note   string // rendered after the swap; how a fork says where it came from
	err    error

	// andThen, when set, runs once the swap has landed — how /retry sends
	// its replay into the forked session rather than the one it left.
	andThen tea.Cmd
}
type overrideProbeMsg struct {
	prompt string
	images []provider.Image
	tier   config.Tier
	client provider.Provider
	note   string
	err    error
}
type updateCheckMsg struct {
	latest string
	err    error
}
type updateAppliedMsg struct{ version string }
type copyMsg struct {
	n    int
	what string // "response" or "code block"; the notice names what landed
	err  error
}
type disarmQuitMsg struct{}
type doctorDoneMsg struct{ report string }

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
	costPct         int // spend as a percentage of the /budget ceiling; 0 when ungoverned
	turnIn, turnOut int
	callTokens      int
	ctxWindow       int
	updateAvail     string

	// moves is every rung the session landed on after a switch, in order:
	// the status bar's routing-history dots. /why keeps the reasons; this
	// keeps the shape of the day.
	moves []int

	// sessionAt anchors the status clock: how long this session has been
	// open, not how long a turn has run.
	sessionAt time.Time

	// The streaming-rate sparkline: samples holds recent tokens-per-second
	// estimates (chars/4, which is why the readout says ~), tokChars counts
	// stream bytes since tokAt.
	samples  []float64
	tokChars int
	tokAt    time.Time

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

	// race is the paired trial in flight, nil otherwise; raceLog keeps each
	// verdict's one-line summary for /why, the way routeLog keeps moves.
	race    *raceRun
	raceLog []string

	// Reverse history search (tui_history.go).
	histSearch bool
	histQuery  string
	histMatch  int

	// Transcript search, ctrl+f (tui_search.go).
	trSearch  bool
	trQuery   string
	trMatches []int
	trMatch   int

	// custom holds the markdown-file commands loaded at startup
	// (tui_custom.go).
	custom []customCommand

	dlg  dialog
	full *diffView

	pendingAsk  chan permission.Response
	restoreTier *config.Tier
	lastTitle   string
	quitArmed   bool
	quitting    bool

	// watchFails is the last /watch run's failure count for the status
	// chip: 0 is green, -1 means the verifier itself could not run.
	watchFails int

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
	trustStore *trust.Store,
	trustErr error,
	mcpEnv *mcpState,
	undoRec *checkpoint.Recorder,
	agents []delegate.Agent,
	agentNotes []string,
	budget *budgetState,
	skillList []skills.Skill,
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
		trust:      trustStore,
		mcp:        mcpEnv,
		undo:       undoRec,
		agents:     agents,
		agentNotes: agentNotes,
		skills:     skillList,
		budget:     budget,
	}
	if trustErr != nil {
		app.trustErr = trustErr.Error()
	}

	m := newTUIModel(app, th, md, ta)

	p := tea.NewProgram(m, tea.WithAltScreen(), tea.WithMouseCellMotion())
	obs.p = p
	app.p = p
	// Subagent rails render through the raw observer, not the watcher:
	// a delegate's stumbles must never escalate the primary.
	subagentForward.set(obs)
	app.watcher = newWatcher(obs, sticky, len(cfg.Tiers)-1, app.moveTo)
	loop.Observer = app.watcher
	loop.Asker = &tuiAsker{p: p}
	// The injection seam is composed once and never swapped: the advisor and
	// the watch each contribute nothing while off.
	loop.Inject = app.inject

	m.addBanner(sess, resumed)
	// Startup notes render into the model directly, because the program is
	// not consuming messages yet; whatever a server says later arrives as a
	// notice through the observer, which is how the user learns a server
	// died an hour into the session.
	for _, n := range mcpEnv.attach(func(n mcpNote) { obs.Notice(n.level, n.text) }) {
		if n.level == "" {
			m.addInfo("  " + n.text)
		} else {
			m.addNotice(n.level, n.text)
		}
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
	// of them: this workspace, this tier. It goes through syncTitle so the
	// startup title and every later update are the same format by
	// construction.
	if cmd := m.syncTitle(); cmd != nil {
		initial = append(initial, cmd)
	}
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
// chord, and the box grows with its content. The bubbles defaults paint the
// cursor line with their own background; that slab is cleared here and the
// composer's frame does the marking instead.
func newTextarea() textarea.Model {
	ta := textarea.New()
	ta.Prompt = "› "
	ta.Placeholder = "describe a task · / commands · @ files · ! shell"
	ta.ShowLineNumbers = false
	ta.CharLimit = 0
	ta.SetHeight(1)
	ta.SetWidth(94)
	ta.KeyMap.InsertNewline.SetKeys("ctrl+j", "alt+enter")
	ta.FocusedStyle.CursorLine = lipgloss.NewStyle()
	ta.BlurredStyle.CursorLine = lipgloss.NewStyle()
	ta.Focus()
	return ta
}

// newTUIModel assembles the model around an app. It is separate from runTUI so
// tests can drive the model without a terminal.
func newTUIModel(app *tuiApp, th *theme, md *markdown, ta textarea.Model) *tuiModel {
	if app.watchSt == nil {
		app.watchSt = &watchState{}
	}
	m := &tuiModel{
		app:       app,
		th:        th,
		md:        md,
		ta:        ta,
		spin:      spinner.New(spinner.WithSpinner(spinner.Dot)),
		tierLine:  app.tierLine(),
		mode:      app.loop.Perms.Mode(),
		history:   loadHistory(app.workspace),
		custom:    loadCustomCommands(app.workspace),
		sessionAt: time.Now(),
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
		m.ta.SetWidth(msg.Width - 6) // margin, frame, and padding
		return m, m.syncTitle()

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
		case tea.MouseButtonLeft:
			// A click on a tool rail or a route line toggles it, the same
			// toggle ctrl+o applies to the most recent one: the transcript
			// is directly manipulable where it has something to show.
			if msg.Action == tea.MouseActionPress && m.dlg == nil {
				if i := m.tr.entryAt(msg.Y); i >= 0 {
					if e := m.tr.entries[i]; e.kind == kindTool || e.kind == kindRoute {
						e.expanded = !e.expanded
						m.tr.invalidate(i)
					}
				}
			}
		}
		return m, nil

	case tea.KeyMsg:
		return m, m.key(msg)

	case spinner.TickMsg:
		if m.busy {
			var cmd tea.Cmd
			m.spin, cmd = m.spin.Update(msg)
			m.sampleRate()
			return m, tea.Batch(cmd, m.syncTitle())
		}
		return m, m.syncTitle()

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

	case watchReportMsg:
		m.onWatchReport(msg)
		return m, nil

	case retryStartMsg:
		return m, m.retryStart(msg)

	case usageMsg:
		m.turnIn += msg.u.Usage.InputTokens + msg.u.Usage.CacheWriteTokens
		m.turnOut += msg.u.Usage.OutputTokens
		m.callTokens = msg.u.Usage.InputTokens + msg.u.Usage.CacheReadTokens + msg.u.Usage.CacheWriteTokens
		return m, nil

	case askMsg:
		m.pendingAsk = msg.respond
		m.dlg = newPermissionDialog(msg.req, msg.out, msg.respond)
		m.ring()
		return m, nil

	case pickerMsg:
		m.dlg = &pickerDialog{title: msg.title, items: msg.items, onPick: msg.action}
		return m, nil

	case secretPromptMsg:
		m.dlg = newSecretDialog(msg.ref, msg.storeName, func(value string) tea.Cmd {
			store := storeSecretCmd(msg.ref, msg.writer, msg.storeName, value)
			if msg.then != nil {
				return tea.Sequence(store, msg.then)
			}
			return store
		})
		return m, nil

	case turnDoneMsg:
		m.ring()
		return m, tea.Batch(m.onTurnDone(msg), m.syncTitle())

	case tierNowMsg:
		// The policy moved the primary mid-turn: the junction marker wears
		// the destination rung's heat, the same color every routing surface
		// speaks.
		m.tr.finalize(m.tr.last())
		m.tr.add(&entry{kind: kindNotice, level: "route", text: msg.line, rank: msg.rank})
		m.routeLog = append(m.routeLog, msg.line)
		m.recordMove(msg.rank)
		m.tierLine = m.app.tierLine()
		m.refreshCtxWindow()
		return m, m.syncTitle()

	case tierSwitchMsg:
		return m, tea.Batch(m.onTierSwitch(msg), m.syncTitle())

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

	case doctorDoneMsg:
		m.addInfo(msg.report)
		return m, nil

	case copyMsg:
		what := msg.what
		if what == "" {
			what = "response"
		}
		if msg.err != nil {
			m.addNotice("error", "copy failed: "+msg.err.Error())
		} else {
			m.addNotice("", "copied "+what+" "+itoa(msg.n)+" to the clipboard")
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

	case raceProbeMsg:
		return m, m.onRaceProbe(msg)

	case raceToolMsg:
		m.onRaceTool(msg)
		return m, nil

	case raceUsageMsg:
		m.onRaceUsage(msg)
		return m, nil

	case raceNoticeMsg:
		m.onRaceNotice(msg)
		return m, nil

	case raceArmDoneMsg:
		return m, m.onRaceArmDone(msg)

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
	if m.trSearch {
		m.transcriptSearchKey(msg)
		return nil
	}

	switch msg.String() {
	case "ctrl+c":
		return m.interrupt()
	case "ctrl+r":
		m.startHistorySearch()
		return nil
	case "ctrl+f":
		m.startTranscriptSearch()
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
	case "home":
		// The endpoints of the scroll story: home is the session's opening,
		// end is where the work is. Both are one press because reaching
		// either by page is a chore proportional to the day's length.
		m.tr.scrollBy(len(m.tr.flat))
		return nil
	case "end":
		m.tr.scrollToBottom()
		return nil
	case "alt+1", "alt+2", "alt+3", "alt+4", "alt+5", "alt+6", "alt+7", "alt+8", "alt+9":
		// The ladder under the fingers: alt+N jumps straight to rung N.
		// Plain digits belong to the composer; the modifier is what makes
		// a rung switch deliberate rather than a typo.
		pressed := msg.String()
		idx := int(pressed[len(pressed)-1] - '1')
		if idx >= len(m.app.config.Tiers) {
			return noticeCmd("", fmt.Sprintf("the ladder has %d rungs; alt+%c names none", len(m.app.config.Tiers), pressed[len(pressed)-1]))
		}
		if m.busy {
			return noticeCmd("warn", "a turn is running; esc to interrupt it first")
		}
		return m.app.switchTier(m.app.config.Tiers[idx].ID)
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
	if m.race != nil && m.race.cancel != nil {
		m.race.cancelled = true
		m.race.cancel()
		m.addNotice("", "cancelling the race; the session stays where it was")
		return nil
	}
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
		probe := func(p string) tea.Cmd {
			return func() tea.Msg {
				probed, client, note, err := m.app.providers.probeTierFallback(context.Background(), tier)
				return overrideProbeMsg{prompt: p, tier: probed, client: client, note: note, err: err}
			}
		}
		if leaks := credential.ScanPrompt(prompt); len(leaks) > 0 {
			return m.openSecretGate(leaks, prompt, probe)
		}
		return probe(prompt)
	}

	// The transcript shows what was typed; the model gets that plus what the
	// @mentions attach and what recent ! commands produced. Expansion happens
	// here, not at submit, so a queued prompt reads its files when it runs.
	m.addUser(prompt)
	expanded, images := m.expandMentions(prompt)
	prompt = m.watchContext(m.adviceContext(m.shellContext(expanded)))
	if len(images) > 0 {
		if reason, refused := m.visionRefusal(); refused {
			m.addNotice("error", reason)
			return nil
		}
	}
	// The scan runs on the expanded prompt, because an @mentioned .env or a
	// `!env` transcript is exactly the outbound copy a key rides in on.
	if leaks := credential.ScanPrompt(prompt); len(leaks) > 0 {
		return m.openSecretGate(leaks, prompt, func(p string) tea.Cmd {
			return m.launchTurn(p, images)
		})
	}
	return m.launchTurn(prompt, images)
}

// launchTurn is startTurn's tail, split off so the secret gate can hold a
// turn while the user decides what leaves the machine.
func (m *tuiModel) launchTurn(prompt string, images []provider.Image) tea.Cmd {
	m.beginTurn(prompt)
	go m.runTurn(m.turnCtx, prompt, images)
	return m.spin.Tick
}

// visionRefusal says why an attached image cannot ride to the active
// target. The evidence order is the §4 one: a live probe that attested
// image input wins, then a catalog entry that carries vision from its own
// verification; with neither, the attach is refused with the reason rather
// than sent to fail — or worse, to be silently ignored.
func (m *tuiModel) visionRefusal() (string, bool) {
	target := m.app.tier.Target
	if attested, _ := m.app.providers.probedVision(target); attested {
		return "", false
	}
	if info, _, ok := m.app.catalog.Lookup(target); ok && info.Vision {
		return "", false
	}
	return string(target.ID()) + " has no evidence it takes images — neither its live probe nor the catalog says so; " +
		"switch to a rung whose model does, or send the prompt without the image mention", true
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
	m.recordMove(m.app.rankOf(msg.tier))

	m.addUser(msg.prompt)
	m.beginTurn(msg.prompt)
	go m.runTurn(m.turnCtx, msg.prompt, msg.images)
	return m.spin.Tick
}

func (m *tuiModel) beginTurn(prompt string) {
	m.busy = true
	m.started = time.Now()
	m.turnIn, m.turnOut = 0, 0
	m.samples, m.tokChars, m.tokAt = nil, 0, time.Time{}
	m.turnPrompt = prompt
	m.turnStarted = m.app.tier
	m.turnBefore = m.app.loop.Session.State()
	m.tr.scrollToBottom()
	ctx, cancel := context.WithCancel(context.Background())
	m.turnCancel = cancel
	m.turnCtx = ctx
	m.app.watchSt.beginTurn(ctx)
}

// runTurn drives one turn on its own goroutine. Everything it reports arrives
// as messages; the session stays the only thing it writes.
func (m *tuiModel) runTurn(ctx context.Context, prompt string, images []provider.Image) {
	m.app.watcher.StartTurn()
	if m.app.advisor != nil {
		m.app.advisor.StartTurn(prompt)
	}
	opening := provider.UserText(prompt)
	for _, img := range images {
		opening.Content = append(opening.Content, img)
	}
	err := m.app.loop.TurnMessage(ctx, opening)

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
	// The final round's edits have no later round boundary; this is theirs.
	// Batched into every exit path, because a tier restore or a queued
	// prompt does not unhappen the edits.
	watchCmd := m.watchTurnEnd()
	m.tr.finalizeAll()
	m.refreshCost(msg.after)
	m.app.route = nil // the opening decision describes the opening choice only

	// The working line's past tense: what ran, for how long, on how many
	// tokens, said once and left in the record. It speaks the rail's own
	// verdict language, closing the rail when one is open directly above.
	if msg.err == nil {
		done := fmt.Sprintf("%s · %s", m.turnStarted.ID, time.Since(m.started).Round(time.Second))
		if m.turnIn+m.turnOut > 0 {
			done += fmt.Sprintf(" · ↓%s ↑%s tokens", compact(m.turnIn), compact(m.turnOut))
		}
		last := m.tr.last()
		m.tr.add(&entry{kind: kindNotice, level: "done", text: done,
			rank: m.activeRank(), rail: last != nil && last.kind == kindTool})
	}
	m.samples = nil
	m.tokChars, m.tokAt = 0, time.Time{}

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
			return tea.Batch(watchCmd, m.restoreCmd(restore))
		}
	}

	// Auto-compaction runs ahead of the queue: a queued prompt sent into a
	// nearly-full window would inherit the failure this exists to prevent,
	// and the queue survives the swap (onSessionSwap drains it).
	if m.shouldAutoCompact() {
		pct := m.callTokens * 100 / m.ctxWindow
		m.addNotice("", fmt.Sprintf("context at %d%% of %s tokens; compacting automatically (/compact auto off disables this)",
			pct, compact(m.ctxWindow)))
		return tea.Batch(watchCmd, compactCmd(m, "", true))
	}

	if len(m.queue) > 0 {
		next := m.queue[0]
		m.queue = m.queue[1:]
		return tea.Batch(watchCmd, m.startTurn(next, ""))
	}
	return watchCmd
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
		probed, client, note, err := m.app.providers.probeTierFallback(context.Background(), tier)
		return tierSwitchMsg{tier: probed, client: client, note: note, err: err, silent: true}
	}
}

func (m *tuiModel) onTierSwitch(msg tierSwitchMsg) tea.Cmd {
	if msg.err != nil {
		m.addNotice("error", msg.err.Error())
		return nil
	}
	if msg.note != "" {
		m.addNotice("warn", msg.note)
		m.app.loop.Session.AppendNote("warn", msg.note)
	}
	m.app.bind(msg.tier, msg.client, true)
	m.tierLine = m.app.tierLine()
	m.refreshCtxWindow()
	m.recordMove(m.app.rankOf(msg.tier))
	if !msg.silent {
		m.tr.add(&entry{kind: kindNotice, level: "route", text: "now on " + m.tierLine,
			rank: m.app.rankOf(msg.tier)})
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
	// The swapped-in context has read nothing, whatever the registry
	// remembers from the old one: reads must happen again before writes,
	// the same contract resume enforces by starting a fresh process.
	m.app.loop.Tools.ForgetAllVersions()
	m.tr.reset()
	// A new log is a new day for the routing dots and the clock; a resumed
	// session's earlier moves live in its record, not the bar.
	m.moves = nil
	m.sessionAt = time.Now()
	m.addBanner(msg.sess, !msg.fresh)
	if !msg.fresh {
		m.replayHistory(msg.sess.State())
	}
	if msg.note != "" {
		m.addNotice("", msg.note)
	}
	m.tierLine = m.app.tierLine()
	m.mode = m.app.loop.Perms.Mode()
	m.refreshCost(msg.sess.State())
	m.refreshCtxWindow()
	// The old session's occupancy does not describe the new one, and leaving
	// it would re-trigger the auto-compaction that produced this swap.
	m.callTokens = 0

	// A swap that carries its own continuation runs it now; /retry's replay
	// belongs to the fork it just landed in, ahead of anything queued.
	if msg.andThen != nil {
		return msg.andThen
	}

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
				// A replayed session does not record which rung ran each
				// call, so history renders neutral rather than guessing.
				m.tr.add(&entry{kind: kindTool, tool: toolEntry{name: b.Name, done: true}, rank: -1})
			}
		}
	}
	m.tr.scrollToBottom()
}

// --- transcript events -----------------------------------------------------

func (m *tuiModel) onDelta(msg deltaMsg) {
	m.tokChars += len(msg.text)
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

// activeRank is the current tier's position on the ladder, for the heat ramp;
// an ad-hoc target (-model, a resumed unknown) has no rung and renders
// neutral.
func (m *tuiModel) activeRank() int {
	return m.app.rankOf(m.app.tier)
}

func (m *tuiModel) onToolStart(msg toolStartMsg) {
	m.tr.finalize(m.tr.last())
	m.tr.add(&entry{kind: kindTool, tool: toolEntry{name: msg.name, desc: describeRequest(msg.req)}, rank: m.activeRank()})
}

func (m *tuiModel) onToolEnd(msg toolEndMsg) {
	// Pair by name, newest first: a delegate's sub-tool rails land between
	// the delegate's own start and end, so the entry finishing is not
	// necessarily the last one.
	idx := -1
	for i := len(m.tr.entries) - 1; i >= 0; i-- {
		e := m.tr.entries[i]
		if e.kind == kindTool && !e.tool.done && e.tool.name == msg.name {
			idx = i
			break
		}
	}
	if idx < 0 {
		return
	}
	e := m.tr.entries[idx]
	// A finished todo call renders as the list itself rather than a verdict
	// line: the list is the result. A failed call keeps the rail so the
	// error shows the way every other tool error does.
	if msg.name == "todo" && !msg.res.IsError {
		e.kind = kindTodo
		e.todos = m.app.loop.Tools.Todos()
		m.tr.invalidate(idx)
		return
	}
	e.tool.done = true
	e.tool.failed = msg.res.IsError
	e.tool.took = msg.took
	e.tool.detail = msg.res.Content
	m.tr.invalidate(idx)
}

func (m *tuiModel) addUser(text string) {
	m.tr.add(&entry{kind: kindUser, text: text})
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
	m.tr.add(&entry{kind: kindRoute, routeSummary: summary, routeLines: lines, rank: m.activeRank()})
}

func routeSummary(d route.Decision) string {
	return fmt.Sprintf("%s via %s (%s)", d.Tier, d.Source, d.Rationale)
}

// --- status state ----------------------------------------------------------

func (m *tuiModel) refreshCost(state session.State) {
	// The ratio resets before the branch, not inside one: a switch from a
	// priced rung to a local one must not leave "local" wearing the old
	// ceiling's warning color.
	m.costPct = 0
	// The three zero-dollar meterings stay distinct (§4), here as everywhere:
	// a plan target consumed quota, not nothing.
	info, _, ok := m.app.catalog.Lookup(m.app.loop.Target)
	switch {
	case !ok:
		m.costLine = "unpriced"
	case info.Metering == catalog.Local:
		m.costLine = "local"
	case info.Metering == catalog.Plan:
		m.costLine = "plan"
	case info.Free():
		m.costLine = "free"
	default:
		m.costLine = catalog.Money(state.CostMicroUSD).String()
		// The ceiling rides the readout so a governed session shows it at
		// rest, the same principle as the tier: visible, not on demand. The
		// percentage feeds the readout's color, so a ceiling being neared
		// warms the same way the context gauge does: the warning comes
		// before the refusal, not as it.
		if m.app.budget != nil {
			if c := m.app.budget.get(); c > 0 {
				m.costLine += " of " + c.String()
				m.costPct = int(int64(state.CostMicroUSD) * 100 / int64(c))
			}
		}
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
	rail := m.height >= 15 // a short pane spends its rows on content
	if rail {
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
	if rail {
		parts = append(parts, m.ctxRail())
	}
	parts = append(parts, m.statusLine())
	return lipgloss.JoinVertical(lipgloss.Left, parts...)
}

// inputZoneView is the composer: a rounded frame one shade off the page,
// the cursor in the active rung's heat — what you type is marked with the
// color it will run on — and the frame itself takes the permission mode's
// color the moment the mode is anything but default, so a widened posture
// is visible at the exact place the next instruction is typed. Popups dock
// above the frame.
func (m *tuiModel) inputZoneView() string {
	if m.dlg != nil {
		return m.dlg.view(m.width, m.th)
	}

	m.ta.FocusedStyle.Prompt = m.th.faint
	m.ta.BlurredStyle.Prompt = m.th.faint
	m.ta.FocusedStyle.CursorLine = lipgloss.NewStyle()
	m.ta.BlurredStyle.CursorLine = lipgloss.NewStyle()
	m.ta.FocusedStyle.Placeholder = m.th.faint
	m.ta.BlurredStyle.Placeholder = m.th.faint
	if rank := m.activeRank(); rank >= 0 && !m.busy {
		m.ta.Cursor.Style = lipgloss.NewStyle().Foreground(m.th.rung(rank).GetForeground())
	} else {
		m.ta.Cursor.Style = lipgloss.NewStyle()
	}

	frame := m.th.border.GetForeground()
	if m.mode != permission.ModeDefault {
		if chip, ok := m.th.modeChip[string(m.mode)]; ok {
			frame = chip.GetBackground()
		}
	}
	box := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(frame).
		Padding(0, 1).
		Width(max(m.width-4, 20))

	var parts []string
	switch {
	case m.trSearch:
		parts = append(parts, m.transcriptSearchView())
	case m.histSearch:
		parts = append(parts, m.historySearchView())
	case m.suggestionsView() != "":
		parts = append(parts, m.suggestionsView())
	case m.mentionsVisible():
		parts = append(parts, m.mentionsView())
	}
	parts = append(parts, box.Render(m.ta.View()))

	lines := strings.Split(lipgloss.JoinVertical(lipgloss.Left, parts...), "\n")
	for i, l := range lines {
		lines[i] = " " + l
	}
	return strings.Join(lines, "\n")
}

// workVerbs are the operator's verbs: what the person behind a switchboard
// did all day. One is chosen every few seconds of a running turn, so the
// working line has a pulse beyond the spinner without inventing progress.
var workVerbs = []string{"patching through", "on the line", "connecting", "holding the line", "splicing"}

// workingLine is the row that appears under the input while a turn runs:
// spinner and verb in the active rung's heat, then the rung and the clock,
// then the way out. Color answers "who is working" before text does. Token
// counts live in the completion line and /cost.
func (m *tuiModel) workingLine() string {
	verb := workVerbs[int(time.Since(m.started).Seconds()/4)%len(workVerbs)]
	who := m.spin.View() + " " + verb
	mid := ""
	if rank := m.activeRank(); rank >= 0 {
		who = m.th.rung(rank).Render(who)
		mid = m.th.dim.Render(" · " + m.app.tier.ID)
	}
	elapsed := time.Since(m.started).Round(time.Second)
	line := " " + who + mid + m.th.dim.Render(" · "+elapsed.String())
	line += m.th.faint.Render("  esc interrupts")
	if len(m.queue) > 0 {
		line += m.th.faint.Render(fmt.Sprintf("  %d queued", len(m.queue)))
	}
	return line
}

// recordMove appends a landed switch to the session's routing history, the
// status bar's dots. Every rebind counts, however it was asked for: the dots
// have to agree with /why about how much the session moved.
func (m *tuiModel) recordMove(rank int) {
	if rank < 0 {
		return
	}
	m.moves = append(m.moves, rank)
}

// sampleRate folds the stream bytes seen since the last sample into a
// tokens-per-second estimate for the sparkline. Chars over four is an
// estimate and the readout marks it as one.
func (m *tuiModel) sampleRate() {
	if m.tokAt.IsZero() {
		m.tokAt = time.Now()
		return
	}
	since := time.Since(m.tokAt)
	if since < 400*time.Millisecond {
		return
	}
	rate := float64(m.tokChars) / 4 / since.Seconds()
	m.samples = append(m.samples, rate)
	if len(m.samples) > 10 {
		m.samples = m.samples[len(m.samples)-10:]
	}
	m.tokChars = 0
	m.tokAt = time.Now()
}

// ring speaks when the session needs its person: on the ask and on the
// turn's end. It goes to stderr because the renderer owns stdout and a BEL
// prints nothing; /notify off keeps the quiet.
func (m *tuiModel) ring() {
	if m.app.config.NotifyOn() {
		os.Stderr.WriteString("\a")
	}
}

// syncTitle keeps the terminal title naming the workspace and the active
// tier, marked while a turn runs, so the working pane is findable from a
// wall of terminals. It returns nil when nothing changed, because a title
// rewrite per tick would be chatter.
func (m *tuiModel) syncTitle() tea.Cmd {
	title := m.titleText()
	if title == m.lastTitle {
		return nil
	}
	m.lastTitle = title
	return tea.SetWindowTitle(title)
}

func (m *tuiModel) titleText() string {
	title := "sb · " + filepath.Base(m.app.workspace) + " · " + m.app.tier.ID
	if m.busy {
		title = "● " + title
	}
	return title
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
