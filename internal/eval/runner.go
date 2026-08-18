package eval

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/switchboard-code/switchboard/internal/agent"
	"github.com/switchboard-code/switchboard/internal/breakpoint"
	"github.com/switchboard-code/switchboard/internal/cachestate"
	"github.com/switchboard-code/switchboard/internal/catalog"
	"github.com/switchboard-code/switchboard/internal/execution"
	"github.com/switchboard-code/switchboard/internal/permission"
	"github.com/switchboard-code/switchboard/internal/provider"
	"github.com/switchboard-code/switchboard/internal/session"
	"github.com/switchboard-code/switchboard/internal/tools"
)

// Arm is one thing being measured: a fixed target used as a baseline, or the
// router. §7.1 compares against the best fixed-target baseline, so the two have
// to stay distinguishable through the whole pipeline.
type Arm struct {
	Name     string
	Target   provider.RouteTarget
	Provider provider.Provider

	// Fallbacks are availability substitutes for the routed arm only, in user
	// policy order. Fixed baselines deliberately ignore them: a baseline must
	// remain the one pinned target it claims to measure.
	Fallbacks []Fallback

	// CacheAware places cache markers. Off is the control arm §7.1 compares
	// against when it asks whether the interval against an otherwise identical
	// cache-unaware router excludes zero: same model, same corpus, same tools,
	// and the one difference is whether §6 runs at all.
	CacheAware bool
}

// Fallback is one concrete target/provider binding inside a routed tier.
type Fallback struct {
	Target     provider.RouteTarget
	Provider   provider.Provider
	CacheAware bool
}

// Runner executes tasks.
//
// Each attempt gets its own copy of the repository, its own session, and its own
// sandbox capability check. Sharing any of those would let one attempt see
// another's edits, which turns a corpus into a single long conversation.
type Runner struct {
	Catalog *catalog.Catalog

	// MaxRounds bounds a single attempt. A task that has not converged in this
	// many tool rounds counts as unsolved rather than running forever, and the
	// bound is recorded so a corpus of timeouts is visible as one.
	MaxRounds int

	// Timeout bounds wall time per attempt for the same reason.
	Timeout time.Duration
}

const (
	DefaultMaxRounds = 40
	DefaultTimeout   = 10 * time.Minute
)

func (r Runner) rounds() int {
	if r.MaxRounds > 0 {
		return r.MaxRounds
	}
	return DefaultMaxRounds
}

func (r Runner) timeout() time.Duration {
	if r.Timeout > 0 {
		return r.Timeout
	}
	return DefaultTimeout
}

// Run attempts one task on one arm with one seed.
//
// It reports a Run whatever happens. A crashed attempt is an unsolved attempt
// with a reason, because dropping it would quietly improve the arm's solve rate.
func (r Runner) Run(ctx context.Context, task Task, arm Arm, seed int) Run {
	return r.run(ctx, task, arm, seed, nil)
}

// escalation is the hook a routed arm supplies so the primary can move
// mid-task. A fixed baseline passes nil and never moves, which is what makes it
// a baseline.
type escalation interface {
	attach(*agent.Loop)
	fidelityError() error
}

// preparedAttempt is the workspace-bound request assembly shared by opening
// routing and execution. Keeping one registry and one system prompt here is
// what proves the router scored the request the selected provider receives;
// rebuilding either side separately would let project instructions, tool
// schemas, or even the temporary workspace path drift.
type preparedAttempt struct {
	dir        string
	capability execution.Capability
	mode       permission.Mode
	registry   *tools.Registry
	system     []provider.Block
	opening    provider.Message
}

func prepareAttempt(task Task, dir string) (*preparedAttempt, error) {
	capability := execution.Detect()
	registry, err := tools.NewRegistry(dir, capability)
	if err != nil {
		return nil, err
	}
	mode := permission.ModeBypass
	return &preparedAttempt{
		dir:        dir,
		capability: capability,
		mode:       mode,
		registry:   registry,
		system:     agent.SystemPrompt(dir, mode, capability),
		opening:    provider.UserText(task.Prompt),
	}, nil
}

func (p *preparedAttempt) openingRequest() provider.Request {
	return provider.Request{
		System:   p.system,
		Tools:    p.registry.Definitions(),
		Messages: []provider.Message{p.opening},
	}
}

type armSelection struct {
	arm             Arm
	escalation      escalation
	estimatedCost   catalog.Money
	estimatedTarget provider.RouteTargetID
}

type selectArm func(context.Context, Task, *preparedAttempt) (armSelection, error)

func (r Runner) run(ctx context.Context, task Task, arm Arm, seed int, esc escalation) Run {
	return r.runSelected(ctx, task, arm.Name, arm.Target.ID(), seed,
		func(context.Context, Task, *preparedAttempt) (armSelection, error) {
			return armSelection{arm: arm, escalation: esc}, nil
		})
}

func (r Runner) runSelected(
	ctx context.Context,
	task Task,
	reportArm string,
	initialTarget provider.RouteTargetID,
	seed int,
	selectTarget selectArm,
) Run {
	out := Run{
		TaskID:     task.ID,
		Provenance: task.Provenance,
		Target:     initialTarget,
		Arm:        reportArm,
		Seed:       seed,
	}

	dir, err := os.MkdirTemp("", "sb-eval-")
	if err != nil {
		out.Detail = "could not create a workspace: " + err.Error()
		out.Failure = FailureWorkspace
		return out
	}
	defer os.RemoveAll(dir)

	if err := task.Setup(dir); err != nil {
		out.Detail = "setup failed: " + err.Error()
		out.Failure = FailureSetup
		return out
	}

	started := time.Now()
	attemptCtx, cancel := context.WithTimeout(ctx, r.timeout())
	defer cancel()

	prepared, err := prepareAttempt(task, dir)
	if err != nil {
		out.Duration = time.Since(started)
		out.Detail = "the turn failed: assembling the evaluation request: " + err.Error()
		out.Failure = FailureTurn
		return out
	}
	selection, err := selectTarget(attemptCtx, task, prepared)
	if err != nil {
		out.Duration = time.Since(started)
		out.Detail = "routed evaluation fidelity failed: " + err.Error()
		out.Failure = FailureFidelity
		return out
	}
	if selection.arm.Provider == nil {
		out.Duration = time.Since(started)
		if reportArm == RoutedArm {
			out.Detail = "routed evaluation fidelity failed: selected target has no provider"
			out.Failure = FailureFidelity
		} else {
			out.Detail = "the turn failed: fixed target has no provider"
			out.Failure = FailureTurn
		}
		return out
	}
	out.Target = selection.arm.Target.ID()
	out.EstimatedCost = selection.estimatedCost
	out.EstimatedTarget = selection.estimatedTarget

	perTarget, denials, runErr := r.attempt(attemptCtx, selection.arm, prepared, selection.escalation)
	out.Duration = time.Since(started)
	out.Denials = denials
	if routed, ok := selection.escalation.(*escalator); ok {
		out.Target = routed.finalTarget(selection.arm)
		out.Escalations = routed.moves
	}

	// Cost follows the tokens, not the arm. A routed run that escalates spends
	// on the target it moved to, and pricing the whole run against the rung it
	// started on reports an escalation to a paid target as free, which would let
	// any cost gate pass trivially and wrongly.
	for target, usage := range perTarget {
		out.Usage = out.Usage.Add(usage)
		if r.Catalog == nil {
			continue
		}
		info, _, ok := r.Catalog.Lookup(targetOf(r.Catalog, target))
		if !ok {
			continue
		}
		if cost, _, priced := info.Cost(usage); priced {
			out.Cost += cost
		}
	}
	if selection.escalation != nil {
		if fidelityErr := selection.escalation.fidelityError(); fidelityErr != nil {
			out.Detail = "routed evaluation fidelity failed: " + fidelityErr.Error()
			out.Failure = FailureFidelity
			out.Solved = false
			return out
		}
	}

	if runErr != nil {
		// A failed turn is still a data point. §8.4 treats provider failure as
		// something other than a bad routing decision, and the distinction is
		// only possible if the failure is recorded rather than discarded.
		out.Detail = "the turn failed: " + runErr.Error()
		switch {
		case errors.Is(runErr, context.DeadlineExceeded):
			out.Failure = FailureTimeout
		case errors.Is(runErr, context.Canceled):
			out.Failure = FailureCancelled
		case errors.Is(runErr, agent.ErrRoundLimit):
			out.Failure = FailureRoundLimit
		case errors.Is(runErr, errRoutedFidelity):
			out.Failure = FailureFidelity
		default:
			out.Failure = FailureTurn
		}
		return out
	}

	solved, detail, verifyErr := task.Verify(dir)
	if verifyErr != nil {
		out.Detail = "the verifier failed to run: " + verifyErr.Error()
		out.Failure = FailureVerifier
		return out
	}
	out.Solved = solved
	out.Detail = detail
	if !solved {
		out.Failure = FailureVerification
	}
	return out
}

// targetOf reconstructs a route target from its id, so usage recorded against
// a target the run moved to can still be priced.
func targetOf(cat *catalog.Catalog, id provider.RouteTargetID) provider.RouteTarget {
	target, err := provider.ParseRouteTargetID(id)
	if err != nil {
		return provider.RouteTarget{}
	}
	return target
}

func (r Runner) attempt(ctx context.Context, arm Arm, prepared *preparedAttempt, esc escalation) (map[provider.RouteTargetID]provider.Usage, int, error) {
	if prepared == nil || prepared.registry == nil {
		return nil, 0, fmt.Errorf("evaluation request was not assembled")
	}
	store, err := session.NewStore(prepared.dir + "/.sessions")
	if err != nil {
		return nil, 0, err
	}
	revision := ""
	if r.Catalog != nil {
		revision = r.Catalog.Revision
	}
	sess, err := store.Create(prepared.dir, arm.Target.ID(), revision)
	if err != nil {
		return nil, 0, err
	}
	defer sess.Close()

	// Bypass, because every task has to run the test suite and acceptEdits
	// deliberately does not cover running commands. This is the one place that
	// is the right mode: the sandbox still governs what a command can reach, the
	// workspace is a throwaway copy of the repository, and a harness that stops
	// to ask is not a harness.
	//
	// It is also a real dependency on §11. Where confinement is unverified the
	// engine downgrades bypass back to asking, and this will fail loudly rather
	// than run unprotected, which is the behaviour design principle 4 wants.
	asker := &denyingAsker{}
	collector := &usageCollector{byTarget: map[provider.RouteTargetID]provider.Usage{}}

	loop := &agent.Loop{
		Provider:      arm.Provider,
		Target:        arm.Target,
		Tools:         prepared.registry,
		Perms:         permission.NewEngine(prepared.mode, prepared.capability),
		Asker:         asker,
		Session:       sess,
		Observer:      collector,
		Catalog:       r.Catalog,
		System:        prepared.system,
		MaxToolRounds: r.rounds(),
	}

	collector.loop = loop
	if arm.CacheAware {
		if info, _, ok := r.Catalog.Lookup(arm.Target); ok {
			loop.Cache = &agent.Cache{
				Manager: &breakpoint.Manager{Policy: info.Cache, Target: arm.Target.ID()},
				Tracker: cachestate.New(),
				Policy:  info.Cache,
				Target:  arm.Target.ID(),
			}
		}
	}
	if esc != nil {
		esc.attach(loop)
	}

	err = loop.TurnMessage(ctx, prepared.opening)
	return collector.byTarget, asker.denied, err
}

// usageCollector attributes each turn's usage to the target that served it,
// which is the only way an escalating run can be priced correctly.
type usageCollector struct {
	agent.NopObserver
	loop     *agent.Loop
	byTarget map[provider.RouteTargetID]provider.Usage
}

func (c *usageCollector) TurnUsage(u session.Usage) {
	id := provider.RouteTargetID("unknown")
	if c.loop != nil {
		id = c.loop.Target.ID()
	}
	c.byTarget[id] = c.byTarget[id].Add(u.Usage)
}

// denyingAsker refuses a request and lets the turn continue, which is what an
// unattended session does. Erroring instead would end a run on its first
// network request, so a task the model could have finished another way would be
// recorded as unsolvable.
//
// The count matters: a corpus that trips approvals constantly is a corpus that
// needs looking at, and a silent denial hides that.
type denyingAsker struct{ denied int }

func (a *denyingAsker) Ask(_ context.Context, _ permission.Request, _ permission.Outcome) (permission.Response, error) {
	a.denied++
	return permission.Response{Approved: false}, nil
}
