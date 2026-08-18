package eval

import (
	"strings"
	"testing"

	"github.com/switchboard-code/switchboard/internal/provider"
)

func TestEvaluationIdentityIsOrderIndependentButBindsArmTargets(t *testing.T) {
	tasks := corpus(2, HandWritten)
	first := evaluationID(
		tasks,
		[]string{RoutedArm, "low", "high"},
		map[string][]provider.RouteTargetID{"low": {"p/s/low"}, "high": {"p/s/high"}},
		[]provider.RouteTargetID{"p/s/high", "p/s/low"},
		[]int{2, 0, 1},
		4,
		pins(),
	)
	reordered := evaluationID(
		tasks,
		[]string{"high", "low", RoutedArm},
		map[string][]provider.RouteTargetID{"high": {"p/s/high"}, "low": {"p/s/low"}},
		[]provider.RouteTargetID{"p/s/low", "p/s/high"},
		[]int{0, 1, 2},
		4,
		pins(),
	)
	if first != reordered {
		t.Fatalf("equivalent configurations produced %q and %q", first, reordered)
	}

	swapped := evaluationID(
		tasks,
		[]string{RoutedArm, "low", "high"},
		map[string][]provider.RouteTargetID{"low": {"p/s/high"}, "high": {"p/s/low"}},
		[]provider.RouteTargetID{"p/s/high", "p/s/low"},
		[]int{0, 1, 2},
		4,
		pins(),
	)
	if first == swapped {
		t.Fatal("swapping fixed-arm target assignments did not change the evaluation identity")
	}

	differentConcurrency := evaluationID(
		tasks,
		[]string{RoutedArm, "low", "high"},
		map[string][]provider.RouteTargetID{"low": {"p/s/low"}, "high": {"p/s/high"}},
		[]provider.RouteTargetID{"p/s/high", "p/s/low"},
		[]int{0, 1, 2},
		2,
		pins(),
	)
	if first == differentConcurrency {
		t.Fatal("changing provider concurrency did not change the evaluation identity")
	}
}

func TestStrictGateAcceptsOnlyRowsBoundToItsConfiguration(t *testing.T) {
	tasks := corpus(25, HandWritten)
	all := append(runs(RoutedArm, 25, 3, true, 60), runs("always-highest", 25, 3, true, 100)...)
	id := evaluationID(
		tasks,
		[]string{RoutedArm, "always-highest"},
		map[string][]provider.RouteTargetID{"always-highest": {"t/s/m"}},
		[]provider.RouteTargetID{"t/s/m"},
		[]int{0, 1, 2},
		4,
		pins(),
	)
	for i := range all {
		all[i].EvaluationID = id
	}

	v := (Gate{RequireEvaluationID: true, EvaluationWorkers: 4}).Evaluate(tasks, all, pins())
	if v.Refused || !v.Passed {
		t.Fatalf("correctly bound evidence did not pass: %#v", v)
	}

	all[0].EvaluationID = "eval-v1:stale"
	v = (Gate{RequireEvaluationID: true, EvaluationWorkers: 4}).Evaluate(tasks, all, pins())
	if !v.Refused || !strings.Contains(strings.Join(v.Reasons, " "), "1 mismatched") {
		t.Fatalf("stale evidence was not refused: %#v", v)
	}
}

func TestStrictGateRefusesLegacyRowsWithoutEvaluationIdentity(t *testing.T) {
	tasks := corpus(25, HandWritten)
	all := append(runs(RoutedArm, 25, 3, true, 60), runs("always-highest", 25, 3, true, 100)...)

	v := (Gate{RequireEvaluationID: true, EvaluationWorkers: 4}).Evaluate(tasks, all, pins())
	if !v.Refused || !strings.Contains(strings.Join(v.Reasons, " "), "missing identity") {
		t.Fatalf("unbound legacy evidence was not refused: %#v", v)
	}
}

func TestSharedTargetAcrossConfiguredArmsIsValid(t *testing.T) {
	tasks := corpus(25, HandWritten)
	all := append(runs(RoutedArm, 25, 3, true, 60), runs("cache-aware", 25, 3, true, 100)...)
	all = append(all, runs("cache-unaware", 25, 3, true, 100)...)

	v := (Gate{
		ExpectedArms:    []string{RoutedArm, "cache-aware", "cache-unaware"},
		ExpectedTargets: []provider.RouteTargetID{"t/s/m", "t/s/m"},
	}).Evaluate(tasks, all, pins())
	if v.Refused {
		t.Fatalf("two controls on one target were refused: %v", v.Reasons)
	}
}
