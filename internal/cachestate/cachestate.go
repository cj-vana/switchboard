// Package cachestate tracks what a provider's cache has actually been observed
// to do, per session, per route target and prefix lineage.
//
// §6.3 turns on one rule: entries are updated from response usage rather than
// assumed from how a request was built. Sending a marker is not evidence that
// anything was cached. A marker below a target's minimum is accepted and does
// nothing; a prefix can be evicted early; a target can report nothing at all.
// Every one of those looks identical from the request side, which is why the
// request side is not consulted here.
//
// The second rule is that a write observation and a read observation are
// different facts. Having written a prefix says the server had it once, not
// that it still does, and a cost model that conflates the two prices a miss as
// a hit.
//
// Retention is modelled rather than known. Providers describe a TTL as a
// minimum or a best effort, so this reports a probability that decays past the
// stated window instead of asserting an expiry it cannot see.
package cachestate

import (
	"fmt"
	"math"
	"sort"
	"sync"
	"time"

	"github.com/cj-vana/switchboard/internal/catalog"
	"github.com/cj-vana/switchboard/internal/provider"
)

// State is what has been seen for one prefix on one target.
type State string

const (
	// Unknown means nothing has been observed. It is also what a target that
	// reports no cache accounting stays at forever, because silence is not
	// evidence of a miss.
	Unknown State = "unknown"

	// WriteObserved means the provider reported writing this prefix. It had it
	// at that moment; whether it still does is a question of retention.
	WriteObserved State = "write observed"

	// ReadObserved means the provider reported reading it back, which is the
	// only direct evidence a cache is working.
	ReadObserved State = "read observed"

	// Missed means the prefix was eligible, had been written, and came back
	// with no read. This is the state worth an alarm.
	Missed State = "missed"
)

// Entry is the tracked state of one prefix on one target.
type Entry struct {
	Target     provider.RouteTargetID
	PrefixHash string

	Tokens        int
	State         State
	LastWriteSeen time.Time
	LastReadSeen  time.Time

	// MinimumTTL is what the target advertises, treated as a floor rather than
	// a deadline.
	MinimumTTL time.Duration

	// CatalogRevision pins the entry to the data that described the target when
	// it was recorded, so a later reading of it is reproducible (§4).
	CatalogRevision string

	// EligibleRequests and Hits are counted per prefix, because a hit rate over
	// requests that were never eligible measures nothing.
	EligibleRequests int
	Hits             int
	ConsecutiveMiss  int
}

// Observation is one turn's reported usage, plus enough context to know whether
// a zero was the correct answer.
type Observation struct {
	Target     provider.RouteTargetID
	PrefixHash string
	Usage      provider.Usage
	At         time.Time

	// Accounting says whether the target reports cache activity at all. Without
	// it a silent target looks like a permanent miss.
	Accounting catalog.UsageAccounting

	// Eligible is whether this request could have hit: markers were placed, or
	// the target caches automatically. A prefix below the minimum, or a target
	// with no cache, is not eligible and its zero is correct.
	Eligible bool

	MinimumTTL      time.Duration
	CatalogRevision string
}

// Tracker holds entries for a session.
type Tracker struct {
	mu      sync.Mutex
	entries map[string]*Entry

	// AlarmAfter is how many consecutive eligible misses on a prefix that was
	// written are worth surfacing. One miss is ordinary: an entry can be
	// evicted early, and a provider's TTL is a floor rather than a promise.
	// Repetition is what distinguishes bad luck from a cache that is not
	// working.
	AlarmAfter int
}

// DefaultAlarmAfter is deliberately not one. A single miss on a written prefix
// is expected behaviour under early eviction; treating it as a fault would
// teach the reader to ignore the warning.
const DefaultAlarmAfter = 3

func New() *Tracker {
	return &Tracker{entries: map[string]*Entry{}, AlarmAfter: DefaultAlarmAfter}
}

func key(target provider.RouteTargetID, prefixHash string) string {
	return string(target) + "\x00" + prefixHash
}

// Observe folds one turn's reported usage into the tracked state.
func (t *Tracker) Observe(obs Observation) Entry {
	t.mu.Lock()
	defer t.mu.Unlock()

	k := key(obs.Target, obs.PrefixHash)
	entry, ok := t.entries[k]
	if !ok {
		entry = &Entry{
			Target:     obs.Target,
			PrefixHash: obs.PrefixHash,
			State:      Unknown,
		}
		t.entries[k] = entry
	}
	entry.MinimumTTL = obs.MinimumTTL
	entry.CatalogRevision = obs.CatalogRevision

	if obs.Accounting == catalog.AccountingNone {
		// The target reports nothing, so this turn carries no information about
		// its cache. Recording a miss here would manufacture evidence, and the
		// alarm would fire forever on a target that may well be caching.
		return *entry
	}

	switch {
	case obs.Usage.CacheReadTokens > 0:
		entry.State = ReadObserved
		entry.LastReadSeen = obs.At
		entry.Tokens = max(entry.Tokens, obs.Usage.CacheReadTokens)
		entry.Hits++
		entry.ConsecutiveMiss = 0

	case obs.Usage.CacheWriteTokens > 0:
		// A write is evidence the server took the prefix, and nothing about
		// whether it will still have it next time.
		entry.State = WriteObserved
		entry.LastWriteSeen = obs.At
		entry.Tokens = max(entry.Tokens, obs.Usage.CacheWriteTokens)
		entry.ConsecutiveMiss = 0

	default:
		// Nothing read and nothing written. That is only a miss if this prefix
		// had been written before and the request could have hit.
		if obs.Eligible && !entry.LastWriteSeen.IsZero() {
			entry.State = Missed
			entry.ConsecutiveMiss++
		}
	}

	if obs.Eligible {
		entry.EligibleRequests++
	}
	return *entry
}

// Expectation is what the tracker believes before a request is sent.
//
// HitProbability is a belief and is named as one. Nothing here can see server
// state, so the honest output is a number that decays with age rather than a
// claim about what is cached.
type Expectation struct {
	State          State
	HitProbability float64
	Age            time.Duration

	// Reason explains the probability in the terms a reader can act on, which
	// matters most when it is zero: a new prefix and a dead cache both score
	// zero and mean opposite things.
	Reason string
}

// Expect reports the belief about a prefix on a target at a moment.
func (t *Tracker) Expect(target provider.RouteTargetID, prefixHash string, now time.Time) Expectation {
	t.mu.Lock()
	defer t.mu.Unlock()

	entry, ok := t.entries[key(target, prefixHash)]
	if !ok {
		return Expectation{
			State:  Unknown,
			Reason: "this prefix has not been sent to this target, so a miss is the correct outcome",
		}
	}

	seen := entry.LastReadSeen
	if entry.LastWriteSeen.After(seen) {
		seen = entry.LastWriteSeen
	}
	if seen.IsZero() {
		return Expectation{
			State:  entry.State,
			Reason: "nothing has been observed for this prefix",
		}
	}

	age := now.Sub(seen)
	return Expectation{
		State:          entry.State,
		Age:            age,
		HitProbability: survival(age, entry.MinimumTTL),
		Reason: fmt.Sprintf("last seen %s ago against a stated minimum retention of %s",
			age.Truncate(time.Second), entry.MinimumTTL),
	}
}

// survival models retention past the advertised window.
//
// Inside the stated TTL the probability is high but not one, because entries
// are evicted early under load and the number is a minimum rather than a
// guarantee. Past it the belief decays smoothly instead of falling to zero at a
// cliff the tracker cannot actually observe.
func survival(age, ttl time.Duration) float64 {
	if age < 0 {
		age = 0
	}
	if ttl <= 0 {
		return 0
	}
	if age <= ttl {
		return 0.95
	}
	// Halve the belief for every further window elapsed.
	elapsed := float64(age-ttl) / float64(ttl)
	return 0.95 * math.Pow(0.5, elapsed+1)
}

// Health summarizes one target, for the per-turn instrumentation §6.6 asks for.
type Health struct {
	Target provider.RouteTargetID

	EligibleRequests int
	Hits             int

	// Alarm is set only when a prefix that was written kept coming back
	// uncached while eligible. A near-zero hit rate is the correct result for a
	// new prefix, a sub-minimum prefix, or an expired entry, and reporting
	// those as faults would make the signal useless.
	Alarm  bool
	Detail string
}

// HitRate is over eligible requests only. Including ineligible ones would
// measure how often caching was possible rather than how often it worked.
func (h Health) HitRate() float64 {
	if h.EligibleRequests == 0 {
		return 0
	}
	return float64(h.Hits) / float64(h.EligibleRequests)
}

func (t *Tracker) Health(target provider.RouteTargetID) Health {
	t.mu.Lock()
	defer t.mu.Unlock()

	health := Health{Target: target}
	var worst *Entry

	for _, entry := range t.entries {
		if entry.Target != target {
			continue
		}
		health.EligibleRequests += entry.EligibleRequests
		health.Hits += entry.Hits
		if worst == nil || entry.ConsecutiveMiss > worst.ConsecutiveMiss {
			worst = entry
		}
	}

	alarmAfter := t.AlarmAfter
	if alarmAfter <= 0 {
		alarmAfter = DefaultAlarmAfter
	}
	if worst != nil && worst.ConsecutiveMiss >= alarmAfter {
		health.Alarm = true
		health.Detail = fmt.Sprintf(
			"a prefix this target reported writing has come back uncached %d times in a row while eligible; "+
				"the estimator is pricing reads that are not happening",
			worst.ConsecutiveMiss)
	}
	return health
}

// Entries returns a stable snapshot, newest activity first, for reporting.
func (t *Tracker) Entries() []Entry {
	t.mu.Lock()
	defer t.mu.Unlock()

	out := make([]Entry, 0, len(t.entries))
	for _, entry := range t.entries {
		out = append(out, *entry)
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := lastSeen(out[i]), lastSeen(out[j])
		if !a.Equal(b) {
			return a.After(b)
		}
		return out[i].PrefixHash < out[j].PrefixHash
	})
	return out
}

func lastSeen(e Entry) time.Time {
	if e.LastReadSeen.After(e.LastWriteSeen) {
		return e.LastReadSeen
	}
	return e.LastWriteSeen
}
