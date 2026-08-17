package main

import (
	"strings"
	"testing"

	"github.com/cj-vana/switchboard/internal/config"
	"github.com/cj-vana/switchboard/internal/provider"
	"github.com/cj-vana/switchboard/internal/session"
)

// The lifetime receipt reads every log the workspace recorded, sums what
// actually ran, and prices the whole history on each rung with the same
// honesty rules as /cost rungs: meterings apart, cold counterfactuals, no
// $0.00 for a rung that has no price.
func TestStatsReadsEverySessionAndKeepsTheMeteringsApart(t *testing.T) {
	cat, priced := pricedTarget(t)
	local := provider.RouteTarget{Provider: "ollama", Surface: "local", ModelID: "qwen3:4b"}
	tiers := []config.Tier{
		{ID: "t1", Label: "light", Target: local},
		{ID: "t2", Label: "paid", Target: priced},
	}

	store, err := session.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	workspace := t.TempDir()
	for _, usage := range []session.Usage{
		{Target: string(local.ID()), Usage: provider.Usage{InputTokens: 1_000, OutputTokens: 200}},
		{Target: string(priced.ID()), Usage: provider.Usage{InputTokens: 500, CacheReadTokens: 9_500, OutputTokens: 300}, CostMicroUSD: 120_000},
	} {
		sess, err := store.Create(workspace, provider.RouteTargetID(usage.Target), "test")
		if err != nil {
			t.Fatal(err)
		}
		if err := sess.AppendUsage(usage); err != nil {
			t.Fatal(err)
		}
		sess.Close()
	}

	out := strings.Join(statsLines(tiers, cat, "t1", store, workspace), "\n")

	if !strings.Contains(out, "2 sessions, 2 model calls") {
		t.Errorf("the header does not count the history:\n%s", out)
	}
	if !strings.Contains(out, "local — nothing to bill") {
		t.Errorf("the local rung lost its metering word:\n%s", out)
	}
	if !strings.Contains(out, "as routed") {
		t.Errorf("the receipt is missing its as-routed half:\n%s", out)
	}
	if !strings.Contains(out, "no cache assumed") {
		t.Errorf("the cold-pricing assumption is not stated:\n%s", out)
	}
	if !strings.Contains(out, "subagent") {
		t.Errorf("the scope is not stated:\n%s", out)
	}
	if strings.Contains(out, "$0.00") {
		t.Errorf("something rendered as free money:\n%s", out)
	}
}

func TestStatsWithNoHistorySaysSo(t *testing.T) {
	store, err := session.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	out := strings.Join(statsLines(nil, nil, "", store, t.TempDir()), "\n")
	if !strings.Contains(out, "no sessions recorded") {
		t.Errorf("an empty history did not say so: %s", out)
	}
}

// The all-form spans workspaces from the logs' own headers - the store's
// directory names are hashes and never held the answer - and keeps rung
// repricing per workspace, where a counterfactual means something.
func TestStatsAllSpansWorkspaces(t *testing.T) {
	store, err := session.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cat, priced := pricedTarget(t)
	wsA, wsB := t.TempDir(), t.TempDir()
	for _, ws := range []string{wsA, wsB} {
		sess, err := store.Create(ws, priced.ID(), "test")
		if err != nil {
			t.Fatal(err)
		}
		sess.AppendUsage(session.Usage{Target: string(priced.ID()),
			Usage: provider.Usage{InputTokens: 1000, OutputTokens: 100}, CostMicroUSD: 5000})
		sess.Close()
	}

	out := strings.Join(statsAllLines(cat, store), "\n")
	for _, want := range []string{wsA, wsB, "across them: 2 sessions, 2 calls", "rung repricing stays per workspace"} {
		if !strings.Contains(out, want) {
			t.Fatalf("all-form missing %q:\n%s", want, out)
		}
	}
}
