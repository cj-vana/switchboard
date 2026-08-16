package main

import (
	"strings"
	"testing"

	"github.com/cj-vana/switchboard/internal/catalog"
	"github.com/cj-vana/switchboard/internal/config"
	"github.com/cj-vana/switchboard/internal/provider"
)

// The bundled catalog is the fixture: the gate's behavior is asserted, not
// its exact dollars, so a price revision does not break these.
func pricedTarget(t *testing.T) (*catalog.Catalog, provider.RouteTarget) {
	t.Helper()
	cat, err := catalog.LoadBundled()
	if err != nil {
		t.Fatal(err)
	}
	target := provider.RouteTarget{Provider: "anthropic", Surface: "first-party", ModelID: "claude-opus-5"}
	if _, _, ok := cat.Lookup(target); !ok {
		t.Fatal("the bundled catalog no longer prices claude-opus-5; pick another fixture target")
	}
	return cat, target
}

func TestPreflightBoundIsAWorstCase(t *testing.T) {
	cat, target := pricedTarget(t)
	info, _, _ := cat.Lookup(target)

	small := preflightBound(info, 1_000)
	large := preflightBound(info, 100_000)
	if small <= 0 {
		t.Fatal("a priced target must produce a positive bound")
	}
	if large <= small {
		t.Errorf("bound did not grow with the prompt: %s then %s", small, large)
	}
}

func TestBudgetGateRefusesAndClears(t *testing.T) {
	cat, target := pricedTarget(t)
	bs := &budgetState{}
	spent := catalog.Money(0)
	gate := budgetGate(bs, cat,
		func() provider.RouteTarget { return target },
		func() catalog.Money { return spent })

	if err := gate(50_000); err != nil {
		t.Fatalf("no ceiling set, but the gate refused: %v", err)
	}

	bs.set(1 * catalog.MicroUSD)
	err := gate(50_000)
	if err == nil {
		t.Fatal("a one-micro-dollar ceiling let a 50k-token call through")
	}
	if !strings.Contains(err.Error(), "/budget") {
		t.Errorf("refusal %q does not say how to raise the ceiling", err)
	}

	bs.set(1_000 * catalog.USD)
	if err := gate(50_000); err != nil {
		t.Errorf("a thousand-dollar ceiling refused a single call: %v", err)
	}

	// Spend eats the headroom: the same ceiling refuses once spent nears it.
	bs.set(2 * catalog.USD)
	spent = 2 * catalog.USD
	if err := gate(1_000); err == nil {
		t.Error("a session at its ceiling was allowed another call")
	}
}

func TestBudgetGatePassesUnpricedTargets(t *testing.T) {
	cat, _ := pricedTarget(t)
	bs := &budgetState{}
	bs.set(1 * catalog.MicroUSD)
	gate := budgetGate(bs, cat,
		func() provider.RouteTarget {
			return provider.RouteTarget{Provider: "nobody", Surface: "nowhere", ModelID: "unknown"}
		},
		func() catalog.Money { return 0 })
	if err := gate(1_000_000); err != nil {
		t.Errorf("a ceiling cannot govern what has no price, but the gate refused: %v", err)
	}
}

func TestBudgetBlocksMoveNamesTheReason(t *testing.T) {
	cat, target := pricedTarget(t)
	bs := &budgetState{}
	dest := config.Tier{ID: "t3", Target: target}

	if _, blocked := budgetBlocksMove(bs, cat, dest, 0, 50_000); blocked {
		t.Fatal("no ceiling, but the move was blocked")
	}

	bs.set(1 * catalog.MicroUSD)
	reason, blocked := budgetBlocksMove(bs, cat, dest, 0, 50_000)
	if !blocked {
		t.Fatal("a one-micro-dollar ceiling allowed an escalation onto a priced rung")
	}
	if !strings.Contains(reason, "t3") || !strings.Contains(reason, "ceiling") {
		t.Errorf("reason %q does not name the rung and the ceiling", reason)
	}
}
