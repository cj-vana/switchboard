package main

// /watch: the user's declared verifier, run at the seams of a turn, with
// only the delta reported (internal/watch). The wiring here follows the
// advisor's: evidence reaches the model through the loop's round-boundary
// injection seam, never by rewriting anything already sent, and the same
// evidence feeds the escalation policy through the watcher — §8.4 calls a
// task-specific verifier stronger evidence than the harness's own
// completion signal, and this is where the declaration gets made.
//
// What decides a run is due is the checkpoint recorder's own capture count:
// the loop's evidence that this turn changed files, which is exactly what
// /undo restores from. Mutations the recorder cannot see — a command's side
// effects, a script writing files — do not trip the watch, and that is the
// §8.3 posture, not an oversight: evidence the loop does not keep is absent,
// not guessed.

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/cj-vana/switchboard/internal/credential"
	"github.com/cj-vana/switchboard/internal/provider"
	"github.com/cj-vana/switchboard/internal/watch"
)

// watchInjectLines caps how many new failing lines ride to the model. The
// point of the message is "look at the verifier", not the verifier's whole
// output; the model can run the command itself when three lines are not
// enough.
const watchInjectLines = 3

type watchReportMsg struct {
	command string
	rep     watch.Report
	turnEnd bool
}

// watchState is the mutable half of the feature, guarded because the loop's
// goroutine consults it at round boundaries while the UI goroutine arms,
// disarms, and folds.
type watchState struct {
	mu      sync.Mutex
	w       *watch.Watch
	turnCtx context.Context

	// lastPending is the recorder's capture count when the verifier last
	// ran, reset each turn with the recorder's own scope. A run is due when
	// the count has grown past it. gen counts turns, because a turn-end run
	// finishes on its own goroutine and may land after the next turn has
	// begun — its stale count must not overwrite the fresh turn's zero, or
	// the new turn's first edits would never look new.
	lastPending int
	gen         int

	// fold holds turn-end reports for the next prompt, the seam advice and
	// ! output already use: one user message per turn.
	fold []string
}

func (ws *watchState) arm(w *watch.Watch) {
	ws.mu.Lock()
	defer ws.mu.Unlock()
	ws.w = w
	ws.lastPending = 0
	ws.fold = nil
}

func (ws *watchState) disarm() *watch.Watch {
	ws.mu.Lock()
	defer ws.mu.Unlock()
	w := ws.w
	ws.w = nil
	ws.fold = nil
	return w
}

func (ws *watchState) armed() *watch.Watch {
	ws.mu.Lock()
	defer ws.mu.Unlock()
	return ws.w
}

// beginTurn resets the per-turn counter alongside the recorder's new scope
// and remembers the turn's context, so an interrupted turn interrupts a
// mid-turn verifier run with it.
func (ws *watchState) beginTurn(ctx context.Context) {
	ws.mu.Lock()
	defer ws.mu.Unlock()
	ws.lastPending = 0
	ws.gen++
	ws.turnCtx = ctx
}

// due reports whether the verifier should run now: armed, and the turn has
// captured files it has not seen. The generation comes back with the
// verdict so the eventual ran() can be told from a stale one.
func (ws *watchState) due(pending int) (*watch.Watch, context.Context, int, bool) {
	ws.mu.Lock()
	defer ws.mu.Unlock()
	if ws.w == nil || pending <= ws.lastPending {
		return nil, nil, 0, false
	}
	ctx := ws.turnCtx
	if ctx == nil {
		ctx = context.Background()
	}
	return ws.w, ctx, ws.gen, true
}

// ran records how much of the turn the verifier has seen. A count from a
// finished generation is dropped: the turn it measured is over, and the
// current one owes its own runs.
func (ws *watchState) ran(gen, pending int) {
	ws.mu.Lock()
	defer ws.mu.Unlock()
	if gen != ws.gen {
		return
	}
	ws.lastPending = pending
}

func (ws *watchState) addFold(text string) {
	ws.mu.Lock()
	defer ws.mu.Unlock()
	ws.fold = append(ws.fold, text)
}

func (ws *watchState) takeFold() []string {
	ws.mu.Lock()
	defer ws.mu.Unlock()
	out := ws.fold
	ws.fold = nil
	return out
}

// inject is the loop's round-boundary seam, composed once at assembly:
// whatever the advisor queued, then the watch's delta. Each part
// contributes nothing when off, so the loop never needs its Inject swapped.
// Every message leaves marked Injected, because a log reader — /retry above
// all — must be able to tell a turn's opening from what rode in mid-turn.
func (a *tuiApp) inject() []provider.Message {
	var out []provider.Message
	if adv := a.advisor; adv != nil {
		out = append(out, adv.Drain()...)
	}
	out = append(out, a.watchRound()...)
	for i := range out {
		out[i].Injected = true
	}
	return out
}

// watchRound runs the verifier at a round boundary when this turn has edits
// it has not checked. It runs on the loop's goroutine deliberately: the
// model is about to build its next request, and "the tests just broke" is
// worth waiting for — a human pair would not keep typing through it either.
func (a *tuiApp) watchRound() []provider.Message {
	if a.undo == nil {
		return nil
	}
	w, ctx, gen, ok := a.watchSt.due(a.undo.PendingFiles())
	if !ok {
		return nil
	}
	rep := w.Run(ctx)
	a.watchSt.ran(gen, a.undo.PendingFiles())
	if a.p != nil {
		a.p.Send(watchReportMsg{command: w.Command(), rep: rep})
	}
	if a.watcher != nil && len(rep.Signatures) > 0 {
		a.watcher.VerifierFailures(rep.Signatures)
	}
	if text := watchInjectText(w.Command(), rep); text != "" {
		return []provider.Message{provider.UserText(text)}
	}
	return nil
}

// watchInjectText is what the model reads. Only a change speaks: a repeat
// verdict injects nothing, because a verifier that repeats itself every
// round teaches its reader to stop reading it.
func watchInjectText(command string, rep watch.Report) string {
	if !rep.Changed() {
		return ""
	}
	var b strings.Builder
	if rep.WentGreen {
		fmt.Fprintf(&b, "[watch] The user's verifier `%s` now passes.", command)
	} else {
		fmt.Fprintf(&b, "[watch] The user's verifier `%s` ran after your edits and reports new failures (exit %d):\n", command, rep.ExitCode)
		for i, f := range rep.New {
			if i == watchInjectLines {
				fmt.Fprintf(&b, "…and %d more new failures\n", len(rep.New)-watchInjectLines)
				break
			}
			b.WriteString(truncate(f.Line, 200) + "\n")
		}
		if rep.Persisting > 0 {
			fmt.Fprintf(&b, "%d earlier failure(s) persist.\n", rep.Persisting)
		}
	}
	text := strings.TrimRight(b.String(), "\n")
	// Verifier output is exactly the surface an env dump leaks a key
	// through, and a round boundary has no one to ask, so this redacts
	// unconditionally — the race record's posture.
	if leaks := credential.ScanPrompt(text); len(leaks) > 0 {
		text = credential.Redact(text, leaks)
	}
	return text
}

// watchTurnEnd covers the edits of a turn's final round, which no later
// round boundary will see. It runs off the UI goroutine and its report
// waits for the next prompt: the turn is over, so there is no request to
// inject into and no escalation left to feed.
func (m *tuiModel) watchTurnEnd() tea.Cmd {
	if m.app.undo == nil {
		return nil
	}
	w, _, gen, ok := m.app.watchSt.due(m.app.undo.PendingFiles())
	if !ok {
		return nil
	}
	pending := m.app.undo.PendingFiles()
	return func() tea.Msg {
		// Deliberately not the turn's context: the turn is over, and this
		// run reports on what it left behind — an esc that ended the turn
		// must not also cancel the report. The watch's own timeout bounds it.
		rep := w.Run(context.Background())
		m.app.watchSt.ran(gen, pending)
		return watchReportMsg{command: w.Command(), rep: rep, turnEnd: true}
	}
}

// onWatchReport renders a run's outcome. The transcript speaks only on a
// change; the status chip always tells the current color.
func (m *tuiModel) onWatchReport(msg watchReportMsg) {
	rep := msg.rep
	if rep.Err != nil {
		m.watchFails = -1
		m.addNotice("warn", fmt.Sprintf("watch: %s could not run: %v", msg.command, rep.Err))
		return
	}
	if rep.Passed {
		m.watchFails = 0
		if rep.WentGreen || rep.FirstRun {
			m.addNotice("watch", fmt.Sprintf("watch: %s is green", msg.command))
		}
		return
	}
	m.watchFails = len(rep.Signatures)
	if len(rep.New) > 0 {
		text := fmt.Sprintf("watch: %s — new failure: %s", msg.command, truncate(rep.New[0].Line, 120))
		if extra := len(rep.New) - 1 + rep.Persisting; extra > 0 {
			text += fmt.Sprintf(" (+%d more)", extra)
		}
		m.addNotice("warn", text)
		// The moment a verifier turns red at a turn's end is the moment
		// "which turn broke it" becomes askable, so /bisect is named here
		// once — with turns to search, and never again this session,
		// because a lesson repeated is noise.
		if msg.turnEnd && !m.bisectHinted && m.app.undo != nil && len(m.app.undo.Turns()) > 1 {
			m.bisectHinted = true
			m.addNotice("", "/bisect can name the turn that broke it")
		}
	}
	if msg.turnEnd && rep.Changed() {
		if text := watchInjectText(msg.command, rep); text != "" {
			m.app.watchSt.addFold(text)
		}
	}
}

// watchContext folds a turn-end verdict into the next prompt, the same seam
// advice and ! output use and for the same reason: one user message per
// turn. The typed prompt leads and the report follows, so an opening never
// leads with the injection label — which is what lets /retry's shape check
// for unmarked logs stay honest.
func (m *tuiModel) watchContext(prompt string) string {
	folds := m.app.watchSt.takeFold()
	if len(folds) == 0 {
		return prompt
	}
	var b strings.Builder
	b.WriteString(prompt)
	for _, f := range folds {
		b.WriteString("\n\n" + f)
	}
	return b.String()
}

// watchChip is the status bar's readout: green when the last run passed,
// the failure count when it did not, a question mark when the verifier
// itself could not run.
func (m *tuiModel) watchChip() string {
	if m.app.watchSt.armed() == nil {
		return ""
	}
	th := m.th
	switch {
	case m.watchFails < 0:
		return th.onBar(th.warn).Render("watch ?")
	case m.watchFails == 0:
		return th.onBar(th.ok).Render("watch ✓")
	default:
		return th.onBar(th.err).Render(fmt.Sprintf("watch ✗%d", m.watchFails))
	}
}

// suggestVerifier names the verifier this workspace's own files imply, for
// the bare /watch hint. It suggests and never arms: the constraint is that
// a verifier is declared, not inferred, so the user's typing stays the only
// way one starts running. A Makefile's test target outranks the language
// manifests because it is the project's own declaration rather than an
// implication.
func suggestVerifier(workspace string) string {
	read := func(name string) string {
		data, err := os.ReadFile(filepath.Join(workspace, name))
		if err != nil {
			return ""
		}
		return string(data)
	}
	if makeTestTarget.MatchString(read("Makefile")) {
		return "make test"
	}
	if read("go.mod") != "" {
		return "go test ./..."
	}
	if read("Cargo.toml") != "" {
		return "cargo test"
	}
	if read("pytest.ini") != "" || strings.Contains(read("pyproject.toml"), "[tool.pytest") {
		return "pytest"
	}
	if pkg := read("package.json"); pkg != "" {
		var manifest struct {
			Scripts map[string]string `json:"scripts"`
		}
		// The npm placeholder script fails on purpose; suggesting it would
		// arm a verifier that is red by construction.
		if err := json.Unmarshal([]byte(pkg), &manifest); err == nil {
			if s := manifest.Scripts["test"]; s != "" && !strings.Contains(s, "no test specified") {
				return "npm test"
			}
		}
	}
	return ""
}

var makeTestTarget = regexp.MustCompile(`(?m)^test:`)

func cmdWatch(m *tuiModel, args string) tea.Cmd {
	args = strings.TrimSpace(args)
	switch {
	case args == "":
		w := m.app.watchSt.armed()
		if w == nil {
			hint := "/watch <command> arms one"
			if s := suggestVerifier(m.app.workspace); s != "" {
				hint = fmt.Sprintf("this workspace implies `%s`; /watch %s arms it", s, s)
			}
			m.addInfo("  no watch set; " + hint + " — it runs after the model's edits, and only changes are reported")
			return nil
		}
		state := "green"
		if w.Red() {
			state = "failing"
		} else if m.watchFails < 0 {
			state = "could not run"
		}
		m.addInfo(fmt.Sprintf("  watching: %s  (%s; /watch off stops)", w.Command(), state))
		return nil

	case args == "off":
		if w := m.app.watchSt.disarm(); w != nil {
			m.watchFails = 0
			m.app.loop.Session.AppendNote("info", "watch disarmed: "+w.Command())
			return noticeCmd("", "watch off; "+w.Command()+" no longer runs")
		}
		return noticeCmd("", "no watch was set")

	default:
		if m.app.undo == nil {
			return noticeCmd("error", "watch needs the turn checkpoint recorder, which this session does not have")
		}
		m.app.watchSt.arm(watch.New(args, m.app.workspace))
		m.watchFails = 0
		m.app.loop.Session.AppendNote("info", "watch armed: "+args)
		m.addNotice("watch", fmt.Sprintf("watching with `%s`: it runs after the model's edits, unconfined, as you would run it; only changes are reported, and new failures count toward escalation", args))
		return nil
	}
}
