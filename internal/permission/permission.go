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
	"net/url"
	"path"
	"path/filepath"
	"strings"
	"sync"

	"github.com/switchboard-code/switchboard/internal/credential"
	"github.com/switchboard-code/switchboard/internal/execution"
	"github.com/switchboard-code/switchboard/internal/terminaltext"
)

type Mode string

const (
	// ModePlan is read-only. Writes and execution are denied outright rather
	// than prompted, because the point of the mode is that nothing happens.
	ModePlan Mode = "plan"

	ModeDefault     Mode = "default"
	ModeAcceptEdits Mode = "acceptEdits"

	// ModeAuto lets ordinary workspace edits proceed and asks a bounded model
	// reviewer about commands. External tools and sensitive requests never go
	// to the reviewer and still need the user.
	ModeAuto Mode = "auto"

	// ModeYOLO grants ordinary writes and commands direct host reach. Explicit
	// deny rules, the outbound-secret gate, and external tools remain outside
	// that grant. The deliberately conspicuous name matches the CLI contract:
	// this is not a sandbox mode and must never be rendered as one.
	ModeYOLO Mode = "yolo"

	// ModeBypass suppresses prompts inside a granted sandbox. Without verified
	// containment there is no sandbox to be inside, so execution still prompts.
	ModeBypass Mode = "bypass"
)

func ParseMode(s string) (Mode, error) {
	switch Mode(s) {
	case ModePlan, ModeDefault, ModeAcceptEdits, ModeAuto, ModeYOLO, ModeBypass:
		return Mode(s), nil
	case "fullAccess", "full-access":
		return ModeYOLO, nil
	default:
		return "", fmt.Errorf("unknown mode %q: want plan, default, acceptEdits, auto, yolo, or bypass", s)
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

	// EffectExternal marks a tool whose action happens outside the workspace
	// and outside any sandbox this host verified: an MCP server's tool, acting
	// wherever that server acts. No mode auto-allows it, bypass included,
	// because bypass suppresses prompts inside a granted sandbox and an
	// external tool is never inside one. Only an explicit rule or a
	// remembered answer lets one run without asking.
	EffectExternal Effect = "external"
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

	// Sensitive is set by a tool that knows the request could carry a secret
	// even when the generic known-token scan cannot see it. It is a policy bit,
	// never a rendering of the secret. Sensitive execution is not automatically
	// approved by yolo and is never sent to the model reviewer.
	Sensitive bool

	// Execution is the immutable reach snapshot the command's Run closure will
	// validate and apply. Binding it into the request makes permission and
	// execution reason about the same sandbox/network posture.
	Execution *execution.CommandPolicy

	// Detail is display only: the prompt and the transcript show it, rules
	// never match it and the remember key never includes it. External tools
	// use it to show their arguments while the remembered answer stays
	// per-tool, since a user approving an MCP tool approves the tool, not one
	// byte-exact invocation.
	Detail string
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
	// ResolvedBy is populated by Resolve, not Check. It distinguishes an
	// automatic policy answer, a model review, and a human response in the
	// durable permission audit.
	ResolvedBy Resolution

	// SandboxAbsent marks an execution that will run without verified
	// containment, whatever the decision. It is set at the moment of the
	// decision rather than left to a status line, because that moment is when
	// the user is deciding, and presenting a prompt as though it were a sandbox
	// is the exact substitution design principle 4 exists to prevent.
	SandboxAbsent bool

	// FullReach says an execution will run directly on the host. In that
	// posture the Network hint is descriptive rather than enforceable: the
	// process has the account's filesystem and network reach either way.
	FullReach bool

	// Review is set when auto mode consulted its command reviewer. The caller
	// persists it with the permission record, including failures and escalations,
	// so automatic approval is never an invisible side channel.
	Review *ReviewAudit

	// permissionRevision binds a resolved allow to the mode that produced it.
	// It is intentionally opaque outside this package; callers acquire a
	// HoldResolution token immediately before journaling and running effects.
	permissionRevision uint64
}

type Resolution string

const (
	ResolvedByPolicy Resolution = "policy"
	ResolvedByModel  Resolution = "model"
	ResolvedByHuman  Resolution = "human"
)

type ReviewDecision string

const (
	ReviewAllow    ReviewDecision = "allow"
	ReviewDeny     ReviewDecision = "deny"
	ReviewEscalate ReviewDecision = "escalate"
)

// ReviewRequest is the bounded, content-free policy packet given to a cheap
// command reviewer. It carries the command the user would otherwise see and
// the effective reach, but never file contents, environment variables, or an
// external tool's arguments.
type ReviewRequest struct {
	Tool               string
	Effect             Effect
	Path               string
	Argv               []string
	Shell              bool
	Network            bool
	FullReach          bool
	HostLoopbackShared bool
	HostIPCShared      bool
}

type ReviewResult struct {
	Decision ReviewDecision
	Reviewer string
	Reason   string
}

type Reviewer interface {
	Review(context.Context, ReviewRequest) (ReviewResult, error)
}

type ReviewAudit struct {
	Reviewer string
	Decision ReviewDecision
	Reason   string
	Error    string
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
	mu         sync.RWMutex
	postureMu  sync.RWMutex
	mode       Mode
	revision   uint64
	rules      []Rule
	capability execution.Capability
	execution  *execution.Controller
	reviewer   Reviewer
	remembered map[string]bool
}

func NewEngine(mode Mode, capability execution.Capability, rules ...Rule) *Engine {
	// Preserve the original constructor's contract for embedders and tests:
	// verified capability means the sandbox is active. Product assembly uses
	// NewEngineWithExecution so its explicit, default-off controller is shared
	// with every command registry.
	controller, _ := execution.NewController(capability, execution.SandboxAuto)
	return NewEngineWithExecution(mode, controller, rules...)
}

func NewEngineWithExecution(mode Mode, controller *execution.Controller, rules ...Rule) *Engine {
	if controller == nil {
		controller = execution.NewDefaultController(execution.Capability{})
	}
	engine := &Engine{
		mode:       mode,
		revision:   1,
		rules:      rules,
		capability: controller.Capability(),
		execution:  controller,
		remembered: map[string]bool{},
	}
	// A non-yolo engine may be a read-only view over a controller owned by
	// another loop (race branches do this). Constructing that view must not
	// silently narrow the primary loop's live execution posture. An explicit
	// yolo engine can widen its freshly assembled controller; later mode
	// changes are the owning surface's responsibility.
	if mode == ModeYOLO {
		controller.SetFullAccess(true)
	}
	return engine
}

func (e *Engine) Mode() Mode {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.mode
}

func (e *Engine) SetMode(m Mode) {
	e.postureMu.Lock()
	defer e.postureMu.Unlock()
	e.mu.Lock()
	if e.mode == m {
		e.mu.Unlock()
		return
	}
	if e.execution != nil {
		// Publish the permission mode and command reach under one engine lock.
		// Check cannot observe yolo with a confined controller (or the inverse).
		e.execution.SetFullAccess(m == ModeYOLO)
	}
	e.mode = m
	e.revision++
	e.mu.Unlock()
}

func (e *Engine) Capability() execution.Capability { return e.capability }

func (e *Engine) Execution() *execution.Controller { return e.execution }

func (e *Engine) SetReviewer(reviewer Reviewer) {
	e.mu.Lock()
	e.reviewer = reviewer
	e.mu.Unlock()
}

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
	e.mu.RLock()
	mode := e.mode
	remembered, wasRemembered := e.remembered[rememberKey(req)]
	e.mu.RUnlock()

	for _, r := range e.rules {
		if r.Decision == Deny && r.matches(req) {
			return Outcome{Decision: Deny, Reason: "denied by rule"}
		}
	}
	if req.Effect == EffectExecute && req.Execution != nil {
		if e.execution == nil || e.execution.Validate(*req.Execution, req.Network) != nil {
			return Outcome{Decision: Deny, Reason: "execution reach changed or the policy snapshot was invalid; submit the command again"}
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

	out := e.modeDefault(mode, req)
	out = e.applyBypassBoundary(mode, out, req)
	return e.gate(out, req, gateNormally)
}

func (e *Engine) applyBypassBoundary(mode Mode, out Outcome, req Request) Outcome {
	if mode != ModeBypass || req.Effect != EffectExecute || out.Decision != Allow {
		return out
	}
	switch {
	case !e.sandboxActive(req):
		out.Decision = Ask
		out.Reason = "bypass only suppresses prompts inside an active verified sandbox"
	case e.hostAuthorityShared(req):
		out.Decision = Ask
		out.Reason = "bypass cannot suppress this prompt because host-local network or IPC services can retain authority outside the sandbox"
	}
	return out
}

func (e *Engine) modeDefault(mode Mode, req Request) Outcome {
	if req.Effect == EffectRead {
		return Outcome{Decision: Allow, Reason: "reads do not change the workspace"}
	}
	if req.Effect == EffectExternal {
		return Outcome{Decision: Ask, Reason: "external MCP, web, and computer actions require their own approval; this mode does not cover them"}
	}

	switch mode {
	case ModeAcceptEdits:
		if req.Effect == EffectWrite {
			return Outcome{Decision: Allow, Reason: "acceptEdits approves file changes"}
		}
		return Outcome{Decision: Ask, Reason: "acceptEdits does not cover running commands"}
	case ModeAuto:
		if req.Effect == EffectWrite {
			return Outcome{Decision: Allow, Reason: "auto mode approves ordinary workspace edits"}
		}
		if e.capability.Platform == "windows" {
			return Outcome{Decision: Ask, Reason: "Windows cannot yet guarantee descendant process cleanup, so auto requires your approval instead of model review"}
		}
		if !e.sandboxActive(req) {
			return Outcome{Decision: Ask, Reason: "host-direct commands can execute workspace-controlled code with full account and network reach, so auto keeps them with you; the command reviewer is available only under verified confinement"}
		}
		if sensitive, _ := SensitiveRequest(req); sensitive {
			return Outcome{Decision: Ask, Reason: "this command looks credential-bearing, so auto keeps its metadata away from the model reviewer and asks you"}
		}
		if opaqueInterpreterPayload(req) {
			return Outcome{Decision: Ask, Reason: "shell and inline interpreter code are opaque policy payloads, so auto asks you instead of sending the code to the model reviewer"}
		}
		if e.hostLoopbackShared(req) {
			return Outcome{Decision: Ask, Reason: "this sandbox shares host loopback, where a localhost service can relay off-machine; auto requires your approval instead of model review"}
		}
		return Outcome{Decision: Ask, Reason: "auto mode sends ordinary commands to the configured command reviewer"}
	case ModeYOLO:
		if req.Effect == EffectExecute {
			if sensitive, _ := SensitiveRequest(req); sensitive {
				return Outcome{Decision: Ask, Reason: "credential-shaped or explicitly sensitive command requires user approval even in yolo mode"}
			}
			reason := "yolo mode grants unsandboxed command execution with full host reach"
			if e.capability.Platform == "windows" {
				reason += "; descendant processes may survive cancellation"
			}
			return Outcome{Decision: Allow, Reason: reason}
		}
		return Outcome{Decision: Allow, Reason: "yolo mode approves file changes"}
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
	if req.Effect != EffectExecute {
		return out
	}
	if !e.sandboxActive(req) {
		out.SandboxAbsent = true
		out.FullReach = true
	}
	return out
}

// gateNetwork prompts for egress even on a host with verified containment. The
// sandbox confines what a command can read and write; it cannot judge whether
// sending this workspace to the internet is what the user wanted.
func (e *Engine) gateNetwork(out Outcome, req Request, kind gateKind) Outcome {
	if !req.Network || req.Effect != EffectExecute || !e.sandboxActive(req) {
		return out
	}
	if kind == alreadyApproved || out.Decision == Deny {
		return out
	}
	const warning = "requests full network access, which can send the workspace off this machine"
	if out.Decision == Ask {
		if out.Reason == "" {
			out.Reason = warning
		} else if !strings.Contains(out.Reason, warning) {
			out.Reason += "; " + warning
		}
		return out
	}
	return Outcome{
		Decision:      Ask,
		Reason:        "this command " + warning,
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
	e.mu.RLock()
	revision := e.revision
	e.mu.RUnlock()
	out := e.Check(req)
	out.permissionRevision = revision
	switch out.Decision {
	case Allow:
		out.ResolvedBy = ResolvedByPolicy
		return e.finishResolution(true, out)
	case Deny:
		out.ResolvedBy = ResolvedByPolicy
		return false, out, nil
	}

	if e.reviewEligible(ctx, req) {
		result, reviewErr := e.runReview(ctx, req, out)
		out.Review = &ReviewAudit{Reviewer: cleanReviewText(result.Reviewer), Decision: result.Decision, Reason: cleanReviewText(result.Reason)}
		if reviewErr != nil {
			out.Review.Error = cleanReviewText(reviewErr.Error())
			out.Reason = "command reviewer failed; asking the user"
		} else {
			switch result.Decision {
			case ReviewAllow:
				out.Decision = Allow
				out.ResolvedBy = ResolvedByModel
				out.Reason = "command reviewer " + reviewerName(result.Reviewer) + " allowed this command: " + cleanReviewText(result.Reason)
				return e.finishResolution(true, out)
			case ReviewDeny:
				out.Decision = Deny
				out.ResolvedBy = ResolvedByModel
				out.Reason = "command reviewer " + reviewerName(result.Reviewer) + " denied this command: " + cleanReviewText(result.Reason)
				return false, out, nil
			case ReviewEscalate:
				out.Reason = "command reviewer " + reviewerName(result.Reviewer) + " escalated this command to the user: " + cleanReviewText(result.Reason)
			default:
				out.Review.Error = "reviewer returned an invalid decision"
				out.Reason = "command reviewer returned an invalid decision; asking the user"
			}
		}
	}
	// A reviewer failure or escalation rewrites the explanatory reason above.
	// Reapply the reach warning before a human sees the request so the final
	// prompt and durable outcome cannot hide an explicit egress grant.
	out = e.gateNetwork(out, req, gateNormally)

	if asker == nil {
		out.Decision = Deny
		out.ResolvedBy = ResolvedByPolicy
		out.Reason += " (no way to ask in this environment)"
		return false, out, nil
	}

	resp, err := asker.Ask(ctx, req, out)
	if err != nil {
		return false, out, err
	}
	out.ResolvedBy = ResolvedByHuman
	if resp.Approved {
		out.Decision = Allow
	} else {
		out.Decision = Deny
	}
	approved, out, err := e.finishResolution(resp.Approved, out)
	if err == nil && resp.Remember && out.ResolvedBy == ResolvedByHuman {
		e.Remember(req, approved)
	}
	return approved, out, err
}

func (e *Engine) finishResolution(approved bool, out Outcome) (bool, Outcome, error) {
	if !approved {
		return false, out, nil
	}
	e.mu.RLock()
	current := e.revision == out.permissionRevision
	e.mu.RUnlock()
	if current {
		return true, out, nil
	}
	out.Decision = Deny
	out.ResolvedBy = ResolvedByPolicy
	out.Reason = "permission mode changed while this request was being resolved; submit it again under the current mode"
	return false, out, nil
}

// HoldResolutions acquires one batch lease after every request has resolved and
// before any final audit or side effect. SetMode waits for release. A single
// lease avoids recursive RWMutex reads and lets later human "always" answers
// update the remember table before the batch is pinned.
func (e *Engine) HoldResolutions(outcomes []Outcome) (release func(), err error) {
	e.postureMu.RLock()
	e.mu.RLock()
	defer e.mu.RUnlock()
	for _, out := range outcomes {
		if out.permissionRevision == 0 || e.revision != out.permissionRevision {
			e.postureMu.RUnlock()
			return nil, fmt.Errorf("permission mode changed after approval; submit the request again")
		}
	}
	return e.postureMu.RUnlock, nil
}

type reviewContextKey struct{}

func (e *Engine) reviewEligible(ctx context.Context, req Request) bool {
	if ctx.Value(reviewContextKey{}) != nil || req.Effect != EffectExecute {
		return false
	}
	if e.capability.Platform == "windows" {
		return false
	}
	// A command such as `go test`, `npm test`, or `make` can execute files the
	// primary model just wrote. The bounded review packet deliberately contains
	// no workspace contents, so a reviewer cannot safely approve that code with
	// full host reach. Keep unconfined execution with the human; auto review is
	// available only when a verified profile contains the interpreted files.
	if !e.sandboxActive(req) {
		return false
	}
	// Shell and inline interpreter programs are opaque policy payloads:
	// assignments, expansions, and code can hide credentials beyond argv-level
	// inspection. The model controls Shell, so a false value cannot make
	// `sh -c`, `python -c`, or an equivalent launcher reviewer-eligible.
	if opaqueInterpreterPayload(req) {
		return false
	}
	if sensitive, _ := SensitiveRequest(req); sensitive {
		return false
	}
	// Seatbelt shares the host's loopback namespace. A command can explicitly
	// address an already-running local forwarder even after proxy variables are
	// stripped, so an ordinary auto review is not enough; ask the human unless
	// the command explicitly requested full network reach.
	if e.hostLoopbackShared(req) {
		return false
	}
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.mode == ModeAuto && e.reviewer != nil
}

func (e *Engine) runReview(ctx context.Context, req Request, out Outcome) (ReviewResult, error) {
	e.mu.RLock()
	reviewer := e.reviewer
	e.mu.RUnlock()
	if reviewer == nil {
		return ReviewResult{}, fmt.Errorf("no command reviewer configured")
	}
	ctx = context.WithValue(ctx, reviewContextKey{}, true)
	return reviewer.Review(ctx, ReviewRequest{
		Tool: req.Tool, Effect: req.Effect, Path: req.Path,
		Argv: append([]string(nil), req.Argv...), Shell: req.Shell,
		Network: req.Network || out.FullReach, FullReach: out.FullReach,
		HostLoopbackShared: e.hostLoopbackShared(req),
		HostIPCShared:      e.hostIPCShared(req),
	})
}

// SensitiveRequest recognizes known credential formats without returning the
// matching bytes. It is intentionally narrow; callers with stronger domain
// knowledge set Request.Sensitive themselves.
func SensitiveRequest(req Request) (bool, string) {
	if req.Sensitive {
		return true, "marked sensitive by the tool"
	}
	values := append(append([]string(nil), req.Argv...), req.Path, req.Detail)
	for _, value := range values {
		if len(credential.ScanPrompt(value)) > 0 {
			return true, "contains credential-shaped data"
		}
	}
	opaque := opaqueInterpreterPayload(req)
	for i, arg := range req.Argv {
		lower := strings.ToLower(strings.TrimSpace(arg))
		if len(arg) > 2 && strings.HasPrefix(lower, "-u") && strings.Contains(arg[2:], ":") {
			return true, "contains attached user credentials"
		}
		if opaque && (credentialName(lower) || opaqueTextCredential(arg)) {
			return true, "inline code references credential-like data"
		}
		if credentialArgumentName(lower) {
			return true, "uses a credential-bearing command argument"
		}
		if key, value, ok := strings.Cut(lower, "="); ok && (credentialName(key) || credentialHeader(value)) {
			return true, "contains a credential-bearing assignment"
		}
		if argumentHasURLUserInfo(arg) {
			return true, "contains URL user information"
		}
		if credentialHeader(lower) {
			return true, "contains a credential-bearing request header"
		}
		if i > 0 && headerArgument(req.Argv[i-1]) && credentialHeader(lower) {
			return true, "contains a credential-bearing request header"
		}
	}
	// Find a known credential-consuming executable even behind wrappers such as
	// env, nice, time, or timeout. This is a single linear pass; once a command
	// executable is found, everything after it is that command's argv.
	if credentialAttachedInCommand(unwrapCommand(req.Argv)) {
		return true, "uses an attached credential-bearing command argument"
	}
	return false, ""
}

// opaqueInterpreterPayload reports executions whose argv embeds code that an
// interpreter will evaluate. The exec tool's Shell bit is model-controlled, so
// policy cannot trust it as the only indication that expansions, environment
// reads, redirects, or nested commands are hidden inside a single argument.
//
// This is deliberately conservative but bounded to well-known inline-code
// forms. Running a script file remains ordinary execution: the reviewer sees
// the exact file argument, while the filesystem and command policy still
// govern what that script can do.
func opaqueInterpreterPayload(req Request) bool {
	if req.Shell {
		return true
	}
	return inlineInterpreterAnywhere(unwrapCommand(req.Argv)) || inlineInterpreterAnywhere(req.Argv)
}

type interpreterMask uint16

const (
	interpreterShell interpreterMask = 1 << iota
	interpreterFish
	interpreterCmd
	interpreterPowerShell
	interpreterPython
	interpreterNode
	interpreterRuby
	interpreterPerl
	interpreterPHP
	interpreterLua
	interpreterDeno
)

// inlineInterpreterAnywhere is a linear conservative scan. A launcher does
// not need to appear in an allowlist: sudo/doas/xargs/custom wrappers cannot
// hide a later `sh -c` or `python -c` merely by occupying argv[0].
func inlineInterpreterAnywhere(argv []string) bool {
	var active interpreterMask
	for _, arg := range argv {
		if arg == "--" {
			active = 0
			continue
		}
		if inlineOption(active, arg) {
			return true
		}
		program := strings.TrimSuffix(strings.ToLower(filepath.Base(arg)), ".exe")
		active |= interpreterFor(program)
	}
	return false
}

func interpreterFor(program string) interpreterMask {
	switch {
	case isPOSIXShell(program):
		mask := interpreterShell
		if program == "fish" {
			mask |= interpreterFish
		}
		return mask
	case program == "cmd":
		return interpreterCmd
	case program == "powershell" || program == "pwsh":
		return interpreterPowerShell
	case strings.HasPrefix(program, "python"):
		return interpreterPython
	case program == "node" || program == "nodejs":
		return interpreterNode
	case program == "ruby":
		return interpreterRuby
	case program == "perl":
		return interpreterPerl
	case program == "php":
		return interpreterPHP
	case program == "lua" || strings.HasPrefix(program, "lua5.") || program == "rscript":
		return interpreterLua
	case program == "deno" || program == "bun":
		return interpreterDeno
	default:
		return 0
	}
}

func inlineOption(active interpreterMask, arg string) bool {
	one := []string{arg}
	return active&interpreterShell != 0 && hasShortOption(one, 'c') ||
		active&interpreterFish != 0 && hasAttachedOption(one, "--command") ||
		active&interpreterCmd != 0 && hasAttachedOption(one, "/c", "/k") ||
		active&interpreterPowerShell != 0 && hasAttachedOption(one, "-c", "-command", "-encodedcommand", "-enc") ||
		active&interpreterPython != 0 && hasShortOption(one, 'c') ||
		active&interpreterNode != 0 && (hasShortOption(one, 'e') || hasShortOption(one, 'p') || hasAttachedOption(one, "--eval", "--print")) ||
		active&interpreterRuby != 0 && hasShortOption(one, 'e') ||
		active&interpreterPerl != 0 && (hasShortOption(one, 'e') || hasShortOption(one, 'E')) ||
		active&interpreterPHP != 0 && hasShortOption(one, 'r') ||
		active&interpreterLua != 0 && hasShortOption(one, 'e') ||
		active&interpreterDeno != 0 && strings.EqualFold(arg, "eval")
}

func unwrapEnv(argv []string) []string {
	if len(argv) == 0 || strings.TrimSuffix(strings.ToLower(filepath.Base(argv[0])), ".exe") != "env" {
		return argv
	}
	for i := 1; i < len(argv); i++ {
		arg := argv[i]
		switch {
		case arg == "--":
			return argv[i+1:]
		case arg == "-S" || strings.HasPrefix(arg, "-S") && len(arg) > 2 ||
			arg == "--split-string" || strings.HasPrefix(arg, "--split-string="):
			// env performs another layer of tokenization; leave it human-only.
			return []string{"sh", "-c"}
		case arg == "-u" || arg == "--unset" || arg == "-C" || arg == "--chdir" || arg == "-a" || arg == "--argv0":
			i++
		case strings.HasPrefix(arg, "-u") && len(arg) > 2 || strings.HasPrefix(arg, "-C") && len(arg) > 2 || strings.HasPrefix(arg, "-a") && len(arg) > 2,
			strings.HasPrefix(arg, "--unset="), strings.HasPrefix(arg, "--chdir="), strings.HasPrefix(arg, "--argv0="):
			continue
		case arg == "-" || arg == "-i" || arg == "--ignore-environment" || arg == "-0" || arg == "--null" || arg == "-v" || arg == "--debug":
			continue
		case strings.HasPrefix(arg, "-"):
			return nil
		case strings.Contains(arg, "="):
			continue
		default:
			return argv[i:]
		}
	}
	return nil
}

// unwrapCommand peels launch-only wrappers until policy reaches the executable
// that will interpret the remaining argv. Each parser is deliberately
// conservative: an ambiguous form becomes an opaque shell marker, keeping
// auto with the human instead of trusting a model-controlled wrapper bit.
func unwrapCommand(argv []string) []string {
	for len(argv) > 0 {
		program := strings.TrimSuffix(strings.ToLower(filepath.Base(argv[0])), ".exe")
		var next []string
		switch program {
		case "env":
			next = unwrapEnv(argv)
		case "nice":
			next = unwrapNice(argv)
		case "time":
			next = unwrapTime(argv)
		case "timeout", "gtimeout":
			next = unwrapTimeout(argv)
		case "stdbuf":
			next = unwrapStdbuf(argv)
		case "nohup", "setsid":
			next = unwrapFlagWrapper(argv)
		default:
			return argv
		}
		if len(next) == 0 || len(next) == len(argv) {
			return []string{"sh", "-c"}
		}
		argv = next
	}
	if len(argv) == 0 {
		return []string{"sh", "-c"}
	}
	return argv
}

func unwrapNice(argv []string) []string {
	for i := 1; i < len(argv); i++ {
		arg := argv[i]
		switch {
		case arg == "--":
			return argv[i+1:]
		case arg == "-n" || arg == "--adjustment":
			i++
		case strings.HasPrefix(arg, "--adjustment=") || niceAdjustment(arg):
			continue
		case strings.HasPrefix(arg, "-"):
			return nil
		default:
			return argv[i:]
		}
	}
	return nil
}

func niceAdjustment(arg string) bool {
	if len(arg) < 2 || arg[0] != '-' {
		return false
	}
	for _, r := range arg[1:] {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

func unwrapTime(argv []string) []string {
	for i := 1; i < len(argv); i++ {
		arg := argv[i]
		switch {
		case arg == "--":
			return argv[i+1:]
		case arg == "-f" || arg == "--format" || arg == "-o" || arg == "--output":
			i++
		case strings.HasPrefix(arg, "--format=") || strings.HasPrefix(arg, "--output="):
			continue
		case arg == "-a" || arg == "--append" || arg == "-p" || arg == "--portability" || arg == "-v" || arg == "--verbose" || arg == "--quiet":
			continue
		case strings.HasPrefix(arg, "-"):
			return nil
		default:
			return argv[i:]
		}
	}
	return nil
}

func unwrapTimeout(argv []string) []string {
	i := 1
	for ; i < len(argv); i++ {
		arg := argv[i]
		switch {
		case arg == "--":
			i++
			goto duration
		case arg == "-k" || arg == "--kill-after" || arg == "-s" || arg == "--signal":
			i++
		case strings.HasPrefix(arg, "--kill-after=") || strings.HasPrefix(arg, "--signal="):
			continue
		case arg == "--foreground" || arg == "--preserve-status" || arg == "-v" || arg == "--verbose":
			continue
		case strings.HasPrefix(arg, "-"):
			return nil
		default:
			goto duration
		}
	}
duration:
	// timeout's first positional argument is the duration; the command follows.
	if i+1 >= len(argv) {
		return nil
	}
	return argv[i+1:]
}

func unwrapStdbuf(argv []string) []string {
	for i := 1; i < len(argv); i++ {
		arg := argv[i]
		switch {
		case arg == "--":
			return argv[i+1:]
		case arg == "-i" || arg == "-o" || arg == "-e" || arg == "--input" || arg == "--output" || arg == "--error":
			i++
		case (strings.HasPrefix(arg, "-i") || strings.HasPrefix(arg, "-o") || strings.HasPrefix(arg, "-e")) && len(arg) > 2,
			strings.HasPrefix(arg, "--input="), strings.HasPrefix(arg, "--output="), strings.HasPrefix(arg, "--error="):
			continue
		case strings.HasPrefix(arg, "-"):
			return nil
		default:
			return argv[i:]
		}
	}
	return nil
}

func unwrapFlagWrapper(argv []string) []string {
	for i := 1; i < len(argv); i++ {
		if argv[i] == "--" {
			return argv[i+1:]
		}
		if strings.HasPrefix(argv[i], "-") {
			continue
		}
		return argv[i:]
	}
	return nil
}

func isPOSIXShell(program string) bool {
	switch program {
	case "sh", "ash", "bash", "dash", "zsh", "ksh", "mksh", "fish":
		return true
	default:
		return false
	}
}

func hasAttachedOption(args []string, options ...string) bool {
	for _, arg := range args {
		if arg == "--" {
			return false
		}
		lower := strings.ToLower(arg)
		for _, option := range options {
			option = strings.ToLower(option)
			if lower == option || strings.HasPrefix(lower, option+"=") || strings.HasPrefix(lower, option+":") ||
				(len(option) == 2 && strings.HasPrefix(lower, option) && len(lower) > len(option)) {
				return true
			}
		}
	}
	return false
}

func hasShortOption(args []string, option byte) bool {
	for _, arg := range args {
		if arg == "--" {
			return false
		}
		if len(arg) >= 2 && arg[0] == '-' && arg[1] != '-' && strings.ContainsRune(arg[1:], rune(option)) {
			return true
		}
	}
	return false
}

func credentialArgumentName(arg string) bool {
	key := arg
	if before, _, ok := strings.Cut(arg, "="); ok {
		key = before
	}
	if strings.HasPrefix(key, "--") && credentialName(key) {
		return true
	}
	switch key {
	case "-u", "--user", "--password", "--passwd", "--token", "--secret",
		"--api-key", "--apikey", "--authorization", "--auth", "--proxy-auth", "-pass", "--passphrase", "--credential",
		"--credentials", "--client-secret", "--access-key", "--secret-key",
		"--secret-access-key", "--proxy-user", "--oauth2-bearer", "--cookie",
		"-d", "--data", "--data-raw", "--data-binary", "--data-ascii", "--data-urlencode", "--json",
		"-f", "--form", "--form-string":
		return true
	default:
		return false
	}
}

type credentialConsumerMask uint8

const (
	consumerCurl credentialConsumerMask = 1 << iota
	consumerPasswordP
	consumerRedis
	consumerHTTPie
	consumerContainer
)

// credentialAttachedInCommand keeps every plausible consumer active while it
// walks argv once. A wrapper option can legitimately be named "curl" before a
// later mysql executable, so stopping after the first candidate would let a
// decoy suppress detection of the real command's attached password.
func credentialAttachedInCommand(argv []string) bool {
	var active credentialConsumerMask
	containerLogin := false
	for _, arg := range argv {
		arg = strings.TrimSpace(arg)
		lower := strings.ToLower(arg)
		attached := func(option string) bool {
			return len(arg) > len(option) && strings.HasPrefix(lower, option)
		}
		if active&consumerCurl != 0 && curlSensitiveShortCluster(arg) ||
			active&consumerPasswordP != 0 && (lower == "-p" || attached("-p")) ||
			active&consumerRedis != 0 && (lower == "-a" || attached("-a")) ||
			active&consumerHTTPie != 0 && (lower == "-a" || attached("-a")) ||
			active&consumerContainer != 0 && containerLogin && (lower == "-p" || attached("-p")) {
			return true
		}
		if active&consumerContainer != 0 && strings.EqualFold(arg, "login") {
			containerLogin = true
		}
		program := strings.TrimSuffix(strings.ToLower(filepath.Base(arg)), ".exe")
		active |= credentialConsumerFor(program)
	}
	return false
}

func credentialConsumerFor(program string) credentialConsumerMask {
	switch program {
	case "curl":
		return consumerCurl
	case "mysql", "mysqldump", "mariadb", "mariadb-dump", "sshpass":
		return consumerPasswordP
	case "redis-cli":
		return consumerRedis
	case "http", "https", "httpie":
		return consumerHTTPie
	case "docker", "podman":
		return consumerContainer
	default:
		return 0
	}
}

func opaqueTextCredential(text string) bool {
	fields := strings.Fields(text)
	for i, field := range fields {
		field = strings.Trim(field, `"'`)
		lower := strings.ToLower(field)
		if credentialArgumentName(lower) || credentialHeader(lower) || argumentHasURLUserInfo(field) {
			return true
		}
		if key, value, ok := strings.Cut(lower, "="); ok && (credentialName(key) || credentialHeader(value)) {
			return true
		}
		if (lower == "-u" || lower == "-a") && i+1 < len(fields) && strings.Contains(strings.Trim(fields[i+1], `"'`), ":") {
			return true
		}
		if len(field) > 2 && (strings.HasPrefix(lower, "-u") || strings.HasPrefix(lower, "-a")) && strings.Contains(field[2:], ":") {
			return true
		}
		if len(field) > 2 && strings.HasPrefix(lower, "-p") && !allDecimal(field[2:]) {
			return true
		}
		if len(field) > 2 && strings.HasPrefix(lower, "-b") && (strings.Contains(field[2:], "=") || credentialName(field[2:])) {
			return true
		}
	}
	return false
}

func allDecimal(value string) bool {
	if value == "" {
		return false
	}
	for _, r := range value {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

func curlSensitiveShortCluster(arg string) bool {
	if len(arg) < 2 || arg[0] != '-' || arg[1] == '-' {
		return false
	}
	for i := 1; i < len(arg); i++ {
		switch arg[i] {
		case 'u', 'd', 'F', 'H', 'b':
			return true
		// These curl options consume the remainder of the same argv element.
		// A credential-looking letter after one belongs to that option's value,
		// not to the short-option cluster (for example -ooutput).
		case 'x':
			return urlHasUserInfo(arg[i+1:])
		case 'A', 'c', 'C', 'D', 'e', 'E', 'K', 'm', 'o', 'P', 'Q', 'r', 'T', 'U', 'w', 'X', 'Y', 'y', 'z':
			return false
		}
	}
	return false
}

func argumentHasURLUserInfo(arg string) bool {
	if urlHasUserInfo(arg) {
		return true
	}
	// Options and environment assignments commonly prefix a URL
	// (--proxy=URL, -xURL, HTTPS_PROXY=URL). Find each scheme-looking substring
	// without retaining or returning it; an ordinary URL with no userinfo stays
	// reviewer-eligible.
	for offset := 0; offset < len(arg); {
		rel := strings.Index(arg[offset:], "://")
		if rel < 0 {
			break
		}
		colon := offset + rel
		start := colon
		for start > 0 && urlSchemeByte(arg[start-1]) {
			start--
		}
		if start < colon && urlHasUserInfo(arg[start:]) {
			return true
		}
		offset = colon + 3
	}
	return false
}

func urlSchemeByte(b byte) bool {
	return b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z' ||
		b >= '0' && b <= '9' || b == '+' || b == '-' || b == '.'
}

func urlHasUserInfo(value string) bool {
	parsed, err := url.Parse(value)
	return err == nil && parsed.Scheme != "" && parsed.User != nil
}

func credentialName(name string) bool {
	name = strings.TrimLeft(strings.ToLower(strings.TrimSpace(name)), "-")
	name = strings.NewReplacer("-", "_", ".", "_").Replace(name)
	for _, marker := range []string{"password", "passwd", "_pwd", "_auth", "token", "secret", "api_key", "apikey", "authorization", "credential", "private_key", "access_key", "session_key", "session_id", "sessionid", "cookie"} {
		if strings.Contains(name, marker) {
			return true
		}
	}
	return false
}

func headerArgument(arg string) bool {
	switch strings.ToLower(strings.TrimSpace(arg)) {
	case "-h", "--header", "--proxy-header":
		return true
	default:
		return false
	}
}

func credentialHeader(value string) bool {
	name, _, ok := strings.Cut(value, ":")
	if !ok {
		return false
	}
	name = strings.ToLower(strings.TrimSpace(name))
	return name == "cookie" || name == "set-cookie" || credentialName(name)
}

func reviewerName(name string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		return "(unnamed)"
	}
	return cleanReviewText(name)
}

func cleanReviewText(text string) string {
	text = strings.TrimSpace(text)
	if text == "" {
		return "no reason supplied"
	}
	if leaks := credential.ScanPrompt(text); len(leaks) > 0 {
		text = credential.Redact(text, leaks)
	}
	text = terminaltext.Escape(text)
	const limit = 500
	if len(text) > limit {
		text = text[:limit] + "…"
	}
	return text
}

func (e *Engine) sandboxActive(req Request) bool {
	if req.Execution != nil {
		return req.Execution.SandboxActive
	}
	return e.execution != nil && e.execution.SandboxActive()
}

func (e *Engine) hostLoopbackShared(req Request) bool {
	if req.Execution != nil {
		return req.Execution.HostLoopbackShared
	}
	if e.execution == nil {
		return false
	}
	return e.execution.CommandPolicy(req.Network).HostLoopbackShared
}

func (e *Engine) hostIPCShared(req Request) bool {
	if req.Execution != nil {
		return req.Execution.HostIPCShared
	}
	if e.execution == nil {
		return false
	}
	return e.execution.CommandPolicy(req.Network).HostIPCShared
}

func (e *Engine) hostAuthorityShared(req Request) bool {
	return e.hostLoopbackShared(req) || e.hostIPCShared(req)
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
	if req.Execution != nil {
		b.WriteString("+reach:")
		switch {
		case req.Execution.FullAccess:
			b.WriteString("full-access")
		case req.Execution.SandboxActive:
			b.WriteString("sandbox/")
			b.WriteString(string(req.Execution.Network))
			if req.Execution.HostLoopbackShared {
				b.WriteString("/host-loopback")
			}
			if req.Execution.HostIPCShared {
				b.WriteString("/host-ipc")
			}
		default:
			b.WriteString("host-direct")
		}
	}
	for _, a := range req.Argv {
		b.WriteByte('\x00')
		b.WriteString(a)
	}
	return b.String()
}
