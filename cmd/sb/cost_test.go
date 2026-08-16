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
