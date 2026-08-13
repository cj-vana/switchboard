// Package permission decides whether a tool call may proceed.
//
// Check is a pure function of mode, rules, host capability, and the request. It
// performs no I/O and never prompts: prompting is a terminal concern, and the
// core is not allowed to know about terminals (design principle 1). A caller
// that gets Ask resolves it through an Asker.
package permission

import (
	"context"
	"fmt"
	"path"
	"strings"
	"sync"

	"github.com/cjvana/switchboard/internal/execution"
)

type Mode string

const (
	// ModePlan is read-only. Writes and execution are denied outright rather
	// than prompted, because the point of the mode is that nothing happens.
	ModePlan Mode = "plan"

	ModeDefault     Mode = "default"
	ModeAcceptEdits Mode = "acceptEdits"

	// ModeBypass suppresses prompts inside a granted sandbox. Without verified
	// containment there is no sandbox to be inside, so execution still prompts.
	ModeBypass Mode = "bypass"
)

func ParseMode(s string) (Mode, error) {
	switch Mode(s) {
	case ModePlan, ModeDefault, ModeAcceptEdits, ModeBypass:
		return Mode(s), nil
	default:
		return "", fmt.Errorf("unknown mode %q: want plan, default, acceptEdits, or bypass", s)
	}
}

type Decision string

const (
	Allow Decision = "allow"
	Deny  Decision = "deny"
	Ask   Decision = "ask"
)

// Effect is what a tool does to the world. Modes are defined over effects
// rather than tool names so a new tool inherits the right policy by declaring
// what it does, instead of by being added to a list in this package.
type Effect string

const (
	EffectRead    Effect = "read"
	EffectWrite   Effect = "write"
	EffectExecute Effect = "execute"
)

// Request is a tool call described in the terms rules match on.
type Request struct {
	Tool   string
	Effect Effect

	// Path is set for file tools, already resolved and workspace-checked by the
	// tool itself. The engine matches on it but does not police the boundary.
	Path string

	// Argv and Shell describe an execution. Matching on them improves prompts
	// and rule ergonomics; it is not a security boundary, since an allowed
	// interpreter or package manager runs arbitrary code either way (§10.2).
	Argv  []string
	Shell bool

	// Network asks for egress off the machine. §11 grants it separately from
	// filesystem access, so it is a distinct field rather than an argv pattern:
	// a command that can reach the internet can send the workspace anywhere,
	// which is a different decision from letting it run at all.
	Network bool
}

type Rule struct {
	Decision Decision

	// An empty field matches anything.
	Tool     string
	Effect   Effect
	PathGlob string

	// ArgvPrefix matches the leading elements of Argv, so a rule can name
	// "go test" without enumerating its flags.
	ArgvPrefix []string

	// Shell restricts a rule to shell or argv mode. Nil matches both.
	Shell *bool
}

type Outcome struct {
	Decision Decision
	Reason   string

	// SandboxAbsent marks an execution that will run without verified
	// containment, whatever the decision. It is set at the moment of the
	// decision rather than left to a status line, because that moment is when
	// the user is deciding, and presenting a prompt as though it were a sandbox
	// is the exact substitution design principle 4 exists to prevent.
	SandboxAbsent bool
}

// Response is a user's answer to an Ask.
type Response struct {
	Approved bool

	// Remember applies the answer to byte-identical later requests for the rest
	// of the session. Matching is exact: remembering an approved argv by prefix
	// would approve commands the user never saw.
	Remember bool
}

// Asker resolves an Ask outcome. cmd/sb implements it against a terminal; a
// headless consumer implements it however it likes.
type Asker interface {
	Ask(ctx context.Context, req Request, outcome Outcome) (Response, error)
}

type Engine struct {
	mu         sync.Mutex
	mode       Mode
	rules      []Rule
	capability execution.Capability
	remembered map[string]bool
}

func NewEngine(mode Mode, capability execution.Capability, rules ...Rule) *Engine {
	return &Engine{
		mode:       mode,
		rules:      rules,
		capability: capability,
		remembered: map[string]bool{},
	}
}

func (e *Engine) Mode() Mode {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.mode
}

func (e *Engine) SetMode(m Mode) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.mode = m
}

func (e *Engine) Capability() execution.Capability { return e.capability }

// Check evaluates a request. Precedence, highest first:
//
//  1. An explicit deny rule.
//  2. The mode floor, so plan mode cannot be widened by a rule.
//  3. A session-remembered answer.
//  4. An explicit allow or ask rule.
//  5. The mode's default for the effect.
//
// A resulting Allow on execution is then downgraded to Ask whenever containment
// is unverified.
func (e *Engine) Check(req Request) Outcome {
	e.mu.Lock()
	mode := e.mode
	remembered, wasRemembered := e.remembered[rememberKey(req)]
	e.mu.Unlock()

	for _, r := range e.rules {
		if r.Decision == Deny && r.matches(req) {
			return Outcome{Decision: Deny, Reason: "denied by rule"}
		}
	}

	if mode == ModePlan && req.Effect != EffectRead {
		return Outcome{
			Decision: Deny,
			Reason:   "plan mode is read-only; switch modes to make changes",
		}
	}

	if wasRemembered {
		if !remembered {
			return Outcome{Decision: Deny, Reason: "you declined this exact request earlier in the session"}
		}
		return e.gate(Outcome{
			Decision: Allow,
			Reason:   "approved earlier in this session",
		}, req, alreadyApproved)
	}

	for _, r := range e.rules {
		if r.Decision != Deny && r.matches(req) {
			return e.gate(Outcome{Decision: r.Decision, Reason: "matched a rule"}, req, gateNormally)
		}
	}

	return e.gate(e.modeDefault(mode, req), req, gateNormally)
}

func (e *Engine) modeDefault(mode Mode, req Request) Outcome {
	if req.Effect == EffectRead {
		return Outcome{Decision: Allow, Reason: "reads do not change the workspace"}
	}

	switch mode {
	case ModeAcceptEdits:
		if req.Effect == EffectWrite {
			return Outcome{Decision: Allow, Reason: "acceptEdits approves file changes"}
		}
		return Outcome{Decision: Ask, Reason: "acceptEdits does not cover running commands"}
	case ModeBypass:
		return Outcome{Decision: Allow, Reason: "bypass mode"}
	default:
		if req.Effect == EffectWrite {
			return Outcome{Decision: Ask, Reason: "changes a file in the workspace"}
		}
		return Outcome{Decision: Ask, Reason: "runs a command"}
	}
}

// gateExecution is the single place automatic execution can be granted, which
// is what makes the sandbox requirement impossible to route around.
type gateKind int

const (
	gateNormally gateKind = iota
	alreadyApproved
)

func (e *Engine) gateExecution(out Outcome, req Request, kind gateKind) Outcome {
	if req.Effect != EffectExecute || e.capability.AutomaticExecutionAllowed() {
		return out
	}

	out.SandboxAbsent = true

	// An answer the user already gave for this exact command stands: they
	// approved the command itself, not a claim about isolation.
	if out.Decision == Allow && kind == gateNormally {
		out.Decision = Ask
		out.Reason = "no verified sandbox on this host, so every command needs approval"
	}
	return out
}

// gateNetwork prompts for egress even on a host with verified containment. The
// sandbox confines what a command can read and write; it cannot judge whether
// sending this workspace to the internet is what the user wanted.
func (e *Engine) gateNetwork(out Outcome, req Request, kind gateKind) Outcome {
	if !req.Network || req.Effect != EffectExecute {
		return out
	}
	if out.Decision != Allow || kind == alreadyApproved {
		return out
	}
	return Outcome{
		Decision:      Ask,
		Reason:        "this command asks for network access, which can send the workspace off this machine",
		SandboxAbsent: out.SandboxAbsent,
	}
}

// gate is the single path to an allowed execution. Both conditions run here so
// no caller can satisfy one and forget the other.
func (e *Engine) gate(out Outcome, req Request, kind gateKind) Outcome {
	return e.gateNetwork(e.gateExecution(out, req, kind), req, kind)
}

// Remember records an answer for the rest of the session.
func (e *Engine) Remember(req Request, approved bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.remembered[rememberKey(req)] = approved
}

// Resolve runs Check and, when the answer is Ask, consults the Asker. It
// returns whether the call may proceed along with the outcome that produced
// that answer, so the caller can record both.
func (e *Engine) Resolve(ctx context.Context, asker Asker, req Request) (bool, Outcome, error) {
	out := e.Check(req)
	switch out.Decision {
	case Allow:
		return true, out, nil
	case Deny:
		return false, out, nil
	}

	if asker == nil {
		return false, Outcome{
			Decision: Deny,
			Reason:   out.Reason + " (no way to ask in this environment)",
		}, nil
	}

	resp, err := asker.Ask(ctx, req, out)
	if err != nil {
		return false, out, err
	}
	if resp.Remember {
		e.Remember(req, resp.Approved)
	}
	return resp.Approved, out, nil
}

func (r Rule) matches(req Request) bool {
	if r.Tool != "" && r.Tool != req.Tool {
		return false
	}
	if r.Effect != "" && r.Effect != req.Effect {
		return false
	}
	if r.Shell != nil && *r.Shell != req.Shell {
		return false
	}
	if r.PathGlob != "" {
		ok, err := path.Match(r.PathGlob, req.Path)
		if err != nil || !ok {
			return false
		}
	}
	if len(r.ArgvPrefix) > 0 {
		if len(req.Argv) < len(r.ArgvPrefix) {
			return false
		}
		for i, want := range r.ArgvPrefix {
			if req.Argv[i] != want {
				return false
			}
		}
	}
	return true
}

func rememberKey(req Request) string {
	var b strings.Builder
	b.WriteString(string(req.Effect))
	b.WriteByte('\x00')
	b.WriteString(req.Tool)
	b.WriteByte('\x00')
	b.WriteString(req.Path)
	b.WriteByte('\x00')
	if req.Shell {
		b.WriteString("shell")
	}
	if req.Network {
		b.WriteString("+net")
	}
	for _, a := range req.Argv {
		b.WriteByte('\x00')
		b.WriteString(a)
	}
	return b.String()
}
