package eval

import (
	"strings"
	"testing"
	"time"

	"github.com/switchboard-code/switchboard/internal/catalog"
	"github.com/switchboard-code/switchboard/internal/provider"
)

func pins() Pins {
	return Pins{
		HarnessCommit:   "abc123",
		CatalogRevision: "2026-08-13+deadbeef",
		PromptVersion:   "v1",
		Snapshots:       map[provider.RouteTargetID]string{"t/s/m": "m-20260101"},
	}
}

func corpus(n int, p Provenance) []Task {
	out := make([]Task, 0, n)
	for i := range n {
		out = append(out, Task{ID: string(rune('a'+i%26)) + string(rune('0'+i/26)), Provenance: p})
	}
	return out
}

func runs(arm string, n, seeds int, solved bool, cost catalog.Money) []Run {
	var out []Run
	for i := range n {
		for s := range seeds {
			run := Run{
				TaskID:     string(rune('a'+i%26)) + string(rune('0'+i/26)),
				Provenance: HandWritten, Arm: arm, Target: "t/s/m",
				Solved: solved, Cost: cost, Seed: s, Duration: time.Second,
			}
			if !solved {
				run.Failure = FailureVerification
			}
			out = append(out, run)
		}
	}
	return out
}

// §8.6's central rule, made mechanical: the harness does not produce a verdict
// before the corpus is populated, because confident numbers about an empty
// corpus are worse than none and they get quoted.
func TestAnUnderpopulatedCorpusIsRefusedNotFailed(t *testing.T) {
	tasks := corpus(4, HandWritten)
	all := append(runs(RoutedArm, 4, 3, true, 100), runs("always-highest", 4, 3, true, 500)...)

	v := Gate{}.Evaluate(tasks, all, pins())

	if !v.Refused {
		t.Fatal("a four-task corpus produced a verdict")
	}
	if v.Passed {
		t.Error("a refused gate reported as passed")
	}
	joined := strings.Join(v.Reasons, "\n")
	if !strings.Contains(joined, "20") {
		t.Errorf("the refusal does not name the floor:\n%s", joined)
	}
	// Refused and failed are different states, and collapsing them is how an
	// unmeasured gate gets reported green.
	if !strings.Contains(joined, "not the same as") {
		t.Errorf("the refusal does not distinguish itself from a failure:\n%s", joined)
	}
}

// A report without its pins describes a measurement nobody can repeat.
func TestMissingPinsRefuseTheVerdict(t *testing.T) {
	tasks := corpus(25, HandWritten)
	all := append(runs(RoutedArm, 25, 3, true, 100), runs("always-highest", 25, 3, true, 500)...)

	v := Gate{}.Evaluate(tasks, all, Pins{HarnessCommit: "abc"})
	if !v.Refused {
		t.Fatal("a report with no catalog revision or snapshots produced a verdict")
	}
	if !strings.Contains(strings.Join(v.Reasons, " "), "reproducible") {
		t.Errorf("reasons = %v", v.Reasons)
	}
}

// One seed per task is not a median.
func TestASingleSeedIsRefused(t *testing.T) {
	tasks := corpus(25, HandWritten)
	all := append(runs(RoutedArm, 25, 1, true, 100), runs("always-highest", 25, 1, true, 500)...)

	v := Gate{}.Evaluate(tasks, all, pins())
	if !v.Refused {
		t.Fatal("a single seed per task produced a verdict")
	}
}

func TestADuplicateMatrixCellIsRefused(t *testing.T) {
	tasks := corpus(25, HandWritten)
	all := append(runs(RoutedArm, 25, 3, true, 60), runs("always-highest", 25, 3, true, 100)...)
	all = append(all, all[0])

	v := Gate{}.Evaluate(tasks, all, pins())
	if !v.Refused {
		t.Fatal("a journal with a duplicate task-arm-replicate cell produced a verdict")
	}
	if !strings.Contains(strings.Join(v.Reasons, " "), "duplicate cell") {
		t.Errorf("reasons = %v", v.Reasons)
	}
}

func TestAConflictingDuplicateMatrixCellIsRefused(t *testing.T) {
	tasks := corpus(25, HandWritten)
	all := append(runs(RoutedArm, 25, 3, true, 60), runs("always-highest", 25, 3, true, 100)...)
	conflict := all[0]
	conflict.Solved = false
	conflict.Failure = FailureVerification
	all = append(all, conflict)

	v := Gate{}.Evaluate(tasks, all, pins())
	if !v.Refused {
		t.Fatal("a journal with conflicting results for one cell produced a verdict")
	}
	if !strings.Contains(strings.Join(v.Reasons, " "), "conflicting duplicate") {
		t.Errorf("reasons = %v", v.Reasons)
	}
}

func TestAMissingMatrixCellIsRefused(t *testing.T) {
	tasks := corpus(25, HandWritten)
	all := append(runs(RoutedArm, 25, 3, true, 60), runs("always-highest", 25, 3, true, 100)...)
	all = all[:len(all)-1]

	v := Gate{}.Evaluate(tasks, all, pins())
	if !v.Refused {
		t.Fatal("an incomplete task-arm-replicate matrix produced a verdict")
	}
	if !strings.Contains(strings.Join(v.Reasons, " "), "matrix is incomplete") {
		t.Errorf("reasons = %v", v.Reasons)
	}
}

func TestAConfiguredArmWithNoRunsIsRefused(t *testing.T) {
	tasks := corpus(25, HandWritten)
	all := append(runs(RoutedArm, 25, 3, true, 60), runs("always-highest", 25, 3, true, 100)...)

	v := (Gate{ExpectedArms: []string{RoutedArm, "always-highest", "always-lowest"}}).
		Evaluate(tasks, all, pins())
	if !v.Refused {
		t.Fatal("a configured arm with no cells produced a verdict")
	}
	if !strings.Contains(strings.Join(v.Reasons, " "), "always-lowest") {
		t.Errorf("reasons = %v", v.Reasons)
	}
}

func TestEveryConfiguredTargetNeedsASnapshot(t *testing.T) {
	tasks := corpus(25, HandWritten)
	all := append(runs(RoutedArm, 25, 3, true, 60), runs("always-highest", 25, 3, true, 100)...)

	v := (Gate{ExpectedTargets: []provider.RouteTargetID{"t/s/m", "t/s/n"}}).
		Evaluate(tasks, all, pins())
	if !v.Refused {
		t.Fatal("a target with no resolved snapshot produced a verdict")
	}
	if !strings.Contains(strings.Join(v.Reasons, " "), "model snapshot for t/s/n") {
		t.Errorf("reasons = %v", v.Reasons)
	}
}

func TestEveryObservedTargetNeedsASnapshotEvenWithConfiguredTargets(t *testing.T) {
	tasks := corpus(25, HandWritten)
	all := runs(RoutedArm, 25, 3, true, 60)
	baseline := runs("always-highest", 25, 3, true, 100)
	for i := range baseline {
		baseline[i].Target = "t/s/unexpected"
	}
	all = append(all, baseline...)

	v := (Gate{ExpectedTargets: []provider.RouteTargetID{"t/s/m"}}).
		Evaluate(tasks, all, pins())
	if !v.Refused {
		t.Fatal("an observed target outside the configured list produced a verdict without a snapshot")
	}
	if !strings.Contains(strings.Join(v.Reasons, " "), "model snapshot for t/s/unexpected") {
		t.Errorf("reasons = %v", v.Reasons)
	}
}

// Only tier 1 decides the gate: tier 2 is contaminated by training cutoffs and
// tier 3 measures the harness.
func TestOnlyHandWrittenTasksCountTowardTheGate(t *testing.T) {
	tasks := append(corpus(25, HandWritten), corpus(50, FromPullRequest)...)

	all := append(runs(RoutedArm, 25, 3, true, 100), runs("always-highest", 25, 3, true, 500)...)
	// A pile of cheap synthetic wins that must not move the verdict.
	for i := range 50 {
		all = append(all, Run{
			TaskID: "syn", Provenance: Synthetic, Arm: RoutedArm,
			Solved: true, Cost: 1, Seed: i,
		})
	}

	v := Gate{}.Evaluate(tasks, all, pins())
	if v.Refused {
		t.Fatalf("refused: %v", v.Reasons)
	}
	if v.Routed.MedianCostPerSolved != 100 {
		t.Errorf("median = %s; synthetic runs leaked into the gate", v.Routed.MedianCostPerSolved)
	}
}

// The comparison is against the *best* baseline. Beating the worst alternative
// is not an improvement.
func TestTheBaselineIsTheBestFixedTarget(t *testing.T) {
	tasks := corpus(25, HandWritten)
	all := runs(RoutedArm, 25, 3, true, 100)
	all = append(all, runs("always-lowest", 25, 3, true, 120)...)
	all = append(all, runs("always-highest", 25, 3, true, 900)...)

	v := Gate{}.Evaluate(tasks, all, pins())
	if v.Baseline.Arm != "always-lowest" {
		t.Errorf("baseline = %q, want the cheapest one that solved", v.Baseline.Arm)
	}
	// 100 against 120 is under 20 percent, so this must fail rather than pass
	// by comparing against the expensive arm.
	if v.Passed {
		t.Errorf("passed against the wrong baseline: %.1f%% reduction", v.CostReduction*100)
	}
}

func TestAClearWinPasses(t *testing.T) {
	tasks := corpus(25, HandWritten)
	all := append(runs(RoutedArm, 25, 3, true, 60), runs("always-highest", 25, 3, true, 100)...)

	v := Gate{}.Evaluate(tasks, all, pins())
	if v.Refused {
		t.Fatalf("refused: %v", v.Reasons)
	}
	if !v.Passed {
		t.Errorf("a 40%% saving with no solve-rate drop failed: %v", v.Reasons)
	}
	if v.CostReduction < 0.39 || v.CostReduction > 0.41 {
		t.Errorf("cost reduction = %.3f, want about 0.40", v.CostReduction)
	}
}

// A saving bought by solving less is not a saving.
func TestASolveRateDropFailsEvenWithASaving(t *testing.T) {
	tasks := corpus(25, HandWritten)

	// The router is far cheaper and solves noticeably less often.
	var all []Run
	all = append(all, runs("always-highest", 25, 3, true, 100)...)
	for i, r := range runs(RoutedArm, 25, 3, true, 10) {
		if i%4 == 0 {
			r.Solved = false
			r.Failure = FailureVerification
		}
		all = append(all, r)
	}

	v := Gate{}.Evaluate(tasks, all, pins())
	if v.Refused {
		t.Fatalf("refused: %v", v.Reasons)
	}
	if v.Passed {
		t.Error("a saving bought by solving fewer tasks passed the gate")
	}
	if !strings.Contains(strings.Join(v.Reasons, " "), "solve rate fell") {
		t.Errorf("reasons = %v", v.Reasons)
	}
}

func TestCompleteIdentityBoundMatrixRefusesInfrastructureAndExcludesItFromQualityRates(t *testing.T) {
	tasks := corpus(MinimumTier1Tasks, HandWritten)
	all := append(runs(RoutedArm, len(tasks), 3, true, 60),
		runs("always-highest", len(tasks), 3, true, 100)...)

	// The matrix is otherwise complete. This one cell represents provider or
	// harness reliability, not a model-quality loss, and must neither disappear
	// nor lower the routed solve rate.
	all[0].Solved = false
	all[0].Failure = FailureTimeout
	all[0].Detail = "the turn failed: context deadline exceeded"

	gate := Gate{
		ExpectedArms:        []string{RoutedArm, "always-highest"},
		ExpectedTargets:     []provider.RouteTargetID{"t/s/m"},
		RequireEvaluationID: true,
		EvaluationWorkers:   4,
	}
	id := evaluationID(
		tasks,
		gate.ExpectedArms,
		map[string][]provider.RouteTargetID{"always-highest": {"t/s/m"}},
		gate.ExpectedTargets,
		[]int{0, 1, 2},
		gate.EvaluationWorkers,
		pins(),
	)
	for i := range all {
		all[i].EvaluationID = id
	}

	v := gate.Evaluate(tasks, all, pins())
	if !v.Refused || v.Passed {
		t.Fatalf("infrastructure-contaminated matrix produced a verdict: %#v", v)
	}
	reasons := strings.Join(v.Reasons, "\n")
	for _, want := range []string{
		"infrastructure-contaminated cell",
		"task a0, arm routed, replicate 0",
		`failure kind "timeout"`,
	} {
		if !strings.Contains(reasons, want) {
			t.Fatalf("refusal did not identify %q:\n%s", want, reasons)
		}
	}
	if v.Routed.Runs != len(tasks)*3-1 || v.Routed.Solved != v.Routed.Runs || v.Routed.SolveRate != 1 {
		t.Fatalf("infrastructure leaked into quality rates: %#v", v.Routed)
	}
}

func TestMovedRoutedEstimateIsUnavailableRatherThanChargedToFinalTarget(t *testing.T) {
	run := Run{
		Arm: RoutedArm, TaskID: "move", Provenance: HandWritten, Solved: true,
		Target: "p/s/b", Cost: 100, EstimatedCost: 10, EstimatedTarget: "p/s/a",
		Escalations: 1,
	}
	got := Summarize(RoutedArm, []Run{run})
	if len(got.EstimateError) != 0 {
		t.Fatalf("opening estimate was attributed to the destination: %v", got.EstimateError)
	}
	if got.EstimatesUnavailable != 1 {
		t.Fatalf("unavailable estimates = %d, want 1", got.EstimatesUnavailable)
	}

	// Returning to the opening target does not make a multi-target run
	// comparable: the middle target's spend is still in the actual total.
	run.Target = run.EstimatedTarget
	got = Summarize(RoutedArm, []Run{run})
	if len(got.EstimateError) != 0 || got.EstimatesUnavailable != 1 {
		t.Fatalf("A -> B -> A estimate was treated as like-for-like: %#v", got)
	}
}

// §7.1 fails the gate on systematic underestimation regardless of the saving,
// because a router that under-predicts spend overruns budgets while passing a
// cost test.
func TestSystematicUnderestimationFailsRegardlessOfSaving(t *testing.T) {
	tasks := corpus(25, HandWritten)

	var all []Run
	all = append(all, runs("always-highest", 25, 3, true, 100)...)
	for _, r := range runs(RoutedArm, 25, 3, true, 50) {
		r.EstimatedCost = 25 // actual is double the estimate
		all = append(all, r)
	}

	v := Gate{}.Evaluate(tasks, all, pins())
	if v.Refused {
		t.Fatalf("refused: %v", v.Reasons)
	}
	if v.Passed {
		t.Error("a 50% saving passed while costing double what it predicted")
	}
	if !strings.Contains(strings.Join(v.Reasons, " "), "underestimation") {
		t.Errorf("reasons = %v", v.Reasons)
	}
}

// Cost per *solved* task, not per attempt: an arm that fails cheaply would
// otherwise look like the winner.
func TestCostIsPerSolvedTask(t *testing.T) {
	var all []Run
	for i := range 10 {
		run := Run{TaskID: "t", Provenance: HandWritten, Arm: "cheap-failer",
			Solved: i == 0, Cost: 10, Seed: i}
		if !run.Solved {
			run.Failure = FailureVerification
		}
		all = append(all, run)
	}
	got := Summarize("cheap-failer", all)
	if got.Solved != 1 {
		t.Fatalf("solved = %d", got.Solved)
	}
	if got.MedianCostPerSolved != 10 {
		t.Errorf("median = %s; only solved runs count toward it", got.MedianCostPerSolved)
	}
	if got.SolveRate > 0.11 {
		t.Errorf("solve rate = %.2f", got.SolveRate)
	}
}

// Overlapping intervals mean the difference has not been established, which is
// the honest thing to say at these sample sizes.
func TestOverlappingIntervalsAreDetectable(t *testing.T) {
	var all []Run
	for i, c := range []catalog.Money{100, 110, 120} {
		all = append(all, Run{Arm: "a", Solved: true, Cost: c, Seed: i})
	}
	for i, c := range []catalog.Money{105, 115, 125} {
		all = append(all, Run{Arm: "b", Solved: true, Cost: c, Seed: i})
	}

	a, b := CostInterval(all, "a"), CostInterval(all, "b")
	if !a.Overlaps(b) {
		t.Error("plainly overlapping samples were reported as separated")
	}

	var apart []Run
	for i, c := range []catalog.Money{10, 11, 12} {
		apart = append(apart, Run{Arm: "c", Solved: true, Cost: c, Seed: i})
	}
	c := CostInterval(apart, "c")
	if a.Overlaps(c) {
		t.Error("plainly separated samples were reported as overlapping")
	}
}

// On a ladder where every arm bills the same, cost never separates them, and
// leaving the choice there picked whichever arm a map yielded first. That
// chose a baseline solving 58% over one solving 97% and reported the routed arm
// as ahead when it was well behind.
func TestTheBestBaselineIsDeterministicWhenCostsTie(t *testing.T) {
	tasks := corpus(25, HandWritten)

	var all []Run
	all = append(all, runs(RoutedArm, 25, 3, true, 0)...)
	// Two free baselines: one strong, one weak. Cost cannot separate them.
	for i, r := range runs("weak-baseline", 25, 3, true, 0) {
		r.Solved = i%2 == 0
		if !r.Solved {
			r.Failure = FailureVerification
		}
		all = append(all, r)
	}
	all = append(all, runs("strong-baseline", 25, 3, true, 0)...)

	for range 5 {
		v := Gate{}.Evaluate(tasks, all, pins())
		if v.Baseline.Arm != "strong-baseline" {
			t.Fatalf("baseline = %q, want the strongest competitor; beating a weak one is not the claim",
				v.Baseline.Arm)
		}
	}
}
