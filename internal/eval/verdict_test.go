package eval

import (
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/switchboard-code/switchboard/internal/catalog"
)

// TestReportJournal recomputes the gate from a recorded run, so a verdict can be
// corrected without paying for the corpus again.
func TestReportJournal(t *testing.T) {
	path := os.Getenv("SB_EVAL_JOURNAL")
	if path == "" {
		t.Skip("set SB_EVAL_JOURNAL to a recorded run")
	}
	runs, err := ReadJournal(path)
	if err != nil {
		t.Fatal(err)
	}

	tasks := Tier1("../..")
	tasks = selectTasks(t, tasks, len(tasks))
	v := (Gate{
		RequireEvaluationID: true,
		EvaluationWorkers:   positiveIntEnv(t, "SB_EVAL_WORKERS", 0),
	}).Evaluate(tasks, runs, Pins{
		HarnessCommit:   os.Getenv("SB_EVAL_COMMIT"),
		CatalogRevision: os.Getenv("SB_EVAL_CATALOG"),
		PromptVersion:   "v1",
		Snapshots:       snapshotsFromEnv(t),
	})

	t.Logf("routed   %d/%d solved, median %s per solved task",
		v.Routed.Solved, v.Routed.Runs, v.Routed.MedianCostPerSolved)
	t.Logf("baseline %s: %d/%d solved, median %s per solved task",
		v.Baseline.Arm, v.Baseline.Solved, v.Baseline.Runs, v.Baseline.MedianCostPerSolved)
	t.Logf("verdict: passed=%v refused=%v", v.Passed, v.Refused)
	for _, r := range v.Reasons {
		t.Logf("  %s", r)
	}
}

// TestDeriveLadder reads an ordering off recorded runs, which is what §8.6 says
// tier labels come from. It runs against a journal rather than the network, so
// a ladder can be re-derived as evidence accumulates without paying again.
func TestDeriveLadder(t *testing.T) {
	path := os.Getenv("SB_EVAL_JOURNAL")
	if path == "" {
		t.Skip("set SB_EVAL_JOURNAL to a recorded run")
	}
	runs, err := ReadJournal(path)
	if err != nil {
		t.Fatal(err)
	}
	cat, err := catalog.LoadBundled()
	if err != nil {
		t.Fatal(err)
	}

	front := DeriveFront(runs, cat, 10)

	t.Log("measured positions:")
	for _, p := range front.Points {
		dominated := ""
		if len(p.Dominated) > 0 {
			dominated = fmt.Sprintf("  dominated by %v", p.Dominated)
		}
		t.Logf("  %-44s %2d/%2d solved (%.0f%%)  median %s  %s%s",
			p.Target, p.Solved, p.Attempts, p.SolveRate*100,
			p.MedianCost, p.MedianLatency.Round(time.Second), dominated)
	}

	t.Log("")
	if len(front.Ladder) == 0 {
		t.Log("no ladder survives the evidence")
	}
	for i, target := range front.Ladder {
		t.Logf("  rung %d  %s", i+1, target)
	}
	for _, w := range front.Warnings {
		t.Logf("  warning: %s", w)
	}
}
