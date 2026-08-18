package main

import (
	"strings"
	"testing"
	"time"

	"github.com/switchboard-code/switchboard/internal/agent"
	"github.com/switchboard-code/switchboard/internal/breakpoint"
	"github.com/switchboard-code/switchboard/internal/cachestate"
	"github.com/switchboard-code/switchboard/internal/provider"
)

// The note prices only what was actually observed: a written prefix on a
// dollar-metered target with a cache discount gets the modeled number; an
// unobserved cache, or one on a rung that bills no dollars, gets silence,
// because a number nobody measured is an argument, not a note.
func TestAbandonedCacheNotePricesOnlyWhatWasHeld(t *testing.T) {
	cat, priced := pricedTarget(t)
	info, _, _ := cat.Lookup(priced)
	if len(info.Cache.TTLs) == 0 {
		t.Skip("the fixture target advertises no cache TTL to model against")
	}

	cache := &agent.Cache{
		Manager: &breakpoint.Manager{Policy: info.Cache, Target: priced.ID()},
		Tracker: cachestate.New(),
		Policy:  info.Cache,
		Target:  priced.ID(),
	}
	if note := abandonedCacheNote(cache, cat, time.Now()); note != "" {
		t.Fatalf("an unobserved cache was priced: %q", note)
	}

	// One reported write: the tracker now believes, from the observation
	// alone, the same evidence the loop would have fed it.
	cache.Tracker.Observe(cachestate.Observation{
		Target:     priced.ID(),
		PrefixHash: "abc123",
		Usage:      provider.Usage{CacheWriteTokens: 50_000},
		At:         time.Now(),
		Accounting: info.Cache.UsageAccounting,
		Eligible:   true,
		MinimumTTL: 5 * time.Minute,
	})

	note := abandonedCacheNote(cache, cat, time.Now())
	if note == "" {
		t.Fatal("a written 50k prefix on a priced target modeled to nothing")
	}
	for _, want := range []string{"50k", "modeled", "hit chance", "$"} {
		if !strings.Contains(note, want) {
			t.Fatalf("note missing %q: %q", want, note)
		}
	}

	parameterized := priced
	parameterized.Params.Reasoning = &provider.Reasoning{Enabled: true, Effort: "high"}
	parameterizedCache := &agent.Cache{
		Manager: &breakpoint.Manager{Policy: info.Cache, Target: parameterized.ID()},
		Tracker: cachestate.New(),
		Policy:  info.Cache,
		Target:  parameterized.ID(),
	}
	parameterizedCache.Tracker.Observe(cachestate.Observation{
		Target: parameterized.ID(), PrefixHash: "parameterized", At: time.Now(),
		Usage: provider.Usage{CacheWriteTokens: 50_000}, Accounting: info.Cache.UsageAccounting,
		Eligible: true, MinimumTTL: 5 * time.Minute,
	})
	parameterizedNote := abandonedCacheNote(parameterizedCache, cat, time.Now())
	if !strings.Contains(parameterizedNote, parameterized.ModelID) || !strings.Contains(parameterizedNote, "think:high") || strings.Contains(parameterizedNote, "rt2:") {
		t.Fatalf("parameterized cache note is not readable: %q", parameterizedNote)
	}
	if note := abandonedCacheNote(nil, cat, time.Now()); note != "" {
		t.Fatal("a nil cache produced a note")
	}
}
