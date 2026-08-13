package main

import (
	"fmt"

	"github.com/cjvana/switchboard/internal/catalog"
	"github.com/cjvana/switchboard/internal/config"
	"github.com/cjvana/switchboard/internal/costmodel"
	"github.com/cjvana/switchboard/internal/prefix"
	route "github.com/cjvana/switchboard/internal/router"
)

// candidatesFor turns the configured ladder into what the router scores.
//
// Every tier becomes a candidate, including ones that will be excluded: the
// router reports why each was ruled out, and a user who expected a target needs
// to see the reason rather than its absence.
func candidatesFor(cfg *config.Config, cat *catalog.Catalog, layout *prefix.Layout, hitProbability float64) []route.Candidate {
	promptTokens := 0
	if layout != nil {
		for _, b := range layout.Boundaries() {
			if b.TokensBefore > promptTokens {
				promptTokens = b.TokensBefore
			}
		}
	}

	out := make([]route.Candidate, 0, len(cfg.Tiers))
	for rank, tier := range cfg.Tiers {
		info, _, ok := cat.Lookup(tier.Target)
		if !ok {
			// No catalog entry means nothing is known about capability or
			// price. It stays a candidate, because refusing to route to a
			// target the user configured would be worse, but with nothing
			// claimed on its behalf.
			info = catalog.ModelInfo{}
		}

		c := route.Candidate{
			Tier:         tier.ID,
			Target:       tier.Target,
			Info:         info,
			Rank:         rank,
			PromptTokens: promptTokens,
		}
		c.Estimate = costmodel.Estimator{}.Turn(costmodel.Inputs{
			Target:       tier.Target,
			Info:         info,
			PrefixTokens: promptTokens,
			// Output is unknown before the turn runs. A flat allowance keeps
			// the comparison between targets honest without pretending to
			// predict length; §6.4's latency model is where this gets real.
			OutputTokens:   512,
			Eligible:       info.Cache.UsageAccounting == catalog.AccountingSeparate,
			HitProbability: hitProbability,
		})
		out = append(out, c)
	}
	return out
}

// describeRoute renders a decision.
//
// §8.1 says Rationale and Source are not diagnostics: design principle 3
// requires that a user can see why a target was chosen, so this is printed
// rather than logged.
func describeRoute(d route.Decision) []string {
	lines := []string{
		fmt.Sprintf("  route      %s via %s (%s)", d.Tier, d.Source, d.Rationale),
	}
	if d.EstimatedCost.Expected > 0 || d.EstimatedCost.High > 0 {
		lines = append(lines, fmt.Sprintf("  estimate   %s, between %s and %s",
			d.EstimatedCost.Expected, d.EstimatedCost.Low, d.EstimatedCost.High))
	}
	for _, why := range d.Infeasible {
		lines = append(lines, "  ruled out  "+why)
	}
	return lines
}
