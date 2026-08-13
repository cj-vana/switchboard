package main

import (
	"strings"
	"time"

	"github.com/cjvana/switchboard/internal/agent"
	"github.com/cjvana/switchboard/internal/permission"
	route "github.com/cjvana/switchboard/internal/router"
	"github.com/cjvana/switchboard/internal/session"
	"github.com/cjvana/switchboard/internal/tools"
)

// watcher feeds the escalation policy from what the loop is already reporting.
//
// It wraps the renderer rather than changing the loop, because the observer
// already carries everything §8.3's detectable triggers need: which tool ran,
// with what arguments, whether it failed, and what the model said. Threading a
// second callback through the loop for the same events would be two things to
// keep in step.
//
// Every method passes through to the renderer first. A watcher that swallowed
// output while deciding what to escalate would trade the thing the user is
// watching for a decision they cannot see.
type watcher struct {
	inner    agent.Observer
	detector *route.Detector
	sticky   *route.Sticky
	out      *renderer

	// maxRank is the top of the configured ladder, so a move that would run off
	// the end is reported rather than silently ignored.
	maxRank int

	// onMove is called when the primary actually changes, so the caller can
	// rebind the target without this needing to know how.
	onMove func(rank int, why string)

	// pendingArgv remembers what the running command was, because a result
	// carries no arguments and "did a test run fail" needs both.
	pendingArgv map[string]string
}

func newWatcher(inner agent.Observer, out *renderer, sticky *route.Sticky, maxRank int, onMove func(int, string)) *watcher {
	return &watcher{
		inner:       inner,
		detector:    route.NewDetector(),
		sticky:      sticky,
		out:         out,
		maxRank:     maxRank,
		onMove:      onMove,
		pendingArgv: map[string]string{},
	}
}

// StartTurn resets the per-turn state. Failure signatures do not carry across
// turns, because §8.3 counts consecutive failures within one.
func (w *watcher) StartTurn() {
	w.detector.Reset()
	w.sticky.StartTurn()
}

func (w *watcher) ThinkingDelta(text string) { w.inner.ThinkingDelta(text) }

func (w *watcher) TextDelta(text string) {
	w.inner.TextDelta(text)
	w.observe(w.detector.AssistantText(text))
}

func (w *watcher) ToolStart(name string, req permission.Request) {
	w.inner.ToolStart(name, req)
	if len(req.Argv) > 0 {
		w.pendingArgv[name] = strings.Join(req.Argv, " ")
	}
	// The permission request carries the resolved arguments, which is what
	// makes two calls comparable: the raw JSON would differ on formatting.
	w.observe(w.detector.ToolCall(name, []byte(req.Path+"\x00"+strings.Join(req.Argv, " "))))
}

func (w *watcher) ToolEnd(name string, res tools.Result, took time.Duration) {
	w.inner.ToolEnd(name, res, took)
	w.observe(w.detector.ToolResult(name, w.pendingArgv[name], res.Content, res.IsError))
	delete(w.pendingArgv, name)

	// A move is assessed per model call rather than per tool call, but a tool
	// result is the last thing to happen before the next call, so this is where
	// the accumulated evidence is weighed.
	w.assess()
}

func (w *watcher) Notice(level, text string) { w.inner.Notice(level, text) }

func (w *watcher) TurnUsage(u session.Usage) { w.inner.TurnUsage(u) }

func (w *watcher) observe(signals []route.Signal) {
	for _, s := range signals {
		w.sticky.Observe(s)
	}
}

func (w *watcher) assess() {
	move := w.sticky.AfterCall(w.maxRank)
	switch {
	case move.Direction != 0:
		// §8.1 renders the reason rather than logging it, and a target changing
		// under the user is exactly the case principle 3 is about.
		direction := "escalated"
		if move.Direction < 0 {
			direction = "stepped down"
		}
		w.out.Notice("route", direction+": "+move.Rationale)
		if w.onMove != nil {
			w.onMove(w.sticky.Rank(), move.Rationale)
		}

	case move.Held:
		// Saying that a switch was warranted and held is worth as much as the
		// switch itself: otherwise the dwell looks like the policy doing
		// nothing.
		w.out.Notice("route", move.Rationale)
	}
}
