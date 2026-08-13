// Package costmodel prices a turn before it runs.
//
// §6.4 gives the shape: an expectation over outcomes, not a single number.
// Whether a prefix is warm decides most of what a turn costs, nothing here can
// see server state, and the two failure directions are symmetric. A router that
// ignores cache state burns money while claiming a saving; a router that treats
// server state as certain makes the same mistake pointing the other way. So an
// estimate carries bounds and is reconciled against what actually happened.
//
// The arithmetic runs on canonical usage rather than on raw provider fields.
// §6.4 puts provider_cost in the adapter because usage fields do not share
// subset or overlap semantics across APIs, and that is exactly what the
// adapters already resolve: each one normalises into a provider.Usage whose
// three input counts are disjoint, saying at the point of conversion which
// convention it started from. Once that has happened the arithmetic is uniform,
// and duplicating it per adapter would be four places to get the same sum
// wrong.
package costmodel

import (
	"fmt"
	"time"

	"github.com/cjvana/switchboard/internal/catalog"
	"github.com/cjvana/switchboard/internal/provider"
)

// Token estimate error, measured rather than assumed. docs/estimator.md records
// 0.76 to 0.82 of actual across 18 calls on two targets, never once above 1.0,
// so a count from the estimator is a floor and the correction only ever widens
// upward.
//
// A target that answers exactly needs no correction at all, which is why
// Inputs carries that as a fact rather than this being applied everywhere.
const (
	EstimateLowRatio  = 0.76
	EstimateHighRatio = 0.82
)

// Inputs describe one prospective turn.
type Inputs struct {
	Target provider.RouteTarget
	Info   catalog.ModelInfo

	// PrefixTokens is what a marker covers, and FreshTokens is everything after
	// it: the volatile tail plus whatever history has arrived since. The split
	// is what makes a hit cheaper than a miss.
	PrefixTokens int
	FreshTokens  int

	// OutputTokens is expected output including reasoning, which is billed at
	// the output rate and is frequently the larger half of a short turn.
	OutputTokens int

	// Eligible is whether this turn could hit at all: markers were placed, or
	// the target caches on its own. When false the prefix is billed as ordinary
	// input and the hit probability is not consulted.
	Eligible bool

	// HitProbability comes from the cache tracker's observations. It is a
	// belief about a server nothing here can inspect.
	HitProbability float64

	// TokensAreExact is true when the count came from the provider rather than
	// from the estimator. Only Anthropic answers the question today.
	TokensAreExact bool

	// TTL is the retention the breakpoint manager asked for, because the write
	// rate differs per TTL and per provider.
	TTL time.Duration
}

// Estimate is the priced expectation with the bounds that make it honest.
type Estimate struct {
	// Expected is the probability-weighted cost.
	Expected catalog.Money

	// Low and High bound the outcomes rather than describing a distribution.
	// Low is a hit costed against the smallest plausible token count; High is a
	// miss against the largest. Calling that a confidence interval would imply
	// a model of the provider's eviction policy that nobody here has.
	Low, High catalog.Money

	// HitCost and MissCost are the two outcomes, which is what a router
	// comparing targets actually needs: the spread between them is how much the
	// cache is worth on this target.
	HitCost, MissCost catalog.Money

	HitProbability float64
	Metering       catalog.Metering
	Notes          []string
}

// Spread is what a warm cache saves on this turn. It is the quantity §6.4's
// switch arithmetic values when it asks what a lost prefix would have been
// worth.
func (e Estimate) Spread() catalog.Money { return e.MissCost - e.HitCost }

type Estimator struct{}

// Turn prices a prospective turn.
func (e Estimator) Turn(in Inputs) Estimate {
	est := Estimate{
		HitProbability: in.HitProbability,
		Metering:       in.Info.Metering,
	}
	if !in.Eligible {
		est.HitProbability = 0
	}

	est.HitCost = e.cost(in, hitUsage(in))
	est.MissCost = e.cost(in, missUsage(in))

	p := est.HitProbability
	est.Expected = catalog.Money(float64(est.HitCost)*p + float64(est.MissCost)*(1-p))

	// Bounds widen for the token estimate's measured bias, upward only, because
	// it has never been observed to overcount.
	low, high := in, in
	if !in.TokensAreExact {
		low.PrefixTokens = scale(in.PrefixTokens, 1/EstimateHighRatio)
		low.FreshTokens = scale(in.FreshTokens, 1/EstimateHighRatio)
		high.PrefixTokens = scale(in.PrefixTokens, 1/EstimateLowRatio)
		high.FreshTokens = scale(in.FreshTokens, 1/EstimateLowRatio)
		est.Notes = append(est.Notes, fmt.Sprintf(
			"token counts are estimated and measured %.0f to %.0f percent of actual, so the bounds are widened upward (docs/estimator.md)",
			EstimateLowRatio*100, EstimateHighRatio*100))
	}
	est.Low = e.cost(low, hitUsage(low))
	est.High = e.cost(high, missUsage(high))

	switch in.Info.Metering {
	case catalog.Local:
		est.Notes = append(est.Notes, "runs locally, so nothing here is a cost")
	case catalog.Plan:
		est.Notes = append(est.Notes,
			"billed as a plan, so this is zero and says nothing about the quota it consumes")
	}
	if !in.Eligible {
		est.Notes = append(est.Notes,
			"this turn cannot hit the cache, so the prefix is priced as ordinary input")
	}
	return est
}

// hitUsage is what a warm turn reports: the prefix read back, and only what has
// arrived since paid for.
func hitUsage(in Inputs) provider.Usage {
	if !in.Eligible {
		return missUsage(in)
	}
	return provider.Usage{
		CacheReadTokens:  in.PrefixTokens,
		CacheWriteTokens: in.FreshTokens,
		OutputTokens:     in.OutputTokens,
	}
}

// missUsage is a cold turn. With markers placed the whole prefix is written,
// which bills at the write rate rather than the input rate; without them it is
// ordinary input.
func missUsage(in Inputs) provider.Usage {
	if !in.Eligible {
		return provider.Usage{
			InputTokens:  in.PrefixTokens + in.FreshTokens,
			OutputTokens: in.OutputTokens,
		}
	}
	return provider.Usage{
		CacheWriteTokens: in.PrefixTokens + in.FreshTokens,
		OutputTokens:     in.OutputTokens,
	}
}

func (e Estimator) cost(in Inputs, usage provider.Usage) catalog.Money {
	total, _, ok := in.Info.Cost(usage)
	if !ok {
		return 0
	}
	return total
}

func scale(tokens int, factor float64) int {
	return int(float64(tokens)*factor + 0.5)
}

// Switch is the incremental cost of moving from one target to another.
type Switch struct {
	From, To provider.RouteTargetID

	// Difference is the straightforward part: what the next turn costs on the
	// destination against what it would have cost where it is.
	Difference catalog.Money

	// LostWarmValue is the opportunity cost of leaving a warm prefix behind,
	// and it is probabilistic on purpose. A switch does not evict the source
	// cache: it stays usable until the provider expires it, so charging the
	// whole prior write as a residual would double-count a sunk cost. What is
	// actually at risk is the saving a future warm read would have given,
	// weighted by the chance the entry is gone before the session returns.
	LostWarmValue catalog.Money

	Total catalog.Money
	Notes []string
}

// SwitchCost prices a target change.
//
// returnProbability is how likely the session is to come back to the source,
// and sourceSurvival is the tracker's belief that the source prefix will still
// be warm when it does. Both are beliefs and neither is inferred from having
// written the prefix, which says only that the server had it once.
func (e Estimator) SwitchCost(from, to Inputs, returnProbability, sourceSurvival float64) Switch {
	fromEstimate := e.Turn(from)
	toEstimate := e.Turn(to)

	s := Switch{
		From:       from.Target.ID(),
		To:         to.Target.ID(),
		Difference: toEstimate.Expected - fromEstimate.Expected,
	}

	// What a warm source would have saved, if the session returns and the entry
	// is still there.
	expiry := 1 - clamp(sourceSurvival)
	s.LostWarmValue = catalog.Money(float64(fromEstimate.Spread()) * clamp(returnProbability) * expiry)
	s.Total = s.Difference + s.LostWarmValue

	s.Notes = append(s.Notes, fmt.Sprintf(
		"the source cache is not evicted by switching; %.0f%% chance it has expired before a return, "+
			"against a warm saving of %s", expiry*100, fromEstimate.Spread()))

	if from.Info.Metering != to.Info.Metering {
		// A plan and a metered target are not comparable on money alone, and a
		// router told the difference is negative would move to the plan every
		// time regardless of what it is burning.
		s.Notes = append(s.Notes, fmt.Sprintf(
			"these targets are metered differently (%s against %s), so the difference is not the whole comparison",
			from.Info.Metering.String(), to.Info.Metering.String()))
	}
	return s
}

func clamp(p float64) float64 {
	switch {
	case p < 0:
		return 0
	case p > 1:
		return 1
	default:
		return p
	}
}
