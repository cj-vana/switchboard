package eval

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cjvana/switchboard/internal/catalog"
	"github.com/cjvana/switchboard/internal/credential"
	"github.com/cjvana/switchboard/internal/provider"
	"github.com/cjvana/switchboard/internal/provider/anthropic"
	"github.com/cjvana/switchboard/internal/provider/kimi"
	"github.com/cjvana/switchboard/internal/provider/ollama"
)

// armsFor builds the ladder, lowest rung first. The baselines are its ends.
//
// SB_EVAL_LADDER selects the configuration, because the choice changes what the
// gate can establish and that is not a detail to leave implicit:
//
//	money  a paid ladder, where §7.1's cost condition means what it says
//	plan   a plan-metered ladder, where every rung bills zero and the cost
//	       condition is degenerate, so only the solve-rate half is measured
func armsFor(t *testing.T) []Arm {
	t.Helper()

	key := func(provider, surface string) string {
		secret, err := credential.Chain(credential.Settings{}).Get(
			context.Background(), credential.Ref{Provider: provider, Account: surface})
		if err != nil {
			t.Skipf("no credential for %s/%s: %v", provider, surface, err)
		}
		return secret.Expose()
	}

	switch os.Getenv("SB_EVAL_LADDER") {
	case "cache":
		// §7.1's second condition, and the cheapest experiment that tests the
		// thesis this project is named for: one model, one corpus, and the only
		// difference is whether §6 places markers at all.
		client := anthropic.New(anthropic.WithAPIKey(key(anthropic.Name, anthropic.Surface)))
		return []Arm{
			{Name: "cache-unaware", Target: anthropic.Target("claude-haiku-4-5"), Provider: client},
			{Name: "cache-aware", Target: anthropic.Target("claude-haiku-4-5"), Provider: client, CacheAware: true},
		}

	case "money":
		// Two paid models on one provider, which §19.3 allows explicitly and
		// which is the only shape where a cost comparison is about money.
		client := anthropic.New(anthropic.WithAPIKey(key(anthropic.Name, anthropic.Surface)))
		return []Arm{
			{Name: "always-lowest", Target: anthropic.Target("claude-haiku-4-5"), Provider: client},
			{Name: "always-highest", Target: anthropic.Target("claude-opus-5"), Provider: client},
		}

	default:
		// Free and plan-metered. Nothing here bills per token, so the cost half
		// of the gate cannot be measured and the report says so rather than
		// reporting a saving that is an artefact of the ladder.
		return []Arm{
			{Name: "always-lowest", Target: ollama.Target("qwen3.5:9b-mlx"), Provider: ollama.New()},
			{Name: "always-highest", Target: kimi.Target("k3-256k"), Provider: kimi.New(key(kimi.Name, kimi.Surface))},
		}
	}
}

// meteringNote reports what the chosen ladder can and cannot establish.
//
// A plan-metered ladder makes every arm cost zero, so the router "wins" on cost
// by construction. Reporting that as a saving would be the confident wrong
// number §8.6 exists to prevent.
func meteringNote(cat *catalog.Catalog, arms []Arm) string {
	paid := 0
	for _, arm := range arms {
		if info, _, ok := cat.Lookup(arm.Target); ok && info.Metering == catalog.PerToken {
			paid++
		}
	}
	if paid < 2 {
		return "this ladder is free or plan-metered, so every arm bills zero and §7.1's cost " +
			"condition is degenerate; only the solve-rate half is being measured here"
	}
	return ""
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
	if note := meteringNote(cat, arms); note != "" {
		t.Logf("note: %s", note)
	}

	log := func(got Run, arm string) {
		status := "solved"
		if !got.Solved {
			status = "unsolved"
		}
		denials := ""
		if got.Denials > 0 {
			denials = fmt.Sprintf(" %dd", got.Denials)
		}
		moves := ""
		if got.Escalations > 0 {
			moves = fmt.Sprintf(" %d^", got.Escalations)
		}
		t.Logf("%-28s %-16s %-8s %9s %6.1fs%s%s  %s",
			got.TaskID, arm, status, got.Cost, got.Duration.Seconds(), moves, denials, firstLine(got.Detail))
	}

	// Attempts are independent by construction: each gets its own copy of the
	// repository, its own session, and its own sandbox check. Running them
	// concurrently is what makes a corpus this size finishable, since the full
	// gate is a few hundred attempts of several minutes each.
	workers := 4
	if n := os.Getenv("SB_EVAL_WORKERS"); n != "" {
		fmt.Sscanf(n, "%d", &workers)
	}

	type job func() Run
	var jobs []job
	for _, arm := range arms {
		for _, task := range tasks {
			for seed := range seeds {
				jobs = append(jobs, func() Run { return runner.Run(ctx, task, arm, seed) })
			}
		}
	}
	// The arm under test: same corpus, same tools, same verifier, and the router
	// choosing the target instead of it being fixed.
	for _, task := range tasks {
		for seed := range seeds {
			jobs = append(jobs, func() Run { return routed.Run(ctx, runner, task, seed) })
		}
	}

	// Results are durable as they happen. A run this long dies to a deadline
	// often enough that holding them in memory loses the whole measurement.
	journalPath := os.Getenv("SB_EVAL_JOURNAL")
	if journalPath == "" {
		journalPath = "eval-runs.jsonl"
	}
	journal, err := NewJournal(journalPath)
	if err != nil {
		t.Fatal(err)
	}
	defer journal.Close()
	t.Logf("recording each attempt to %s as it finishes", journalPath)

	var mu sync.Mutex
	var wg sync.WaitGroup
	var runs []Run
	queue := make(chan job)

	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range queue {
				got := j()
				if err := journal.Append(got); err != nil {
					t.Errorf("recording an attempt failed: %v", err)
				}
				mu.Lock()
				runs = append(runs, got)
				log(got, got.Arm)
				mu.Unlock()
			}
		}()
	}
	for _, j := range jobs {
		queue <- j
	}
	close(queue)
	wg.Wait()

	sort.Slice(runs, func(i, j int) bool {
		if runs[i].Arm != runs[j].Arm {
			return runs[i].Arm < runs[j].Arm
		}
		return runs[i].TaskID < runs[j].TaskID
	})

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
