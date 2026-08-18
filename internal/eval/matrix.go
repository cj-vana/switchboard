package eval

import (
	"fmt"
	"sort"
	"strings"

	"github.com/switchboard-code/switchboard/internal/provider"
)

type matrixCell struct {
	task string
	arm  string
	seed int
}

type matrixEvidence struct {
	Runs       []Run
	Arms       []string
	ArmTargets map[string][]provider.RouteTargetID
	Targets    []provider.RouteTargetID
	Replicates []int
	Reasons    []string
}

// validateMatrix establishes the grain before any rates are computed. Every
// tier-1 task must have exactly one run for every arm and replicate. A partial
// or repeated journal is useful recovery evidence, but it is not a verdict.
func (g Gate) validateMatrix(tasks []Task, runs []Run) matrixEvidence {
	out := matrixEvidence{ArmTargets: map[string][]provider.RouteTargetID{}}
	taskSet := map[string]bool{}
	for _, task := range tasks {
		if task.Provenance != HandWritten {
			continue
		}
		if task.ID == "" {
			out.Reasons = append(out.Reasons, "the tier-1 corpus contains a task with no id")
			continue
		}
		if taskSet[task.ID] {
			out.Reasons = append(out.Reasons, fmt.Sprintf("the tier-1 corpus contains duplicate task id %q", task.ID))
		}
		taskSet[task.ID] = true
	}

	expectedArms := stringSet(g.ExpectedArms)
	if len(g.ExpectedArms) > 0 && len(expectedArms) != len(g.ExpectedArms) {
		out.Reasons = append(out.Reasons, "the configured arm list contains an empty or duplicate name")
	}
	configuredTargets := map[provider.RouteTargetID]bool{}
	for _, target := range g.ExpectedTargets {
		if target == "" {
			out.Reasons = append(out.Reasons, "the configured target list contains an empty target")
			continue
		}
		// Multiple arms may intentionally share a target (for example, the
		// cache-aware and cache-unaware controls), so this is a set rather than
		// a uniqueness constraint.
		configuredTargets[target] = true
	}

	var candidates []Run
	observedArms := map[string]bool{}
	for _, run := range runs {
		if run.Provenance != HandWritten {
			continue
		}
		if !taskSet[run.TaskID] {
			out.Reasons = append(out.Reasons, fmt.Sprintf(
				"run for unknown tier-1 task %q is not part of this corpus", run.TaskID))
			continue
		}
		if run.Arm == "" {
			out.Reasons = append(out.Reasons, fmt.Sprintf(
				"task %s replicate %d has no arm", run.TaskID, run.Seed))
			continue
		}
		observedArms[run.Arm] = true
		candidates = append(candidates, run)
	}
	if len(expectedArms) == 0 {
		expectedArms = observedArms
	}

	replicates := map[int]bool{}
	targets := map[provider.RouteTargetID]bool{}
	armTargets := map[string]map[provider.RouteTargetID]bool{}
	cells := map[matrixCell][]Run{}
	for _, run := range candidates {
		if !expectedArms[run.Arm] {
			out.Reasons = append(out.Reasons, fmt.Sprintf(
				"run for unexpected arm %q is not part of this configuration", run.Arm))
			continue
		}
		out.Runs = append(out.Runs, run)
		replicates[run.Seed] = true
		if run.Target != "" {
			targets[run.Target] = true
			if run.Arm != RoutedArm {
				if armTargets[run.Arm] == nil {
					armTargets[run.Arm] = map[provider.RouteTargetID]bool{}
				}
				armTargets[run.Arm][run.Target] = true
			}
		}
		key := matrixCell{task: run.TaskID, arm: run.Arm, seed: run.Seed}
		cells[key] = append(cells[key], run)
	}

	out.Arms = sortedStrings(expectedArms)
	if len(replicates) < g.minRuns() {
		out.Reasons = append(out.Reasons, fmt.Sprintf(
			"the evidence has %d replicate(s) and a median needs at least %d",
			len(replicates), g.minRuns()))
	}

	cellKeys := make([]matrixCell, 0, len(cells))
	for key := range cells {
		cellKeys = append(cellKeys, key)
	}
	sort.Slice(cellKeys, func(i, j int) bool {
		if cellKeys[i].task != cellKeys[j].task {
			return cellKeys[i].task < cellKeys[j].task
		}
		if cellKeys[i].arm != cellKeys[j].arm {
			return cellKeys[i].arm < cellKeys[j].arm
		}
		return cellKeys[i].seed < cellKeys[j].seed
	})
	for _, key := range cellKeys {
		group := cells[key]
		for _, run := range group {
			if !modelQualityOutcome(run) {
				out.Reasons = append(out.Reasons, fmt.Sprintf(
					"infrastructure-contaminated cell: task %s, arm %s, replicate %d has failure kind %q",
					key.task, key.arm, key.seed, run.FailureKind()))
			}
		}
		if len(group) < 2 {
			continue
		}
		kind := "duplicate"
		for _, run := range group[1:] {
			if !sameResult(group[0], run) {
				kind = "conflicting duplicate"
				break
			}
		}
		out.Reasons = append(out.Reasons, fmt.Sprintf(
			"%s cell for task %s, arm %s, replicate %d",
			kind, key.task, key.arm, key.seed))
	}

	taskIDs := sortedStrings(taskSet)
	replicateIDs := sortedInts(replicates)
	out.Replicates = replicateIDs
	var missing []matrixCell
	for _, task := range taskIDs {
		for _, arm := range out.Arms {
			for _, seed := range replicateIDs {
				key := matrixCell{task: task, arm: arm, seed: seed}
				if len(cells[key]) == 0 {
					missing = append(missing, key)
				}
			}
		}
	}
	if len(missing) > 0 {
		first := missing[0]
		out.Reasons = append(out.Reasons, fmt.Sprintf(
			"the evidence matrix is incomplete: missing %d cell(s), including task %s, arm %s, replicate %d",
			len(missing), first.task, first.arm, first.seed))
	}

	if len(configuredTargets) > 0 {
		for _, target := range sortedTargets(targets) {
			if !configuredTargets[target] {
				out.Reasons = append(out.Reasons, fmt.Sprintf(
					"observed target %q is not part of this configuration", target))
			}
		}
		for target := range configuredTargets {
			targets[target] = true
		}
	}
	if len(targets) == 0 {
		out.Reasons = append(out.Reasons,
			"the evidence and configuration name no model targets to snapshot")
	}
	for arm, observed := range armTargets {
		out.ArmTargets[arm] = sortedTargets(observed)
	}
	out.Targets = sortedTargets(targets)
	return out
}

func sameResult(a, b Run) bool {
	return a.Solved == b.Solved &&
		a.FailureKind() == b.FailureKind() &&
		a.Target == b.Target
}

func stringSet(values []string) map[string]bool {
	out := map[string]bool{}
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" {
			out[value] = true
		}
	}
	return out
}

func sortedStrings(values map[string]bool) []string {
	out := make([]string, 0, len(values))
	for value := range values {
		out = append(out, value)
	}
	sort.Strings(out)
	return out
}

func sortedInts(values map[int]bool) []int {
	out := make([]int, 0, len(values))
	for value := range values {
		out = append(out, value)
	}
	sort.Ints(out)
	return out
}

func sortedTargets(values map[provider.RouteTargetID]bool) []provider.RouteTargetID {
	out := make([]provider.RouteTargetID, 0, len(values))
	for value := range values {
		out = append(out, value)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}
