package main

import (
	"strings"
	"testing"
	"time"

	"github.com/switchboard-code/switchboard/internal/catalog"
	"github.com/switchboard-code/switchboard/internal/provider"
)

// The cache surface keeps the tracker's honesty: a cache-unaware loop says
// so, an unsent session's correct expectation is a miss, and a surface that
// reports no accounting is unknown rather than failing.
func TestCacheLinesKeepTheTrackersHonesty(t *testing.T) {
	if out := strings.Join(cacheLines(nil, catalog.CachePolicy{}, time.Now()), "\n"); !strings.Contains(out, "cache-unaware") {
		t.Fatalf("a nil cache did not say the loop runs cache-unaware: %s", out)
	}

	cat, priced := pricedTarget(t)
	info, _, _ := cat.Lookup(priced)
	cache := cacheFor(priced, cat)
	out := strings.Join(cacheLines(cache, info.Cache, time.Now()), "\n")
	if !strings.Contains(out, "nothing has been sent yet") {
		t.Fatalf("an unsent session did not state the correct miss:\n%s", out)
	}
	if !strings.Contains(out, "floor, not a promise") {
		t.Fatalf("retention is not stated as a floor:\n%s", out)
	}

	silent := catalog.CachePolicy{DefaultMode: catalog.CacheAutomatic, UsageAccounting: catalog.AccountingNone}
	out = strings.Join(cacheLines(cache, silent, time.Now()), "\n")
	if !strings.Contains(out, "silence is not evidence of a miss") {
		t.Fatalf("a silent surface's unknowability is not stated:\n%s", out)
	}
}

func TestCacheSetPreservesPerTargetTrackersAcrossSwitches(t *testing.T) {
	cat, first := pricedTarget(t)
	second := first
	second.ModelID = "claude-sonnet-5"
	initial := cacheFor(first, cat)
	set := newCacheSet(first, initial)

	away := set.For(second, cat)
	back := set.For(first, cat)
	if away == nil {
		t.Fatal("second priced target did not receive a cache controller")
	}
	if back != initial {
		t.Fatal("switching away and back discarded the first target's tracker")
	}
	if set.For(second, cat) != away {
		t.Fatal("second target's tracker was not stable")
	}
}

func TestCacheSetTreatsUnknownTargetWarmthAsUnknown(t *testing.T) {
	cat, first := pricedTarget(t)
	set := newCacheSet(first, cacheFor(first, cat))
	unknown := provider.RouteTarget{Provider: "custom", Surface: "local", ModelID: "unlisted"}
	if got := set.HitProbability(unknown, cat, provider.Request{}); got != 0 {
		t.Fatalf("unknown target hit probability = %v, want 0", got)
	}
}

func TestCacheSetSeparatesExplicitInferenceParameters(t *testing.T) {
	cat, base := pricedTarget(t)
	baseCache := cacheFor(base, cat)
	set := newCacheSet(base, baseCache)

	withMax := base
	withMax.Params.MaxOutputTokens = 2_048
	temperature := 0.2
	withTemperature := base
	withTemperature.Params.Temperature = &temperature

	maxCache := set.For(withMax, cat)
	temperatureCache := set.For(withTemperature, cat)
	if maxCache == nil || temperatureCache == nil {
		t.Fatal("priced parameter variants did not receive cache controllers")
	}
	if maxCache == baseCache || temperatureCache == baseCache || maxCache == temperatureCache {
		t.Fatal("targets with different wire parameters shared cache warmth")
	}
	if set.For(withMax, cat) != maxCache || set.For(withTemperature, cat) != temperatureCache {
		t.Fatal("parameter-specific cache identity was not stable")
	}
}
