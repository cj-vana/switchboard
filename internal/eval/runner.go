package eval

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/cjvana/switchboard/internal/agent"
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
	usage, rounds, runErr := r.attempt(ctx, task, arm, dir, esc)
	out.Duration = time.Since(started)
	out.Usage = usage
	_ = rounds

	if info, _, ok := r.Catalog.Lookup(arm.Target); ok {
		if cost, _, priced := info.Cost(usage); priced {
			out.Cost = cost
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

func (r Runner) attempt(ctx context.Context, task Task, arm Arm, dir string, esc escalation) (provider.Usage, int, error) {
	ctx, cancel := context.WithTimeout(ctx, r.timeout())
	defer cancel()

	store, err := session.NewStore(dir + "/.sessions")
	if err != nil {
		return provider.Usage{}, 0, err
	}
	sess, err := store.Create(dir, arm.Target.ID(), r.Catalog.Revision)
	if err != nil {
		return provider.Usage{}, 0, err
	}
	defer sess.Close()

	capability := execution.Detect()
	registry, err := tools.NewRegistry(dir, capability)
	if err != nil {
		return provider.Usage{}, 0, err
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
	collector := &usageCollector{}

	loop := &agent.Loop{
		Provider:      arm.Provider,
		Target:        arm.Target,
		Tools:         registry,
		Perms:         permission.NewEngine(mode, capability),
		Asker:         refusingAsker{},
		Session:       sess,
		Observer:      collector,
		Catalog:       r.Catalog,
		System:        agent.SystemPrompt(dir, mode, capability),
		MaxToolRounds: r.rounds(),
	}

	if esc != nil {
		esc.attach(loop)
	}

	err = loop.Turn(ctx, task.Prompt)
	return collector.total, collector.turns, err
}

type usageCollector struct {
	agent.NopObserver
	total provider.Usage
	turns int
}

func (c *usageCollector) TurnUsage(u session.Usage) {
	c.total = c.total.Add(u.Usage)
	c.turns++
}

// refusingAsker fails rather than blocking. Anything reaching it is a request
// the corpus should not have produced, and a harness that silently approved it
// would be measuring a different run than the one it reports.
type refusingAsker struct{}

func (refusingAsker) Ask(_ context.Context, req permission.Request, outcome permission.Outcome) (permission.Response, error) {
	// Naming the request and the reason turns an unsolved run into a fixable
	// one. "The corpus should not need an approval prompt" says nothing about
	// which prompt, and a corpus that trips one is a corpus bug.
	return permission.Response{}, fmt.Errorf(
		"stopped for approval: %s wanted %s (%s)", req.Tool, req.Effect, outcome.Reason)
}
