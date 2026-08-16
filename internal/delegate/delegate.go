// Package delegate lets the primary model hand a scoped task to a subagent
// on a rung of its choosing.
//
// This is the ladder's idea applied to orchestration: a search, a survey, or
// a mechanical edit does not need the primary's rung, and a subagent on t1
// with its own fresh context is often cheaper than the primary doing the
// work inside a context it then drags forward forever. The tier parameter is
// the visible, priced version of that decision — the same bet the router
// makes, made available to the model. §19.2 phase 6 expects delegation to be
// evaluated against sticky single-primary baselines; that eval has not run,
// so this ships as a tool the user can watch, not as a claim it wins.
//
// Depth is one: a subagent's registry has no delegate tool, because an agent
// that can recurse is an agent whose cost has no ceiling.
package delegate

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/cj-vana/switchboard/internal/agent"
	"github.com/cj-vana/switchboard/internal/config"
	"github.com/cj-vana/switchboard/internal/permission"
	"github.com/cj-vana/switchboard/internal/provider"
	"github.com/cj-vana/switchboard/internal/session"
	"github.com/cj-vana/switchboard/internal/tools"
)

// MaxRounds bounds a subagent's turn tighter than the primary's, because a
// subagent stuck in a retry cycle has no user watching to interrupt it.
const MaxRounds = 25

// Preamble is appended to the subagent's system blocks. It states the one
// fact a fresh context cannot know: that only the final message survives.
const Preamble = "You are a delegated subagent. Complete the task you are given and reply " +
	"with your findings. Your final message is returned verbatim to the agent that " +
	"delegated the task, and nothing else you produce survives, so put everything " +
	"that matters in it."

// Config wires a delegate tool to the session's machinery. Every closure is
// supplied by the surface (cmd/sb), because building a provider client, a
// session, and a loop is assembly, and assembly lives where the credentials
// and the catalog do.
type Config struct {
	// Tiers is the ladder, for validation and for the default rung.
	Tiers []config.Tier

	// Probe resolves a tier to a live client, the same way a manual /t2
	// switch does: a tier that cannot be served is an error, not a fallback.
	Probe func(ctx context.Context, tierID string) (config.Tier, provider.Provider, error)

	// NewSession creates the subagent's own session record. Sub-sessions are
	// real logs — crash-safe, auditable — kept out of the primary store so
	// /resume never offers a context that was never the user's.
	NewSession func(target provider.RouteTargetID) (*session.Session, error)

	// NewLoop assembles a loop for the subagent: fresh registry without
	// delegate, the shared permission engine and asker, the parent's hooks.
	// A non-nil named agent carries the definition's prompt and tool grant
	// for the assembly to apply.
	NewLoop func(tier config.Tier, client provider.Provider, sess *session.Session, obs agent.Observer, named *Agent) (*agent.Loop, error)

	// Forward receives the subagent's tool activity so the user watches the
	// work as it happens. Nil means unobserved.
	Forward func() agent.Observer

	// Agents are the named definitions discovered at session assembly,
	// sorted by name. Empty leaves the tool exactly as it is without them —
	// same schema, same description — so a session with no definitions
	// renders byte-identical requests.
	Agents []Agent
}

func (c Config) defaultTier() string {
	if len(c.Tiers) == 0 {
		return ""
	}
	return c.Tiers[0].ID
}

// New builds the delegate tool. It returns an error when the ladder is too
// small to delegate on, so the tool is absent rather than broken.
func New(c Config) (tools.Tool, error) {
	if len(c.Tiers) == 0 {
		return nil, fmt.Errorf("delegate needs a configured ladder")
	}
	if c.Probe == nil || c.NewSession == nil || c.NewLoop == nil {
		return nil, fmt.Errorf("delegate is missing assembly wiring")
	}
	return &delegateTool{c: c}, nil
}

type delegateTool struct{ c Config }

func (t *delegateTool) Name() string { return "delegate" }

func (t *delegateTool) Description() string {
	var ids []string
	for _, tier := range t.c.Tiers {
		ids = append(ids, tier.ID)
	}
	desc := fmt.Sprintf("Hand a self-contained task to a subagent with a fresh context and return "+
		"its final answer. The subagent has the core tools but cannot delegate further, "+
		"and it starts with no knowledge of this conversation, so the task must carry "+
		"everything it needs: file paths, constraints, what to return. tier picks the "+
		"ladder rung it runs on (%s); the default %s is the cheap rung, right for "+
		"searches, surveys, and mechanical work. Use a higher tier only when the "+
		"subtask itself is hard.", strings.Join(ids, ", "), t.c.defaultTier())
	if len(t.c.Agents) == 0 {
		return desc
	}
	var b strings.Builder
	b.WriteString(desc)
	b.WriteString(" Named agents carry standing instructions and their own default rung and " +
		"tool grant; pass one as agent when its charter fits the subtask:")
	for _, ag := range t.c.Agents {
		fmt.Fprintf(&b, "\n- %s", ag.Name)
		if ag.Description != "" {
			fmt.Fprintf(&b, ": %s", ag.Description)
		}
		if ag.Tier != "" {
			fmt.Fprintf(&b, " (runs on %s)", ag.Tier)
		}
	}
	return b.String()
}

// ParallelSafe is false: subagents share the permission engine and the
// observer, and two of them interleaving prompts and rails would be
// unattributable noise.
func (t *delegateTool) ParallelSafe() bool { return false }

func (t *delegateTool) Schema() json.RawMessage {
	if len(t.c.Agents) == 0 {
		return json.RawMessage(`{
  "type": "object",
  "properties": {
    "task": {"type": "string", "description": "Complete instructions for the subagent, self-contained: it starts with no context from this conversation."},
    "tier": {"type": "string", "description": "Ladder rung to run on, e.g. t1. Defaults to the bottom rung."}
  },
  "required": ["task"]
}`)
	}
	var names []string
	for _, ag := range t.c.Agents {
		names = append(names, ag.Name)
	}
	quoted, _ := json.Marshal(names)
	return json.RawMessage(`{
  "type": "object",
  "properties": {
    "task": {"type": "string", "description": "Complete instructions for the subagent, self-contained: it starts with no context from this conversation."},
    "tier": {"type": "string", "description": "Ladder rung to run on, e.g. t1. Defaults to the agent's rung, then the bottom rung."},
    "agent": {"type": "string", "enum": ` + string(quoted) + `, "description": "Named agent to run as: its standing instructions, default rung, and tool grant apply."}
  },
  "required": ["task"]
}`)
}

type delegateInput struct {
	Task  string `json:"task"`
	Tier  string `json:"tier"`
	Agent string `json:"agent"`
}

func (t *delegateTool) Plan(input json.RawMessage) (tools.Plan, error) {
	var in delegateInput
	if err := json.Unmarshal(input, &in); err != nil {
		return tools.Plan{}, fmt.Errorf("delegate: %w", err)
	}
	if strings.TrimSpace(in.Task) == "" {
		return tools.Plan{}, fmt.Errorf("delegate: task is required")
	}
	var named *Agent
	if in.Agent != "" {
		for i := range t.c.Agents {
			if t.c.Agents[i].Name == in.Agent {
				named = &t.c.Agents[i]
				break
			}
		}
		if named == nil {
			return tools.Plan{}, fmt.Errorf("delegate: no agent %q is defined", in.Agent)
		}
	}
	// An explicit tier wins over the agent's default, which wins over the
	// ladder's bottom: the caller saying "run it on t3" is the more specific
	// intent, whoever the agent is.
	if in.Tier == "" && named != nil {
		in.Tier = named.Tier
	}
	if in.Tier == "" {
		in.Tier = t.c.defaultTier()
	}
	found := false
	for _, tier := range t.c.Tiers {
		if tier.ID == in.Tier {
			found = true
			break
		}
	}
	if !found {
		return tools.Plan{}, fmt.Errorf("delegate: no tier %q in the ladder", in.Tier)
	}

	// Spawning is free and touches nothing; every call the subagent then
	// makes goes through the shared permission engine on its own merits, so
	// the spawn itself carries the read effect.
	summary := in.Task
	if len(summary) > 80 {
		summary = summary[:80] + "…"
	}
	who := in.Tier
	if named != nil {
		who = named.Name + " on " + in.Tier
	}
	return tools.Plan{
		Request: permission.Request{
			Tool:   t.Name(),
			Effect: permission.EffectRead,
			Detail: fmt.Sprintf("%s → %s", who, summary),
		},
		Run: func(ctx context.Context) (tools.Result, error) {
			return t.run(ctx, in, named)
		},
	}, nil
}

func (t *delegateTool) run(ctx context.Context, in delegateInput, named *Agent) (tools.Result, error) {
	tier, client, err := t.c.Probe(ctx, in.Tier)
	if err != nil {
		return tools.Result{Content: fmt.Sprintf("tier %s cannot be served: %v", in.Tier, err), IsError: true}, nil
	}

	sess, err := t.c.NewSession(tier.Target.ID())
	if err != nil {
		return tools.Result{Content: fmt.Sprintf("could not record a delegate session: %v", err), IsError: true}, nil
	}
	defer sess.Close()

	var obs agent.Observer = agent.NopObserver{}
	if t.c.Forward != nil {
		if fwd := t.c.Forward(); fwd != nil {
			obs = &forwarding{parent: fwd}
		}
	}

	loop, err := t.c.NewLoop(tier, client, sess, obs, named)
	if err != nil {
		return tools.Result{Content: fmt.Sprintf("could not assemble the subagent: %v", err), IsError: true}, nil
	}

	started := time.Now()
	turnErr := loop.Turn(ctx, in.Task)
	state := sess.State()
	answer := finalText(state)

	who := "on " + tier.ID
	if named != nil {
		who = named.Name + " on " + tier.ID
	}
	trailer := fmt.Sprintf("[delegate %s: %d model calls, %s]",
		who, state.Calls, time.Since(started).Round(time.Second))

	switch {
	case ctx.Err() != nil:
		return tools.Result{}, ctx.Err()
	case turnErr != nil && answer == "":
		return tools.Result{Content: fmt.Sprintf("the subagent failed: %v\n%s", turnErr, trailer), IsError: true}, nil
	case turnErr != nil:
		// A partial answer with a named failure beats discarding the work.
		return tools.Result{Content: fmt.Sprintf("%s\n\n[the subagent stopped early: %v]\n%s", answer, turnErr, trailer)}, nil
	case answer == "":
		return tools.Result{Content: "the subagent finished without a final answer\n" + trailer, IsError: true}, nil
	default:
		return tools.Result{Content: answer + "\n\n" + trailer}, nil
	}
}

// finalText is the last complete assistant message's text, which the
// preamble told the subagent is the part that survives.
func finalText(state session.State) string {
	for i := len(state.Messages) - 1; i >= 0; i-- {
		m := state.Messages[i]
		if m.Role != provider.RoleAssistant || m.Incomplete {
			continue
		}
		if s := strings.TrimSpace(m.Text()); s != "" {
			return s
		}
	}
	return ""
}

// forwarding passes the subagent's tool activity to the parent's observer so
// the rails render live, and swallows the rest: streamed text would
// interleave with the primary's, usage is the sub-session's record, and a
// todo list would collide with the primary's in any surface that renders
// registry state.
type forwarding struct {
	parent agent.Observer
}

func (f *forwarding) ThinkingDelta(string) {}
func (f *forwarding) TextDelta(string)     {}

func (f *forwarding) ToolStart(name string, req permission.Request) {
	if name == "todo" {
		return
	}
	f.parent.ToolStart(name, req)
}

func (f *forwarding) ToolEnd(name string, res tools.Result, took time.Duration) {
	if name == "todo" {
		return
	}
	f.parent.ToolEnd(name, res, took)
}

func (f *forwarding) Notice(level, text string) { f.parent.Notice(level, text) }
func (f *forwarding) TurnUsage(session.Usage)   {}
