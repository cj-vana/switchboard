package eval

import (
	"context"

	"github.com/cjvana/switchboard/internal/catalog"
	"github.com/cjvana/switchboard/internal/costmodel"
	"github.com/cjvana/switchboard/internal/provider"
	"github.com/cjvana/switchboard/internal/router"
)

// RoutedArmFor builds the arm under test: the same targets as the baselines,
// with the router choosing between them.
//
// The comparison §7.1 asks for only means something if the arms differ in the
// routing and nothing else. Same corpus, same tools, same sandbox, same
// verifier; the one variable is who picks the target.
type RoutedArmFor struct {
	Catalog *catalog.Catalog
	Router  router.Heuristic

	// Ladder is the ordered set of targets the router may choose from, lowest
	// first. Order is the user's policy ladder, not a capability claim (§3.1).
	Ladder []Arm
}

// Pick chooses a target for a task and reports why.
//
// The router sees the prompt and nothing that would leak the answer. It gets no
// task id, no provenance, and no knowledge of which package is broken: a router
// told which task it was solving would be measured on a job nobody has.
func (r RoutedArmFor) Pick(task Task) (Arm, router.Decision, error) {
	candidates := make([]router.Candidate, 0, len(r.Ladder))
	for rank, arm := range r.Ladder {
		info, _, ok := r.Catalog.Lookup(arm.Target)
		if !ok {
			info = catalog.ModelInfo{}
		}
		c := router.Candidate{
			Tier:   arm.Name,
			Target: arm.Target,
			Info:   info,
			Rank:   rank,
			// The corpus prompt is short; what the turn actually reads is the
			// repository, which no arm knows the size of before it starts.
			PromptTokens: len(task.Prompt) / 4,
		}
		c.Estimate = costmodel.Estimator{}.Turn(costmodel.Inputs{
			Target: arm.Target, Info: info,
			PrefixTokens: c.PromptTokens, OutputTokens: 2048,
			Eligible:       info.Cache.UsageAccounting == catalog.AccountingSeparate,
			HitProbability: 0,
		})
		candidates = append(candidates, c)
	}

	decision, err := r.Router.Route(router.Input{
		Prompt:       task.Prompt,
		Candidates:   candidates,
		Requirements: router.Requirements{NeedsTools: true},
	})
	if err != nil {
		return Arm{}, decision, err
	}

	for _, arm := range r.Ladder {
		if arm.Name == decision.Tier {
			// Renamed so the report can tell the routed arm apart from the
			// baseline that happens to use the same target.
			arm.Name = RoutedArm
			return arm, decision, nil
		}
	}
	return Arm{}, decision, context.Canceled
}

// RunRouted attempts a task with the router choosing the target.
func (r RoutedArmFor) Run(ctx context.Context, runner Runner, task Task, seed int) Run {
	arm, decision, err := r.Pick(task)
	if err != nil {
		return Run{
			TaskID: task.ID, Provenance: task.Provenance, Arm: RoutedArm, Seed: seed,
			Detail: "routing failed: " + err.Error(),
		}
	}

	out := runner.Run(ctx, task, arm, seed)
	out.Arm = RoutedArm
	out.Target = arm.Target.ID()

	// §7.1 requires estimate against actual per target, and the estimate has to
	// be the one that was actually used to decide rather than one computed
	// afterwards knowing what happened.
	out.EstimatedCost = decision.EstimatedCost.Expected
	return out
}

// TargetsUsed reports which targets the router actually chose, which is the
// first thing to check when a routed arm matches a baseline exactly: a router
// that always picks the same rung is a baseline wearing a different name.
func TargetsUsed(runs []Run) map[provider.RouteTargetID]int {
	out := map[provider.RouteTargetID]int{}
	for _, r := range runs {
		if r.Arm == RoutedArm {
			out[r.Target]++
		}
	}
	return out
}
