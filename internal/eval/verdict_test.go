package eval

import (
	"os"
	"testing"

	"github.com/cjvana/switchboard/internal/provider"
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
	v := Gate{}.Evaluate(tasks, runs, Pins{
		HarnessCommit:   os.Getenv("SB_EVAL_COMMIT"),
		CatalogRevision: os.Getenv("SB_EVAL_CATALOG"),
		PromptVersion:   "v1",
		Snapshots:       map[provider.RouteTargetID]string{"kimi/coding/k3-256k": "k3-256k"},
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
