package eval

import (
	"context"
	"os"
	"strings"
	"time"

	"github.com/cjvana/switchboard/internal/agent"
	"github.com/cjvana/switchboard/internal/breakpoint"
	"github.com/cjvana/switchboard/internal/cachestate"
	"github.com/cjvana/switchboard/internal/catalog"
	"github.com/cjvana/switchboard/internal/execution"
	"github.com/cjvana/switchboard/internal/permission"
	"github.com/cjvana/switchboard/internal/provider"
	"github.com/cjvana/switchboard/internal/session"
	"github.com/cjvana/switchboard/internal/tools"
)

// Arm is one thing being measured: a fixed target used as a baseline, or the
// router. §7.1 compares against the best fixed-target baseline, so the two have
// to stay distinguishable through the whole pipeline.
type Arm struct {
	Name     string
	Target   provider.RouteTarget
	Provider provider.Provider

	// CacheAware places cache markers. Off is the control arm §7.1 compares
	// against when it asks whether the interval against an otherwise identical
	// cache-unaware router excludes zero: same model, same corpus, same tools,
	// and the one difference is whether §6 runs at all.
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
}

func (r Runner) run(ctx context.Context, task Task, arm Arm, seed int, esc escalation) Run {
	out := Run{
		TaskID:     task.ID,
		Provenance: task.Provenance,
		Target:     arm.Target.ID(),
		Arm:        arm.Name,
		Seed:       seed,
	}

	dir, err := os.MkdirTemp("", "sb-eval-")
	if err != nil {
		out.Detail = "could not create a workspace: " + err.Error()
		return out
	}
	defer os.RemoveAll(dir)

	if err := task.Setup(dir); err != nil {
		out.Detail = "setup failed: " + err.Error()
		return out
	}

	started := time.Now()
	perTarget, denials, runErr := r.attempt(ctx, task, arm, dir, esc)
	out.Duration = time.Since(started)
	out.Denials = denials

	// Cost follows the tokens, not the arm. A routed run that escalates spends
	// on the target it moved to, and pricing the whole run against the rung it
	// started on reports an escalation to a paid target as free, which would let
	// any cost gate pass trivially and wrongly.
	for target, usage := range perTarget {
		out.Usage = out.Usage.Add(usage)
		info, _, ok := r.Catalog.Lookup(targetOf(r.Catalog, target))
		if !ok {
			continue
		}
		if cost, _, priced := info.Cost(usage); priced {
			out.Cost += cost
		}
	}

	if runErr != nil {
		// A failed turn is still a data point. §8.4 treats provider failure as
		// something other than a bad routing decision, and the distinction is
		// only possible if the failure is recorded rather than discarded.
		out.Detail = "the turn failed: " + runErr.Error()
		return out
	}

	solved, detail, verifyErr := task.Verify(dir)
	if verifyErr != nil {
		out.Detail = "the verifier failed to run: " + verifyErr.Error()
		return out
	}
	out.Solved = solved
	out.Detail = detail
	return out
}

// targetOf reconstructs a route target from its id, so usage recorded against
// a target the run moved to can still be priced.
func targetOf(cat *catalog.Catalog, id provider.RouteTargetID) provider.RouteTarget {
	parts := strings.SplitN(string(id), "/", 3)
	if len(parts) < 3 {
		return provider.RouteTarget{}
	}
	model, _, _ := strings.Cut(parts[2], "+")
	return provider.RouteTarget{Provider: parts[0], Surface: parts[1], ModelID: model}
}

func (r Runner) attempt(ctx context.Context, task Task, arm Arm, dir string, esc escalation) (map[provider.RouteTargetID]provider.Usage, int, error) {
	ctx, cancel := context.WithTimeout(ctx, r.timeout())
	defer cancel()

	store, err := session.NewStore(dir + "/.sessions")
	if err != nil {
		return nil, 0, err
	}
	sess, err := store.Create(dir, arm.Target.ID(), r.Catalog.Revision)
	if err != nil {
		return nil, 0, err
	}
	defer sess.Close()

	capability := execution.Detect()
	registry, err := tools.NewRegistry(dir, capability)
	if err != nil {
		return nil, 0, err
	}

	// Bypass, because every task has to run the test suite and acceptEdits
	// deliberately does not cover running commands. This is the one place that
	// is the right mode: the sandbox still governs what a command can reach, the
	// workspace is a throwaway copy of the repository, and a harness that stops
	// to ask is not a harness.
	//
	// It is also a real dependency on §11. Where confinement is unverified the
	// engine downgrades bypass back to asking, and this will fail loudly rather
	// than run unprotected, which is the behaviour design principle 4 wants.
	mode := permission.ModeBypass
	asker := &denyingAsker{}
	collector := &usageCollector{byTarget: map[provider.RouteTargetID]provider.Usage{}}

	loop := &agent.Loop{
		Provider:      arm.Provider,
		Target:        arm.Target,
		Tools:         registry,
		Perms:         permission.NewEngine(mode, capability),
		Asker:         asker,
		Session:       sess,
		Observer:      collector,
		Catalog:       r.Catalog,
		System:        agent.SystemPrompt(dir, mode, capability),
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

	err = loop.Turn(ctx, task.Prompt)
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
