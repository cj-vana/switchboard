package main

import (
	"strings"
	"testing"

	"github.com/cj-vana/switchboard/internal/catalog"
	"github.com/cj-vana/switchboard/internal/provider"
	"github.com/cj-vana/switchboard/internal/session"
)

func TestCostCLIKeepsTheThreeMeteringsApart(t *testing.T) {
	cat, pricedTgt := pricedTarget(t)
	store, err := session.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	workspace := t.TempDir()

	record := func(target provider.RouteTargetID, cost int64) {
		t.Helper()
		sess, err := store.Create(workspace, target, "test")
		if err != nil {
			t.Fatal(err)
		}
		defer sess.Close()
		if err := sess.AppendUsage(session.Usage{
			Usage:        provider.Usage{InputTokens: 1000, OutputTokens: 100},
			CostMicroUSD: cost,
		}); err != nil {
			t.Fatal(err)
		}
	}
	record(pricedTgt.ID(), 420_000) // $0.42 on a dollar-metered target
	record(provider.RouteTarget{Provider: "ollama", Surface: "local", ModelID: "qwen3:4b"}.ID(), 0)

	var b strings.Builder
	if err := runCostCLI(&b, store, cat, workspace); err != nil {
		t.Fatal(err)
	}
	out := b.String()
	for _, want := range []string{"$0.42", "local", "bill dollars", "nothing to bill", "not the provider's invoice"} {
		if !strings.Contains(out, want) {
			t.Errorf("sb cost output missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "$0.00") {
		t.Errorf("a local session was rendered as free money rather than as local:\n%s", out)
	}
}

func TestCostCLISaysSoWhenEmpty(t *testing.T) {
	cat, err := catalog.LoadBundled()
	if err != nil {
		t.Fatal(err)
	}
	store, err := session.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	if err := runCostCLI(&b, store, cat, t.TempDir()); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(b.String(), "no sessions recorded") {
		t.Fatalf("empty workspace output: %s", b.String())
	}
}

// The per-ask receipt: turns ordered by what they billed, each beside its
// own words, with the unbilled meterings folded rather than rendered as
// free money.
func TestCostTurnsOrdersAsksByBill(t *testing.T) {
	turns := []session.TurnCost{
		{Turn: 1, Prompt: "cheap warmup", Calls: 2, Usage: provider.Usage{InputTokens: 900, OutputTokens: 80}},
		{Turn: 2, Prompt: "the expensive refactor", Calls: 6, Usage: provider.Usage{InputTokens: 40_000, OutputTokens: 2_000}, CostMicroUSD: 840_000},
		{Turn: 3, Prompt: "a smaller fix", Calls: 3, Usage: provider.Usage{InputTokens: 9_000, OutputTokens: 400}, CostMicroUSD: 310_000},
	}
	out := strings.Join(costTurnsLines(turns), "\n")

	expensive := strings.Index(out, "the expensive refactor")
	smaller := strings.Index(out, "a smaller fix")
	if expensive < 0 || smaller < 0 || expensive > smaller {
		t.Errorf("the dearest ask should lead:\n%s", out)
	}
	if !strings.Contains(out, "$0.8400") || !strings.Contains(out, "$0.3100") {
		t.Errorf("the bills are missing:\n%s", out)
	}
	if !strings.Contains(out, "1 turn billed nothing") {
		t.Errorf("the unbilled fold is missing:\n%s", out)
	}
	if strings.Contains(out, "$0.00") {
		t.Errorf("something rendered as free money:\n%s", out)
	}
}

// The reader folds usage records onto the turn whose opening they follow.
func TestReadTurnCostsFoldsUsageOntoItsTurn(t *testing.T) {
	store, err := session.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	target := provider.RouteTargetID("ollama/local/qwen3:4b")
	sess, err := store.Create(t.TempDir(), target, "test")
	if err != nil {
		t.Fatal(err)
	}
	if err := sess.AppendMessage(provider.UserText("first ask")); err != nil {
		t.Fatal(err)
	}
	if err := sess.AppendUsage(session.Usage{Target: string(target), Usage: provider.Usage{InputTokens: 100, OutputTokens: 10}}); err != nil {
		t.Fatal(err)
	}
	if err := sess.AppendUsage(session.Usage{Target: string(target), Usage: provider.Usage{InputTokens: 200, OutputTokens: 20}, CostMicroUSD: 5_000}); err != nil {
		t.Fatal(err)
	}
	if err := sess.AppendMessage(provider.UserText("second ask")); err != nil {
		t.Fatal(err)
	}
	if err := sess.AppendUsage(session.Usage{Target: string(target), Usage: provider.Usage{InputTokens: 50, OutputTokens: 5}}); err != nil {
		t.Fatal(err)
	}
	path := sess.Path()
	if err := sess.Close(); err != nil {
		t.Fatal(err)
	}

	turns, err := session.ReadTurnCosts(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(turns) != 2 {
		t.Fatalf("two turns recorded, %d read: %+v", len(turns), turns)
	}
	first := turns[0]
	if first.Calls != 2 || first.Usage.InputTokens != 300 || first.CostMicroUSD != 5_000 || first.Prompt != "first ask" {
		t.Errorf("the first turn's metering drifted: %+v", first)
	}
	if turns[1].Calls != 1 || turns[1].Usage.InputTokens != 50 {
		t.Errorf("the second turn's metering drifted: %+v", turns[1])
	}
}
