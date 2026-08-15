// Package advisor is §9.2's reviewer run continuously: a second model that
// watches the loop through its observer events and speaks up when the
// evidence says the worker is in trouble.
//
// The §9.2 posture holds throughout. The advisor produces advice and never
// edits: it has no tools, no write path, and its output reaches the worker as
// a labelled user-role message. It is bounded — a consult budget per turn and
// a cooldown between consults — because the failure mode is not the advisor
// malfunctioning but the advisor finding something true to say after every
// round, at a model call each, while the marginal value of each finding
// falls. And it is evidence-scoped: it sees the task, a bounded event log,
// and the trigger that woke it, never the full transcript.
//
// The triggers are the router's own (§8.3): repeated tool calls, error
// spikes, new failure signatures, hedging. Both consumers watch the same
// stream for the same reason — those are the shapes a stuck agent makes — and
// a signal vocabulary that diverged between them would mean one of the two
// was wrong about what stuck looks like.
package advisor

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/cj-vana/switchboard/internal/agent"
	"github.com/cj-vana/switchboard/internal/permission"
	"github.com/cj-vana/switchboard/internal/provider"
	route "github.com/cj-vana/switchboard/internal/router"
	"github.com/cj-vana/switchboard/internal/session"
	"github.com/cj-vana/switchboard/internal/tools"
)

const (
	// DefaultMaxConsultsPerTurn is §9.2's two-round bound, applied to
	// consults: provisional, not a measured optimum.
	DefaultMaxConsultsPerTurn = 2

	DefaultCooldown = 45 * time.Second

	// maxEvidenceLines bounds what a consult sees. The advisor reads a tail,
	// not a transcript: recent events are what triggered it, and a full
	// history at advisor prices is §9.2's "expensive and mostly noise".
	maxEvidenceLines = 40
	maxLineBytes     = 400
)

const systemPrompt = `You are a senior engineer watching a coding agent work on a task. You see its recent actions and their results. Something in that stream looks like trouble; you were woken to look at it.

Reply with advice for the agent: two to five sentences that would unstick it or stop it repeating a mistake. Be concrete — name the command, file, or assumption at fault. You cannot edit anything; the agent decides what to do with what you say.

If, on inspection, the agent is actually doing fine, reply with exactly NONE.`

// Advisor watches one loop. It implements agent.Observer by wrapping the
// observer the loop already had, so it sees exactly what the surface sees.
type Advisor struct {
	inner    agent.Observer
	client   provider.Provider
	target   provider.RouteTarget
	onAdvice func(text string)

	maxConsults int
	cooldown    time.Duration

	mu          sync.Mutex
	task        string
	events      []string
	consults    int
	lastConsult time.Time
	inflight    bool
	pending     []string
	pendingArgv map[string]string
	detector    *route.Detector
}

// Option configures an Advisor at construction; there is no reconfiguration
// mid-flight, because a bound that can be raised while the loop runs is not a
// bound.
type Option func(*Advisor)

func WithBounds(maxConsultsPerTurn int, cooldown time.Duration) Option {
	return func(a *Advisor) {
		if maxConsultsPerTurn > 0 {
			a.maxConsults = maxConsultsPerTurn
		}
		if cooldown > 0 {
			a.cooldown = cooldown
		}
	}
}

// New builds an advisor around the observer the loop already had. onAdvice is
// called off the loop goroutine whenever the consult produces something; the
// caller renders it and decides nothing else, because the advice also queues
// itself for injection.
func New(inner agent.Observer, client provider.Provider, target provider.RouteTarget, onAdvice func(string), opts ...Option) *Advisor {
	a := &Advisor{
		inner:       inner,
		client:      client,
		target:      target,
		onAdvice:    onAdvice,
		maxConsults: DefaultMaxConsultsPerTurn,
		cooldown:    DefaultCooldown,
		pendingArgv: map[string]string{},
		detector:    route.NewDetector(),
	}
	for _, o := range opts {
		o(a)
	}
	return a
}

// Target reports which model advises, for status displays.
func (a *Advisor) Target() provider.RouteTarget { return a.target }

// SetInner repoints the wrapped observer. The surface rebuilds its own
// observer when the tier moves; the advisor survives the rebuild by wrapping
// whatever replaced it.
func (a *Advisor) SetInner(inner agent.Observer) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.inner = inner
}

// StartTurn resets the per-turn evidence and budget. Pending advice survives:
// generated at one turn's end, it is delivered into the next.
func (a *Advisor) StartTurn(task string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.task = task
	a.events = nil
	a.consults = 0
	a.detector.Reset()
}

// Drain returns queued advice as labelled user-role messages and clears the
// queue. The loop calls this between tool rounds; the surface calls it when
// folding leftovers into the next prompt.
func (a *Advisor) Drain() []provider.Message {
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(a.pending) == 0 {
		return nil
	}
	out := make([]provider.Message, 0, len(a.pending))
	for _, text := range a.pending {
		out = append(out, provider.UserText("[advisor] A second model reviewing this session says:\n\n"+text))
	}
	a.pending = nil
	return out
}

// --- agent.Observer ---------------------------------------------------------

func (a *Advisor) ThinkingDelta(text string) { a.inner.ThinkingDelta(text) }

func (a *Advisor) TextDelta(text string) {
	a.inner.TextDelta(text)
	a.observe(a.detector.AssistantText(text))
}

func (a *Advisor) ToolStart(name string, req permission.Request) {
	a.inner.ToolStart(name, req)
	argv := strings.Join(req.Argv, " ")
	a.mu.Lock()
	if argv != "" {
		a.pendingArgv[name] = argv
	}
	a.record(fmt.Sprintf("tool %s %s %s", name, req.Path, argv))
	a.mu.Unlock()
	a.observe(a.detector.ToolCall(name, []byte(req.Path+"\x00"+argv)))
}

func (a *Advisor) ToolEnd(name string, res tools.Result, took time.Duration) {
	a.inner.ToolEnd(name, res, took)
	a.mu.Lock()
	argv := a.pendingArgv[name]
	delete(a.pendingArgv, name)
	status := "ok"
	if res.IsError {
		status = "FAILED"
	}
	a.record(fmt.Sprintf("  → %s in %s: %s", status, took.Round(time.Second), firstLine(res.Content)))
	a.mu.Unlock()
	a.observe(a.detector.ToolResult(name, argv, res.Content, res.IsError))
}

func (a *Advisor) Notice(level, text string) {
	a.inner.Notice(level, text)
	if level == "error" || level == "warn" {
		a.mu.Lock()
		a.record("notice " + level + ": " + text)
		a.mu.Unlock()
	}
}

func (a *Advisor) TurnUsage(u session.Usage) { a.inner.TurnUsage(u) }

// --- consulting -------------------------------------------------------------

func (a *Advisor) observe(signals []route.Signal) {
	for _, s := range signals {
		a.maybeConsult(string(s))
	}
}

// maybeConsult starts a consult for a trigger unless the turn's budget says
// no. The call runs on its own goroutine: the worker does not wait for advice,
// it keeps working and the advice lands at the next round boundary.
func (a *Advisor) maybeConsult(trigger string) {
	a.mu.Lock()
	if a.inflight || a.consults >= a.maxConsults || time.Since(a.lastConsult) < a.cooldown {
		a.mu.Unlock()
		return
	}
	a.inflight = true
	a.consults++
	a.lastConsult = time.Now()
	task := a.task
	evidence := strings.Join(a.events, "\n")
	a.mu.Unlock()

	go func() {
		defer func() {
			a.mu.Lock()
			a.inflight = false
			a.mu.Unlock()
		}()

		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		advice, err := a.consult(ctx, task, evidence, trigger)
		if err != nil || advice == "" {
			// A failed or empty consult is silent by design: an advisor that
			// narrates its own outages is noise on top of trouble.
			return
		}
		a.mu.Lock()
		a.pending = append(a.pending, advice)
		a.mu.Unlock()
		if a.onAdvice != nil {
			a.onAdvice(advice)
		}
	}()
}

func (a *Advisor) consult(ctx context.Context, task, evidence, trigger string) (string, error) {
	var b strings.Builder
	fmt.Fprintf(&b, "The task the agent is working on:\n%s\n\n", task)
	fmt.Fprintf(&b, "What woke you: %s\n\n", trigger)
	fmt.Fprintf(&b, "The agent's recent actions, oldest first:\n%s\n", evidence)

	req := provider.Request{
		System:   []provider.Block{provider.Text{Text: systemPrompt}},
		Messages: []provider.Message{provider.UserText(b.String())},
	}
	stream, err := a.client.Stream(ctx, a.target, req)
	if err != nil {
		return "", err
	}
	defer stream.Close()

	var out strings.Builder
	for {
		ev, err := stream.Next()
		if err != nil {
			return "", err
		}
		switch ev.Type {
		case provider.EventTextDelta:
			out.WriteString(ev.Text)
		case provider.EventDone:
			text := strings.TrimSpace(out.String())
			if text == "NONE" || strings.HasPrefix(text, "NONE") {
				return "", nil
			}
			return text, nil
		}
	}
}

// record appends one evidence line under the caller's lock, keeping the tail.
func (a *Advisor) record(line string) {
	if len(line) > maxLineBytes {
		line = line[:maxLineBytes] + "…"
	}
	a.events = append(a.events, line)
	if len(a.events) > maxEvidenceLines {
		a.events = a.events[len(a.events)-maxEvidenceLines:]
	}
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
