// Package eval is the harness the router is measured with.
//
// §8.6 says built early rather than late, because it is the difference between
// the router being a product and being a demo. It also sets the rule this
// package enforces above all others: the harness does not ship before the tier-1
// corpus is populated, since an eval harness with an empty corpus produces
// confident numbers about nothing, and those numbers get quoted.
//
// So a verdict is refused rather than approximated when the corpus is too small.
// That refusal is the feature. Everything else here is arithmetic over runs, and
// arithmetic over four tasks looks exactly like arithmetic over forty.
package eval

import (
	"fmt"
	"math"
	"sort"
	"time"

	"github.com/cjvana/switchboard/internal/catalog"
	"github.com/cjvana/switchboard/internal/provider"
)

// Provenance is where a task came from, which decides how much its result is
// worth. §8.6 keeps the three separate because they carry different trust.
type Provenance int

const (
	// HandWritten is tier 1: authored from repositories whose ground truth the
	// author can establish directly. Small and uncontaminated, and the only
	// tier the gate measurement depends on.
	HandWritten Provenance = 1

	// FromPullRequest is tier 2: extracted from merged public pull requests
	// where the suite fails at the parent and passes at the merge. Credible at
	// volume, and contaminated by any model whose training predates the PR,
	// which is why its results are reported separately and annotated with each
	// target's cutoff.
	FromPullRequest Provenance = 2

	// Synthetic is tier 3: volume on top of the other two. It validates the
	// harness rather than the models, because the generator's notion of
	// difficulty is the thing under test.
	Synthetic Provenance = 3
)

func (p Provenance) String() string {
	switch p {
	case HandWritten:
		return "hand-written"
	case FromPullRequest:
		return "from a pull request"
	case Synthetic:
		return "synthetic"
	}
	return "unknown"
}

// MinimumTier1Tasks is the floor §8.6 sets at twenty to thirty tasks. Below it
// no verdict is produced.
const MinimumTier1Tasks = 20

// Task is one unit of work with an executable verifier.
//
// Verify is a function rather than a description because §8.6 wants independent
// verification: a harness that asks the model whether it succeeded is measuring
// the model's opinion of itself.
type Task struct {
	ID         string
	Provenance Provenance
	Prompt     string

	// Setup materialises the workspace. It runs fresh per attempt, so one
	// attempt cannot see another's edits.
	Setup func(dir string) error

	// Verify decides whether the task was solved, and says why when it was not.
	Verify func(dir string) (solved bool, detail string, err error)
}

// Run is one attempt.
type Run struct {
	TaskID     string
	Provenance Provenance
	Target     provider.RouteTargetID

	// Arm names what produced this run: a fixed target used as a baseline, or
	// the router. §7.1 compares against the best fixed-target baseline, so the
	// arms have to stay distinguishable.
	Arm string

	Solved   bool
	Detail   string
	Cost     catalog.Money
	Usage    provider.Usage
	Duration time.Duration

	// EstimatedCost is what the model predicted before the run, which §7.1
	// requires be reported against the actual per target.
	EstimatedCost catalog.Money

	Escalations int
	Seed        int
}

// Pins are what makes a report reproducible. §8.6 requires every one of these,
// because a number without them cannot be compared to a later number.
type Pins struct {
	HarnessCommit   string
	CatalogRevision string
	PromptVersion   string

	// Snapshots maps each target to the dated model it actually resolved to. An
	// alias moves; a report that recorded only the alias describes a
	// measurement nobody can repeat.
	Snapshots map[provider.RouteTargetID]string
}

func (p Pins) complete() []string {
	var missing []string
	if p.HarnessCommit == "" {
		missing = append(missing, "harness commit")
	}
	if p.CatalogRevision == "" {
		missing = append(missing, "catalog revision")
	}
	if p.PromptVersion == "" {
		missing = append(missing, "prompt version")
	}
	if len(p.Snapshots) == 0 {
		missing = append(missing, "model snapshots")
	}
	return missing
}

// ArmResult is one arm's performance over a corpus.
type ArmResult struct {
	Arm   string
	Runs  int
	Tasks int

	Solved    int
	SolveRate float64

	// MedianCostPerSolved is the §7.1 figure. It is per *solved* task on
	// purpose: cost per attempt rewards an arm that fails cheaply, which is the
	// opposite of what is being bought.
	MedianCostPerSolved catalog.Money
	MedianLatency       time.Duration

	// EstimateError is actual over estimated, per §7.1's requirement that this
	// be reported by target with no systematic underestimation.
	EstimateError map[provider.RouteTargetID]float64
}

// Summarize reduces runs to one arm's result.
func Summarize(arm string, runs []Run) ArmResult {
	res := ArmResult{Arm: arm, EstimateError: map[provider.RouteTargetID]float64{}}

	tasks := map[string]bool{}
	var solvedCosts []catalog.Money
	var solvedLatency []time.Duration
	estimated := map[provider.RouteTargetID][2]float64{}

	for _, r := range runs {
		if r.Arm != arm {
			continue
		}
		res.Runs++
		tasks[r.TaskID] = true
		if r.Solved {
			res.Solved++
			solvedCosts = append(solvedCosts, r.Cost)
			solvedLatency = append(solvedLatency, r.Duration)
		}
		if r.EstimatedCost > 0 {
			acc := estimated[r.Target]
			estimated[r.Target] = [2]float64{acc[0] + float64(r.Cost), acc[1] + float64(r.EstimatedCost)}
		}
	}

	res.Tasks = len(tasks)
	if res.Runs > 0 {
		res.SolveRate = float64(res.Solved) / float64(res.Runs)
	}
	res.MedianCostPerSolved = medianMoney(solvedCosts)
	res.MedianLatency = medianDuration(solvedLatency)

	for target, acc := range estimated {
		if acc[1] > 0 {
			res.EstimateError[target] = acc[0] / acc[1]
		}
	}
	return res
}

func medianMoney(values []catalog.Money) catalog.Money {
	if len(values) == 0 {
		return 0
	}
	sorted := append([]catalog.Money(nil), values...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	return sorted[len(sorted)/2]
}

func medianDuration(values []time.Duration) time.Duration {
	if len(values) == 0 {
		return 0
	}
	sorted := append([]time.Duration(nil), values...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	return sorted[len(sorted)/2]
}

// Gate is the §7.1 falsification gate.
//
// The thresholds are provisional product targets and §7.1 is explicit that they
// must be changed before seeing decisive results rather than after, which is why
// they are fields with documented defaults rather than constants buried in the
// comparison.
type Gate struct {
	// MinCostReduction against the best fixed-target baseline.
	MinCostReduction float64

	// MaxSolveRateDrop in absolute percentage points.
	MaxSolveRateDrop float64

	// MinRunsPerTask is how many seeds are needed before a median means
	// anything. §8.6 asks for results over multiple runs with uncertainty
	// intervals.
	MinRunsPerTask int
}

const (
	DefaultMinCostReduction = 0.20
	DefaultMaxSolveRateDrop = 0.02
	DefaultMinRunsPerTask   = 3
)

func (g Gate) minCostReduction() float64 {
	if g.MinCostReduction > 0 {
		return g.MinCostReduction
	}
	return DefaultMinCostReduction
}

func (g Gate) maxSolveRateDrop() float64 {
	if g.MaxSolveRateDrop > 0 {
		return g.MaxSolveRateDrop
	}
	return DefaultMaxSolveRateDrop
}

func (g Gate) minRuns() int {
	if g.MinRunsPerTask > 0 {
		return g.MinRunsPerTask
	}
	return DefaultMinRunsPerTask
}

// Verdict is the gate's answer.
//
// Refused is separate from Passed and Failed on purpose. A gate that cannot be
// measured has not been passed and has not been failed, and collapsing those
// into one boolean is how an unmeasured gate gets reported as a green one.
type Verdict struct {
	Passed  bool
	Refused bool

	// Reasons explains the verdict in full. A gate reported as a single word is
	// a gate nobody can argue with, which §0 of the design asks for the
	// opposite of.
	Reasons []string

	Routed   ArmResult
	Baseline ArmResult

	CostReduction float64
	SolveRateDrop float64
}

// Evaluate runs the gate over a corpus of runs.
//
// It refuses before it compares. §8.6's rule about an unpopulated corpus is not
// advice: numbers computed over four tasks are indistinguishable in shape from
// numbers over forty, and only one of them means anything.
func (g Gate) Evaluate(tasks []Task, runs []Run, pins Pins) Verdict {
	var v Verdict

	if missing := pins.complete(); len(missing) > 0 {
		v.Refused = true
		v.Reasons = append(v.Reasons, fmt.Sprintf(
			"the report is not reproducible: no %s recorded", joinWords(missing)))
	}

	tier1 := 0
	for _, t := range tasks {
		if t.Provenance == HandWritten {
			tier1++
		}
	}
	if tier1 < MinimumTier1Tasks {
		v.Refused = true
		v.Reasons = append(v.Reasons, fmt.Sprintf(
			"the corpus has %d hand-written tasks and the gate needs %d; "+
				"§8.6 keeps the harness from shipping before this is populated, because confident numbers "+
				"about an empty corpus are worse than no numbers at all",
			tier1, MinimumTier1Tasks))
	}

	// Only tier 1 counts toward the gate. Tier 2 is contaminated by training
	// cutoffs and tier 3 measures the harness, so both are reported elsewhere
	// and neither decides this.
	var eligible []Run
	perTask := map[string]int{}
	for _, r := range runs {
		if r.Provenance != HandWritten {
			continue
		}
		eligible = append(eligible, r)
		perTask[r.TaskID]++
	}

	for task, count := range perTask {
		if count < g.minRuns() {
			v.Refused = true
			v.Reasons = append(v.Reasons, fmt.Sprintf(
				"task %s was run %d time(s) and a median needs at least %d", task, count, g.minRuns()))
			break
		}
	}

	arms := map[string]bool{}
	for _, r := range eligible {
		arms[r.Arm] = true
	}
	if !arms[RoutedArm] {
		v.Refused = true
		v.Reasons = append(v.Reasons, "no routed runs to compare")
	}

	// The best fixed-target baseline is the one that beats the router hardest,
	// which is the comparison §7.1 asks for: an improvement over the worst
	// alternative is not an improvement.
	v.Routed = Summarize(RoutedArm, eligible)
	best := ArmResult{}
	for arm := range arms {
		if arm == RoutedArm {
			continue
		}
		candidate := Summarize(arm, eligible)
		if candidate.Solved == 0 {
			continue
		}
		if best.Arm == "" || candidate.MedianCostPerSolved < best.MedianCostPerSolved {
			best = candidate
		}
	}
	if best.Arm == "" {
		v.Refused = true
		v.Reasons = append(v.Reasons, "no fixed-target baseline solved anything to compare against")
	}
	v.Baseline = best

	if v.Refused {
		v.Reasons = append(v.Reasons,
			"refused rather than failed: this gate has not been measured, which is not the same as not having been met")
		return v
	}

	if best.MedianCostPerSolved > 0 {
		v.CostReduction = 1 - float64(v.Routed.MedianCostPerSolved)/float64(best.MedianCostPerSolved)
	}
	v.SolveRateDrop = best.SolveRate - v.Routed.SolveRate

	costOK := v.CostReduction >= g.minCostReduction()
	safetyOK := v.SolveRateDrop <= g.maxSolveRateDrop()

	if costOK {
		v.Reasons = append(v.Reasons, fmt.Sprintf(
			"median cost per verified solved task fell %.1f%% against %s, clearing the %.0f%% threshold",
			v.CostReduction*100, best.Arm, g.minCostReduction()*100))
	} else {
		v.Reasons = append(v.Reasons, fmt.Sprintf(
			"median cost per verified solved task moved %.1f%% against %s, short of the %.0f%% the gate requires",
			v.CostReduction*100, best.Arm, g.minCostReduction()*100))
	}

	if safetyOK {
		v.Reasons = append(v.Reasons, fmt.Sprintf(
			"verified solve rate moved %.1f points, inside the %.0f point allowance",
			-v.SolveRateDrop*100, g.maxSolveRateDrop()*100))
	} else {
		v.Reasons = append(v.Reasons, fmt.Sprintf(
			"verified solve rate fell %.1f points, past the %.0f point allowance",
			v.SolveRateDrop*100, g.maxSolveRateDrop()*100))
	}

	// §7.1 requires estimate-versus-actual by target with no systematic
	// underestimation, because a router that under-predicts spend can pass a
	// cost gate while overrunning a budget.
	for target, ratio := range v.Routed.EstimateError {
		if ratio > 1.05 {
			safetyOK = false
			v.Reasons = append(v.Reasons, fmt.Sprintf(
				"%s cost %.0f%% more than estimated, which is systematic underestimation and fails the gate regardless of the saving",
				target, (ratio-1)*100))
		}
	}

	v.Passed = costOK && safetyOK
	return v
}

// RoutedArm is the arm under test. The baselines are named by the caller, since
// "always lowest" and "always highest" depend on the ladder.
const RoutedArm = "routed"

func joinWords(words []string) string {
	switch len(words) {
	case 0:
		return ""
	case 1:
		return words[0]
	case 2:
		return words[0] + " or " + words[1]
	}
	return words[0] + ", " + joinWords(words[1:])
}

// Interval is a simple uncertainty interval over a sample, reported because
// §8.6 requires one and a bare median invites over-reading a difference that a
// second run would erase.
type Interval struct {
	Median catalog.Money
	Low    catalog.Money
	High   catalog.Money
	N      int
}

// CostInterval reports the median with a bootstrap-free spread: the lowest and
// highest observed. With the handful of seeds §8.6 asks for, a percentile
// interval would be a confident-sounding restatement of the range.
func CostInterval(runs []Run, arm string) Interval {
	var costs []catalog.Money
	for _, r := range runs {
		if r.Arm == arm && r.Solved {
			costs = append(costs, r.Cost)
		}
	}
	if len(costs) == 0 {
		return Interval{}
	}
	sort.Slice(costs, func(i, j int) bool { return costs[i] < costs[j] })
	return Interval{
		Median: costs[len(costs)/2],
		Low:    costs[0],
		High:   costs[len(costs)-1],
		N:      len(costs),
	}
}

// Overlaps reports whether two intervals overlap, which is the honest way to
// say a difference has not been established at this sample size.
func (i Interval) Overlaps(o Interval) bool {
	return math.Max(float64(i.Low), float64(o.Low)) <= math.Min(float64(i.High), float64(o.High))
}
