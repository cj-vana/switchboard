package main

// The price of walking away. A target switch abandons whatever the old
// target was holding warm; this file puts the modeled number on it that
// "not modelled yet" promised. Every input is something actually held:
// the token count is what the provider reported writing or reading, the
// hit probability is the tracker's decaying belief, and the rates are the
// old target's own catalog bands. The output says modeled, because it is.

import (
	"fmt"
	"time"

	"github.com/switchboard-code/switchboard/internal/agent"
	"github.com/switchboard-code/switchboard/internal/catalog"
	"github.com/switchboard-code/switchboard/internal/provider"
)

// abandonedCacheNote prices what a switch leaves behind on the old target,
// or returns "" when there is nothing honest to price: an unobserved cache,
// a target that does not bill dollars, a discount its bands do not offer,
// or a value so small the rendering would round to the free money this
// codebase refuses to print.
func abandonedCacheNote(oldCache *agent.Cache, cat *catalog.Catalog, now time.Time) string {
	if oldCache == nil || oldCache.Tracker == nil {
		return ""
	}
	// The newest observed entry is what was warm - observed, not planned:
	// a prefix the provider reported holding, whether or not another
	// request was assembled since.
	entries := oldCache.Snapshot()
	if len(entries) == 0 || entries[0].Tokens <= 0 {
		return ""
	}
	tokens := entries[0].Tokens
	exp := oldCache.Tracker.Expect(oldCache.Target, entries[0].PrefixHash, now)
	if exp.HitProbability <= 0 {
		return ""
	}

	target, err := parseRecordedTarget(string(oldCache.Target))
	if err != nil {
		return ""
	}
	info, _, found := cat.Lookup(target)
	// Local and plan meterings bill no dollars, and an absent metering is
	// per-token by the catalog's own convention - the same arms every
	// pricing site here switches on.
	if !found || info.Metering == catalog.Local || info.Metering == catalog.Plan || info.Free() {
		return ""
	}
	cold, _, okCold := info.Cost(provider.Usage{InputTokens: tokens})
	warm, _, okWarm := info.Cost(provider.Usage{CacheReadTokens: tokens})
	if !okCold || !okWarm || cold <= warm {
		return ""
	}
	value := catalog.Money(float64(cold-warm) * exp.HitProbability)
	if value < 1000 { // under a tenth of a cent, the honest rendering is silence
		return ""
	}
	return fmt.Sprintf(
		"switching leaves %s warm prefix tokens behind on %s; against re-sending them cold there, that warmth's modeled value is ~%s (hit chance ~%.0f%%)",
		compact(tokens), provider.DisplayRouteTargetID(oldCache.Target), value, exp.HitProbability*100)
}
