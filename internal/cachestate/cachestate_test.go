package cachestate

import (
	"strings"
	"testing"
	"time"

	"github.com/switchboard-code/switchboard/internal/catalog"
	"github.com/switchboard-code/switchboard/internal/provider"
)

const target provider.RouteTargetID = "anthropic/first-party/claude-haiku-4-5"

func at(s string) time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		panic(err)
	}
	return t
}

func obs(hash string, usage provider.Usage, when string) Observation {
	return Observation{
		Target:     target,
		PrefixHash: hash,
		Usage:      usage,
		At:         at(when),
		Accounting: catalog.AccountingSeparate,
		Eligible:   true,
		MinimumTTL: 5 * time.Minute,
	}
}

// A write says the server took the prefix. A read says it gave it back. They
// are different facts, and a cost model that conflates them prices a miss as a
// hit.
func TestWriteAndReadAreDistinctObservations(t *testing.T) {
	tr := New()

	wrote := tr.Observe(obs("h1", provider.Usage{CacheWriteTokens: 5000}, "2026-08-13T12:00:00Z"))
	if wrote.State != WriteObserved {
		t.Errorf("state = %q after a write", wrote.State)
	}
	if wrote.Hits != 0 {
		t.Error("a write counted as a hit; having written a prefix is not evidence it was read")
	}

	read := tr.Observe(obs("h1", provider.Usage{CacheReadTokens: 5000}, "2026-08-13T12:01:00Z"))
	if read.State != ReadObserved {
		t.Errorf("state = %q after a read", read.State)
	}
	if read.Hits != 1 {
		t.Errorf("hits = %d after one read", read.Hits)
	}
}

// Nothing is inferred from the request. Sending markers is not evidence that
// anything was cached, which is the whole rule §6.3 turns on.
func TestNothingIsAssumedFromSendingMarkers(t *testing.T) {
	tr := New()

	// An eligible request with markers placed, and a provider that reported no
	// cache activity at all.
	got := tr.Observe(obs("h1", provider.Usage{InputTokens: 5000}, "2026-08-13T12:00:00Z"))

	if got.State == WriteObserved || got.State == ReadObserved {
		t.Errorf("state = %q from a turn that reported no cache activity", got.State)
	}
	if tr.Expect(target, "h1", at("2026-08-13T12:00:01Z")).HitProbability != 0 {
		t.Error("a prefix nothing was observed for was given a nonzero hit probability")
	}
}

// A target that reports nothing about caching cannot be distinguished from one
// that cached nothing. Recording misses for it would manufacture evidence and
// leave the alarm on forever.
func TestSilentTargetIsNotRecordedAsAMiss(t *testing.T) {
	tr := New()
	silent := func(when string) Observation {
		o := obs("h1", provider.Usage{InputTokens: 5000}, when)
		o.Accounting = catalog.AccountingNone
		return o
	}

	// Prime it with a write so a miss would otherwise be recordable.
	tr.Observe(obs("h1", provider.Usage{CacheWriteTokens: 5000}, "2026-08-13T12:00:00Z"))
	for i := range 10 {
		tr.Observe(silent(at("2026-08-13T12:00:00Z").Add(time.Duration(i+1) * time.Minute).Format(time.RFC3339)))
	}

	if health := tr.Health(target); health.Alarm {
		t.Errorf("a target that reports nothing raised a cache alarm: %s", health.Detail)
	}
}

// A zero is the correct answer for a new prefix, a sub-minimum prefix, and a
// target with no cache. Counting those against the hit rate would measure how
// often caching was possible rather than how often it worked.
func TestIneligibleRequestsDoNotCountAgainstTheHitRate(t *testing.T) {
	tr := New()

	for i := range 5 {
		o := obs("short", provider.Usage{InputTokens: 100},
			at("2026-08-13T12:00:00Z").Add(time.Duration(i)*time.Minute).Format(time.RFC3339))
		o.Eligible = false // below the target's minimum, so no marker was placed
		tr.Observe(o)
	}

	health := tr.Health(target)
	if health.EligibleRequests != 0 {
		t.Errorf("eligible requests = %d, want none", health.EligibleRequests)
	}
	if health.Alarm {
		t.Error("a prefix too short to cache raised an alarm")
	}
}

// One miss on a written prefix is ordinary: entries are evicted early and a
// stated TTL is a floor. Repetition is what separates bad luck from a cache
// that is not working.
func TestAlarmNeedsRepeatedMissesNotOne(t *testing.T) {
	tr := New()
	tr.AlarmAfter = 3

	tr.Observe(obs("h1", provider.Usage{CacheWriteTokens: 5000}, "2026-08-13T12:00:00Z"))

	miss := func(minute int) {
		tr.Observe(obs("h1", provider.Usage{InputTokens: 5000},
			at("2026-08-13T12:00:00Z").Add(time.Duration(minute)*time.Minute).Format(time.RFC3339)))
	}

	miss(1)
	if tr.Health(target).Alarm {
		t.Error("one miss raised the alarm; early eviction makes that ordinary")
	}
	miss(2)
	if tr.Health(target).Alarm {
		t.Error("two misses raised the alarm")
	}
	miss(3)

	health := tr.Health(target)
	if !health.Alarm {
		t.Fatal("three consecutive eligible misses on a written prefix did not raise the alarm")
	}
	if !strings.Contains(health.Detail, "not happening") {
		t.Errorf("detail = %q; it has to say what the consequence is", health.Detail)
	}
}

// A read after misses clears the streak: the cache started working again, and
// leaving the alarm latched would train the reader to ignore it.
func TestAReadClearsTheMissStreak(t *testing.T) {
	tr := New()
	tr.AlarmAfter = 2

	tr.Observe(obs("h1", provider.Usage{CacheWriteTokens: 5000}, "2026-08-13T12:00:00Z"))
	tr.Observe(obs("h1", provider.Usage{InputTokens: 5000}, "2026-08-13T12:01:00Z"))
	tr.Observe(obs("h1", provider.Usage{CacheReadTokens: 5000}, "2026-08-13T12:02:00Z"))
	tr.Observe(obs("h1", provider.Usage{InputTokens: 5000}, "2026-08-13T12:03:00Z"))

	if tr.Health(target).Alarm {
		t.Error("the alarm stayed latched after the cache was observed working again")
	}
}

// Retention is a floor, not a deadline. Inside the window the belief is high
// and not certain, because entries are evicted early; past it the belief decays
// rather than falling off a cliff the tracker cannot see.
func TestHitProbabilityDecaysRatherThanExpiring(t *testing.T) {
	tr := New()
	tr.Observe(obs("h1", provider.Usage{CacheWriteTokens: 5000}, "2026-08-13T12:00:00Z"))

	inside := tr.Expect(target, "h1", at("2026-08-13T12:02:00Z"))
	if inside.HitProbability <= 0.5 || inside.HitProbability >= 1 {
		t.Errorf("probability inside the window = %.2f, want high but not certain", inside.HitProbability)
	}

	justPast := tr.Expect(target, "h1", at("2026-08-13T12:06:00Z")).HitProbability
	wellPast := tr.Expect(target, "h1", at("2026-08-13T12:30:00Z")).HitProbability

	if !(justPast < inside.HitProbability) {
		t.Errorf("probability did not fall past the window: %.2f then %.2f", inside.HitProbability, justPast)
	}
	if !(wellPast < justPast) {
		t.Errorf("probability did not keep falling: %.2f then %.2f", justPast, wellPast)
	}
	if wellPast <= 0 {
		t.Error("probability hit zero, which asserts an expiry the tracker cannot observe")
	}
}

// Zero means two opposite things and the reader has to be able to tell them
// apart: a prefix never sent, and one that has stopped being cached.
func TestAnUnknownPrefixExplainsItsZero(t *testing.T) {
	tr := New()

	got := tr.Expect(target, "never-sent", at("2026-08-13T12:00:00Z"))
	if got.State != Unknown || got.HitProbability != 0 {
		t.Errorf("expectation = %+v", got)
	}
	if !strings.Contains(got.Reason, "has not been sent") {
		t.Errorf("reason = %q; a new prefix and a dead cache both score zero", got.Reason)
	}
}

// State is per target and per prefix. Sharing either would attribute one
// target's cache to another, which is the mistake the routing key exists to
// prevent on the provider side.
func TestStateIsPerTargetAndPerPrefix(t *testing.T) {
	tr := New()
	tr.Observe(obs("h1", provider.Usage{CacheReadTokens: 5000}, "2026-08-13T12:00:00Z"))

	other := obs("h1", provider.Usage{InputTokens: 5000}, "2026-08-13T12:00:00Z")
	other.Target = "kimi/coding/k3-256k"
	tr.Observe(other)

	if got := tr.Expect("kimi/coding/k3-256k", "h1", at("2026-08-13T12:00:01Z")); got.State == ReadObserved {
		t.Error("one target's read was attributed to another")
	}
	if got := tr.Expect(target, "h2", at("2026-08-13T12:00:01Z")); got.State != Unknown {
		t.Error("one prefix's state was attributed to another")
	}
}

func TestHitRateIsOverEligibleRequests(t *testing.T) {
	tr := New()

	tr.Observe(obs("h1", provider.Usage{CacheWriteTokens: 5000}, "2026-08-13T12:00:00Z"))
	tr.Observe(obs("h1", provider.Usage{CacheReadTokens: 5000}, "2026-08-13T12:01:00Z"))
	tr.Observe(obs("h1", provider.Usage{CacheReadTokens: 5000}, "2026-08-13T12:02:00Z"))

	ineligible := obs("h1", provider.Usage{InputTokens: 10}, "2026-08-13T12:03:00Z")
	ineligible.Eligible = false
	tr.Observe(ineligible)

	health := tr.Health(target)
	if health.EligibleRequests != 3 || health.Hits != 2 {
		t.Fatalf("health = %+v, want 3 eligible and 2 hits", health)
	}
	if got := health.HitRate(); got < 0.66 || got > 0.67 {
		t.Errorf("hit rate = %.2f, want two thirds", got)
	}
}
