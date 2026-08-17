package main

import (
	"strings"
	"testing"
	"time"

	"github.com/cj-vana/switchboard/internal/catalog"
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
