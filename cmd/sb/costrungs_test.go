package main

import (
	"strings"
	"testing"

	"github.com/switchboard-code/switchboard/internal/catalog"
	"github.com/switchboard-code/switchboard/internal/config"
	"github.com/switchboard-code/switchboard/internal/provider"
	"github.com/switchboard-code/switchboard/internal/session"
)

func TestCostRungsPricesColdAndKeepsTheMeteringsApart(t *testing.T) {
	cat, priced := pricedTarget(t)
	local := provider.RouteTarget{Provider: "ollama", Surface: "local", ModelID: "qwen3:4b"}
	unknown := provider.RouteTarget{Provider: "nobody", Surface: "nowhere", ModelID: "made-up"}

	tiers := []config.Tier{
		{ID: "t1", Label: "light", Target: local},
		{ID: "t2", Label: "paid", Target: priced},
		{ID: "t3", Label: "ghost", Target: unknown},
	}
	usages := []session.Usage{
		{Target: string(local.ID()), Usage: provider.Usage{InputTokens: 1_000, OutputTokens: 200}},
		{Target: string(priced.ID()), Usage: provider.Usage{InputTokens: 500, CacheReadTokens: 9_500, OutputTokens: 300}, CostMicroUSD: 120_000},
	}

	out := strings.Join(costRungsLines(tiers, cat, "t1", usages), "\n")

	if !strings.Contains(out, "local — nothing to bill") {
		t.Errorf("the local rung was not reported as local:\n%s", out)
	}
	if !strings.Contains(out, "no catalog entry, so no price") {
		t.Errorf("the unknown rung was not reported as unpriced:\n%s", out)
	}
	if strings.Contains(out, "$0.00") {
		t.Errorf("an unpriced or local rung rendered as free money:\n%s", out)
	}
	if !strings.Contains(out, "no cache assumed") {
		t.Errorf("the cold-pricing assumption is not stated:\n%s", out)
	}
	if !strings.Contains(out, "as routed") {
		t.Errorf("the receipt is missing its as-routed half:\n%s", out)
	}
	if !strings.Contains(out, catalog.Money(120_000).String()+" across the 1 calls that bill dollars") {
		t.Errorf("as-routed did not report the recorded spend:\n%s", out)
	}

	// The priced rung's counterfactual must bill the cache reads as ordinary
	// input: 10,000 input tokens and 500 output across two calls, at the
	// entry's input rate, which is strictly more than the same tokens priced
	// as cache reads would be.
	info, _, _ := cat.Lookup(priced)
	var want catalog.Money
	for _, u := range usages {
		cost, _, ok := info.Cost(provider.Usage{
			InputTokens:  u.Usage.InputTokens + u.Usage.CacheReadTokens + u.Usage.CacheWriteTokens,
			OutputTokens: u.Usage.OutputTokens,
		})
		if !ok {
			t.Fatal("fixture calls must fit the fixture target's bands")
		}
		want += cost
	}
	if !strings.Contains(out, want.String()) {
		t.Errorf("counterfactual for the priced rung should be %s:\n%s", want, out)
	}
}

func TestCostRungsRefusesAPartialSum(t *testing.T) {
	cat, priced := pricedTarget(t)
	tiers := []config.Tier{{ID: "t2", Label: "paid", Target: priced}}

	info, _, _ := cat.Lookup(priced)
	if info.ContextWindow == 0 {
		t.Fatal("the fixture target records no context window; pick another fixture target")
	}

	// One call fits, one could not have: its input exceeds the rung's context
	// window. A rung that could not have taken every call has no
	// counterfactual price — feasibility before economics — and a sum over
	// the calls that fit would price a session that never existed.
	usages := []session.Usage{
		{Target: string(priced.ID()), Usage: provider.Usage{InputTokens: 1_000, OutputTokens: 100}, CostMicroUSD: 30_000},
		{Target: string(priced.ID()), Usage: provider.Usage{InputTokens: info.ContextWindow + 1, OutputTokens: 100}, CostMicroUSD: 90_000},
	}

	lines := costRungsLines(tiers, cat, "t2", usages)
	out := strings.Join(lines, "\n")
	if !strings.Contains(out, "would not fit this rung's context window") {
		t.Errorf("a call the rung could not have held must leave it unpriced:\n%s", out)
	}
	for _, line := range lines {
		if strings.Contains(line, "t2") && strings.Contains(line, "$") {
			t.Errorf("no dollar figure may render for a rung that could not take the session: %q", line)
		}
	}
}

func TestAsRoutedNeverRendersZeroDollars(t *testing.T) {
	cat, priced := pricedTarget(t)
	// A call priced when its target had no catalog entry records zero cost;
	// read back now that the entry exists, it must not render as $0.00.
	usages := []session.Usage{
		{Target: string(priced.ID()), Usage: provider.Usage{InputTokens: 1_000, OutputTokens: 100}},
	}
	line := asRoutedLine(cat, usages)
	if strings.Contains(line, "$0.00") {
		t.Fatalf("a recorded zero rendered as free money: %q", line)
	}
	if !strings.Contains(line, "no cost was recorded") {
		t.Fatalf("the zero case should say what it is: %q", line)
	}
}

func TestCostRungsWithNothingRecorded(t *testing.T) {
	cat, priced := pricedTarget(t)
	tiers := []config.Tier{{ID: "t2", Target: priced}}
	out := strings.Join(costRungsLines(tiers, cat, "t2", nil), "\n")
	if !strings.Contains(out, "nothing has been priced yet") {
		t.Errorf("an empty session should say so:\n%s", out)
	}
}
