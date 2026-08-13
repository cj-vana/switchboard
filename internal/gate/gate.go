// Package gate runs identical tasks on two pinned route targets and reports
// how far the token estimator was from what the servers actually charged.
//
// This is the phase-1 exit gate from §19.2: "identical tasks run on two pinned
// targets and actual usage reconciles within documented estimator error." The
// second half is the part that needs a harness, because "documented estimator
// error" is not a number anyone has yet. `provider.TokenEstimate` is flagged
// inexact and the estimate behind it is characters divided by four; how wrong
// that is has been an open question rather than a measurement.
//
// Both reachable targets are free, so the dollar half of reconciliation is
// $0 against $0 and proves nothing. The token half is where the uncertainty
// actually lives: cost is a multiplication over token counts, so an estimator
// whose error is bounded gives a cost estimate whose error is bounded by the
// same factor, and one whose error is unknown gives a budget check that is
// guesswork however carefully the arithmetic is done.
package gate

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strings"

	"github.com/cjvana/switchboard/internal/provider"
)

// MinRatio and MaxRatio bound estimate-over-actual for the targets this build
// can reach. They come from a run, not from a preference: 18 calls across two
// targets landed between 0.76 and 0.82, and the band is widened from there to
// absorb run-to-run variation in what the model chooses to do without hiding a
// real change. docs/estimator.md records the measurement and has to be updated
// whenever these move.
const (
	MinRatio = 0.70
	MaxRatio = 1.10
)

// Sample is one model call: what the estimator predicted before the request was
// sent, and what the server reported for the same request.
type Sample struct {
	Target   provider.RouteTargetID
	Task     string
	Estimate int
	Actual   int
}

// Ratio is estimate over actual. Below 1 the estimator undercounts, which is
// the dangerous direction: a budget check that believes a request is smaller
// than it is approves spending that has already happened by the time the
// server disagrees.
func (s Sample) Ratio() float64 {
	if s.Actual == 0 {
		return math.NaN()
	}
	return float64(s.Estimate) / float64(s.Actual)
}

// Recorder wraps a provider and measures its estimator against its own reported
// usage.
//
// It sits between the loop and the adapter rather than inside either, so what
// gets measured is every request the loop actually sent, in the shape it
// actually sent it: system prompt, tool schemas, accumulated history and all.
// An estimator checked only against hand-built requests is checked against the
// easy case.
type Recorder struct {
	provider.Provider

	Target provider.RouteTargetID
	Task   string

	samples []Sample
}

func NewRecorder(p provider.Provider) *Recorder { return &Recorder{Provider: p} }

func (r *Recorder) Samples() []Sample { return r.samples }

func (r *Recorder) Stream(ctx context.Context, target provider.RouteTarget, req provider.Request) (provider.EventStream, error) {
	estimate, err := r.Provider.CountTokens(ctx, target, req)
	if err != nil {
		return nil, err
	}

	s, err := r.Provider.Stream(ctx, target, req)
	if err != nil {
		return nil, err
	}
	return &recordingStream{
		EventStream: s,
		recorder:    r,
		target:      target.ID(),
		estimate:    estimate.InputTokens,
	}, nil
}

type recordingStream struct {
	provider.EventStream

	recorder *Recorder
	target   provider.RouteTargetID
	estimate int
}

func (s *recordingStream) Next() (provider.Event, error) {
	ev, err := s.EventStream.Next()
	if err == nil && ev.Type == provider.EventDone {
		// The cached read is added back: the estimator predicts the whole
		// prompt, and the adapter reports the uncached remainder separately.
		// Comparing against the remainder would credit the estimator for a
		// cache it knows nothing about.
		actual := ev.Usage.InputTokens + ev.Usage.CacheReadTokens
		s.recorder.samples = append(s.recorder.samples, Sample{
			Target:   s.target,
			Task:     s.recorder.Task,
			Estimate: s.estimate,
			Actual:   actual,
		})
	}
	return ev, err
}

// Summary is the estimator's measured behavior over a set of samples.
type Summary struct {
	Target provider.RouteTargetID
	Calls  int

	MinRatio    float64
	MaxRatio    float64
	MedianRatio float64

	// WorstUnderReport is the largest fraction by which the estimator claimed a
	// request was smaller than it turned out to be. It is reported on its own
	// because it is the direction a budget check can be hurt by.
	WorstUnderReport float64
}

func Summarize(target provider.RouteTargetID, samples []Sample) Summary {
	sum := Summary{Target: target, MinRatio: math.Inf(1), MaxRatio: math.Inf(-1)}

	var ratios []float64
	for _, s := range samples {
		if s.Target != target || math.IsNaN(s.Ratio()) {
			continue
		}
		ratio := s.Ratio()
		ratios = append(ratios, ratio)
		sum.Calls++
		sum.MinRatio = math.Min(sum.MinRatio, ratio)
		sum.MaxRatio = math.Max(sum.MaxRatio, ratio)
		if under := 1 - ratio; under > sum.WorstUnderReport {
			sum.WorstUnderReport = under
		}
	}
	if len(ratios) == 0 {
		return Summary{Target: target}
	}

	sort.Float64s(ratios)
	sum.MedianRatio = ratios[len(ratios)/2]
	return sum
}

func (s Summary) String() string {
	if s.Calls == 0 {
		return fmt.Sprintf("%s: no calls recorded", s.Target)
	}
	return fmt.Sprintf("%-44s %3d calls  ratio %.2f-%.2f (median %.2f)  worst undercount %.0f%%",
		s.Target, s.Calls, s.MinRatio, s.MaxRatio, s.MedianRatio, s.WorstUnderReport*100)
}

// Table renders per-call detail, which is what makes a bad summary
// actionable: a single outlying request is a different problem from a bias
// that holds across every call.
func Table(samples []Sample) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%-44s %-22s %9s %9s %7s\n", "target", "task", "estimate", "actual", "ratio")
	for _, s := range samples {
		fmt.Fprintf(&b, "%-44s %-22s %9d %9d %7.2f\n", s.Target, s.Task, s.Estimate, s.Actual, s.Ratio())
	}
	return b.String()
}
