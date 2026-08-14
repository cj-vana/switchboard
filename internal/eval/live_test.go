package eval

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/cjvana/switchboard/internal/catalog"
	"github.com/cjvana/switchboard/internal/credential"
	"github.com/cjvana/switchboard/internal/provider"
	"github.com/cjvana/switchboard/internal/provider/anthropic"
	"github.com/cjvana/switchboard/internal/provider/ollama"
)

// armsFor builds the fixed-target baselines §7.1 compares against.
func armsFor(t *testing.T) []Arm {
	t.Helper()

	secret, err := credential.Chain(credential.Settings{}).Get(
		context.Background(), credential.Ref{Provider: anthropic.Name, Account: anthropic.Surface})
	if err != nil {
		t.Skipf("no Anthropic credential: %v", err)
	}
	client := anthropic.New(anthropic.WithAPIKey(secret.Expose()))

	return []Arm{
		{Name: "always-lowest", Target: ollama.Target("qwen3.5:9b-mlx"), Provider: ollama.New()},
		{Name: "always-highest", Target: anthropic.Target("claude-haiku-4-5"), Provider: client},
	}
}

// TestLiveBaselineRuns is the baseline half of §8.6: pinned targets across the
// corpus, which is what "appropriate tier" labels are later derived from. It is
// scoped by SB_EVAL_TASKS because the full corpus on every arm is hours and
// real money.
func TestLiveBaselineRuns(t *testing.T) {
	if os.Getenv("SB_LIVE") == "" {
		t.Skip("set SB_LIVE=1 to run the corpus against live targets (this spends money)")
	}

	cat, err := catalog.LoadBundled()
	if err != nil {
		t.Fatal(err)
	}

	root, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	tasks := Tier1(root + "/../..")

	// A subset by default. The gate refuses below the floor, which is the
	// correct outcome for a partial run and the reason a partial run cannot be
	// mistaken for a measurement.
	limit := 3
	if n := os.Getenv("SB_EVAL_TASKS"); n != "" {
		fmt.Sscanf(n, "%d", &limit)
	}
	if limit > len(tasks) {
		limit = len(tasks)
	}
	tasks = tasks[:limit]

	seeds := 1
	if n := os.Getenv("SB_EVAL_SEEDS"); n != "" {
		fmt.Sscanf(n, "%d", &seeds)
	}

	runner := Runner{Catalog: cat, Timeout: 8 * time.Minute}
	ctx := context.Background()

	arms := armsFor(t)
	routed := RoutedArmFor{Catalog: cat, Ladder: arms}

	log := func(got Run, arm string) {
		status := "solved"
		if !got.Solved {
			status = "unsolved"
		}
		t.Logf("%-28s %-16s %-8s %8s %6.1fs  %s",
			got.TaskID, arm, status, got.Cost, got.Duration.Seconds(), firstLine(got.Detail))
	}

	var runs []Run
	for _, arm := range arms {
		for _, task := range tasks {
			for seed := range seeds {
				got := runner.Run(ctx, task, arm, seed)
				runs = append(runs, got)
				log(got, arm.Name)
			}
		}
	}

	// The arm under test: same corpus, same tools, same verifier, and the router
	// choosing the target instead of it being fixed.
	for _, task := range tasks {
		for seed := range seeds {
			got := routed.Run(ctx, runner, task, seed)
			runs = append(runs, got)
			log(got, got.Arm)
		}
	}

	// A router that always picks the same rung is a baseline under another
	// name, and a comparison against it would be measuring nothing.
	used := TargetsUsed(runs)
	moved, totalRouted := Escalations(runs)
	t.Logf("the routed arm ended on: %v", used)
	t.Logf("escalated on %d of %d routed runs", moved, totalRouted)
	if moved == 0 && len(arms) > 1 {
		t.Logf("note: no routed run ever changed target, so this arm is a fixed baseline " +
			"under another name and the comparison answers nothing")
	}

	report(t, runs)

	// The gate must refuse a partial corpus rather than report a number.
	v := Gate{}.Evaluate(tasks, runs, Pins{
		HarnessCommit:   "live",
		CatalogRevision: cat.Revision,
		PromptVersion:   "v1",
		Snapshots:       map[provider.RouteTargetID]string{"anthropic/first-party/claude-haiku-4-5": "claude-haiku-4-5-20251001"},
	})
	if len(tasks) < MinimumTier1Tasks && !v.Refused {
		t.Errorf("a %d task run produced a verdict rather than refusing", len(tasks))
	}
	for _, reason := range v.Reasons {
		t.Logf("gate: %s", reason)
	}
}

func report(t *testing.T, runs []Run) {
	t.Helper()

	arms := map[string]bool{}
	for _, r := range runs {
		arms[r.Arm] = true
	}
	names := make([]string, 0, len(arms))
	for a := range arms {
		names = append(names, a)
	}
	sort.Strings(names)

	t.Log("")
	for _, name := range names {
		res := Summarize(name, runs)
		interval := CostInterval(runs, name)
		t.Logf("%-16s solved %d/%d  median cost %s  spread %s to %s  median %s",
			name, res.Solved, res.Runs, res.MedianCostPerSolved,
			interval.Low, interval.High, res.MedianLatency.Round(time.Second))
	}
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > 70 {
		s = s[:70] + "..."
	}
	return s
}
