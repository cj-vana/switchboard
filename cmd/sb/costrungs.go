package main

// /cost rungs prices the session that already happened against the rungs it
// did not run on. The bet behind this tool is that allocation beats always
// using the best model, and a bet gets measured: this is the receipt, per
// session, in the user's own ladder.
//
// A counterfactual has no provider reports, so it cannot claim a cache. Every
// input token is priced at the rung's ordinary input rate — no reads, no
// writes — and the report says so, because a number that quietly assumed a
// warm cache on a server that never saw the session would be an argument, not
// a measurement. Token counts are the session's own; another model would
// tokenize the same text differently, and that too is stated rather than
// adjusted for, since no correction factor exists that is not a guess.

import (
	"fmt"
	"strings"

	"github.com/cj-vana/switchboard/internal/catalog"
	"github.com/cj-vana/switchboard/internal/config"
	"github.com/cj-vana/switchboard/internal/provider"
	"github.com/cj-vana/switchboard/internal/session"
)

// costRungsLines renders the counterfactual table. Each rung prices every
// recorded call through its own banded catalog entry — a sum of tokens is the
// wrong shape, because bands select by the size of one call — and a rung that
// cannot price a call reports that, never a partial dollar figure.
func costRungsLines(tiers []config.Tier, cat *catalog.Catalog, activeTier string, usages []session.Usage) []string {
	if len(usages) == 0 {
		return []string{"  nothing has been priced yet; the table needs at least one model call"}
	}
	if len(tiers) == 0 {
		return []string{"  no ladder is configured, so there are no rungs to compare"}
	}

	lines := []string{
		"  if every call had gone to one rung — no cache assumed, every input",
		"  token at the rung's ordinary input rate, and this session's token",
		"  counts, though another model would tokenize the same text differently:",
	}

	width := 0
	for _, t := range tiers {
		if w := len(t.String()); w > width {
			width = w
		}
	}
	for _, t := range tiers {
		marker := "   "
		if t.ID == activeTier {
			marker = " * "
		}
		lines = append(lines, fmt.Sprintf(" %s%-*s  %s", marker, width, t.String(), rungWord(cat, t, usages)))
	}

	lines = append(lines, "  "+asRoutedLine(cat, usages))
	lines = append(lines, "  an estimator and reconciliation aid, not the provider's invoice (§15)")
	return lines
}

// rungWord prices one rung's counterfactual, or names why there is no price.
// The three zero-dollar meterings stay three different words here as
// everywhere, and an unpriced rung is never rendered as $0.00.
func rungWord(cat *catalog.Catalog, t config.Tier, usages []session.Usage) string {
	info, _, ok := cat.Lookup(t.Target)
	switch {
	case !ok:
		return "no catalog entry, so no price"
	case info.Metering == catalog.Local:
		return "local — nothing to bill"
	case info.Metering == catalog.Plan:
		return "plan — quota, not dollars"
	case info.Free():
		return "no per-token cost recorded"
	}

	var total catalog.Money
	for _, u := range usages {
		cold := coldUsage(u)
		// Feasibility before economics, here as in the router: a rung whose
		// context window could not have held a call could not have taken the
		// session, and a partial sum over the calls that fit would price a
		// session that never existed.
		if info.ContextWindow != 0 && cold.InputTokens > info.ContextWindow {
			return "no price — a recorded call would not fit this rung's context window"
		}
		cost, _, ok := info.Cost(cold)
		if !ok {
			return "no price — a recorded call is outside this rung's price bands"
		}
		total += cost
	}
	return total.String()
}

// coldUsage flattens a recorded call into the no-cache counterfactual: what
// the provider reported as read or written is input the other rung would have
// been sent cold.
func coldUsage(u session.Usage) provider.Usage {
	return provider.Usage{
		InputTokens:  u.Usage.InputTokens + u.Usage.CacheReadTokens + u.Usage.CacheWriteTokens,
		OutputTokens: u.Usage.OutputTokens,
	}
}

// asRoutedLine is the other half of the receipt: what the ladder actually
// spent, with the caches it actually hit, keeping the meterings apart the way
// `sb cost` does.
func asRoutedLine(cat *catalog.Catalog, usages []session.Usage) string {
	var dollars catalog.Money
	var priced, local, plan, unpriced int
	for _, u := range usages {
		target, err := parseRecordedTarget(u.Target)
		if err != nil {
			unpriced++
			continue
		}
		info, _, ok := cat.Lookup(target)
		switch {
		case !ok:
			unpriced++
		case info.Metering == catalog.Local:
			local++
		case info.Metering == catalog.Plan:
			plan++
		case info.Free():
			// A dollar-metered entry whose prices are all zero has nothing
			// to sum, and counting it as a call that bills dollars would
			// render the $0.00 this file exists to avoid.
			unpriced++
		default:
			priced++
			dollars += catalog.Money(u.CostMicroUSD)
		}
	}

	var parts []string
	switch {
	case priced > 0 && dollars == 0:
		// Reachable: a call priced when its target had no catalog entry
		// records zero, and rendering that as $0.00 would teach the same
		// wrong lesson it does everywhere else.
		parts = append(parts, fmt.Sprintf("no cost was recorded for the %d calls that bill dollars", priced))
	case priced > 0:
		parts = append(parts, fmt.Sprintf("%s across the %d calls that bill dollars, with the caches they actually hit", dollars, priced))
	}
	if local > 0 {
		parts = append(parts, fmt.Sprintf("%d ran locally", local))
	}
	if plan > 0 {
		parts = append(parts, fmt.Sprintf("%d billed a plan", plan))
	}
	if unpriced > 0 {
		parts = append(parts, fmt.Sprintf("%d had no price", unpriced))
	}
	return "as routed: " + strings.Join(parts, "; ")
}
