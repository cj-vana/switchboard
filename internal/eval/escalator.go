package eval

import (
	"strings"
	"time"

	"github.com/cjvana/switchboard/internal/agent"
	"github.com/cjvana/switchboard/internal/catalog"
	"github.com/cjvana/switchboard/internal/permission"
	"github.com/cjvana/switchboard/internal/provider"
	"github.com/cjvana/switchboard/internal/router"
	"github.com/cjvana/switchboard/internal/session"
	"github.com/cjvana/switchboard/internal/tools"
)

// escalator lets the routed arm change its target mid-task.
//
// This is the same wiring cmd/sb uses, and it is here rather than shared with it
// because the two want different things at the edges: the terminal renders every
// move for the user, and the harness only needs to count them. What must not
// differ is the policy, and that comes from the same package either way.
type escalator struct {
	sticky  *router.Sticky
	detect  *router.Detector
	ladder  []Arm
	catalog *catalog.Catalog

	loop  *agent.Loop
	inner agent.Observer

	moves       int
	pendingArgv map[string]string
	visited     []provider.RouteTargetID
}

func (e *escalator) attach(loop *agent.Loop) {
	e.loop = loop
	e.inner = loop.Observer
	e.pendingArgv = map[string]string{}
	e.visited = []provider.RouteTargetID{loop.Target.ID()}
	loop.Observer = e
}

// finalTarget reports where the run ended up, which is what the cost was
// actually paid to. Reporting the opening choice would attribute an escalated
// run's spend to the rung it started on.
func (e *escalator) finalTarget(start Arm) provider.RouteTargetID {
	if e.loop == nil {
		return start.Target.ID()
	}
	return e.loop.Target.ID()
}

func (e *escalator) ThinkingDelta(text string) { e.inner.ThinkingDelta(text) }

func (e *escalator) TextDelta(text string) {
	e.inner.TextDelta(text)
	e.observe(e.detect.AssistantText(text))
}

func (e *escalator) ToolStart(name string, req permission.Request) {
	e.inner.ToolStart(name, req)
	if len(req.Argv) > 0 {
		e.pendingArgv[name] = strings.Join(req.Argv, " ")
	}
	e.observe(e.detect.ToolCall(name, []byte(req.Path+"\x00"+strings.Join(req.Argv, " "))))
}

func (e *escalator) ToolEnd(name string, res tools.Result, took time.Duration) {
	e.inner.ToolEnd(name, res, took)
	e.observe(e.detect.ToolResult(name, e.pendingArgv[name], res.Content, res.IsError))
	delete(e.pendingArgv, name)
	e.assess()
}

func (e *escalator) Notice(level, text string) { e.inner.Notice(level, text) }

func (e *escalator) TurnUsage(u session.Usage) { e.inner.TurnUsage(u) }

func (e *escalator) observe(signals []router.Signal) {
	for _, s := range signals {
		e.sticky.Observe(s)
	}
}

func (e *escalator) assess() {
	before := e.sticky.Rank()
	move := e.sticky.AfterCall(len(e.ladder) - 1)
	if move.Direction == 0 || e.sticky.Rank() == before {
		return
	}

	arm := e.ladder[e.sticky.Rank()]
	e.loop.Target = arm.Target
	e.loop.Provider = arm.Provider
	e.moves++
	e.visited = append(e.visited, arm.Target.ID())
}

// Visited is every target the run touched, in order. A routed run that only
// ever touched one is a fixed baseline, and the report says so rather than
// letting the arm names imply otherwise.
func (e *escalator) Visited() []provider.RouteTargetID { return e.visited }
