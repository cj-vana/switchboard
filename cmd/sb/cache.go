package main

// /cache: the cache state surface. The whole product bets that knowing what
// a provider is holding warm changes what a turn should cost; this command
// shows the belief itself — what §6.3's tracker has observed for the active
// target, what it expects for the prefix the session would send next, and
// the alarm when a written prefix keeps missing. Every number keeps the
// tracker's own honesty: a probability is modeled, not observed; a target
// that reports no accounting stays unknown, because silence is not a miss.

import (
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/switchboard-code/switchboard/internal/agent"
	"github.com/switchboard-code/switchboard/internal/cachestate"
	"github.com/switchboard-code/switchboard/internal/catalog"
)

func cacheLines(cache *agent.Cache, policy catalog.CachePolicy, now time.Time) []string {
	if cache == nil {
		return []string{"  this target has no catalog entry, so the loop runs cache-unaware: no markers, no tracking"}
	}

	var lines []string
	switch {
	case policy.DefaultMode == catalog.CacheNone || len(policy.Modes) == 0 && policy.DefaultMode == "":
		lines = append(lines, "  this surface does not cache; every token is sent and priced cold")
	case policy.UsageAccounting == catalog.AccountingNone:
		lines = append(lines, fmt.Sprintf("  caching is %s, but the surface reports no cache accounting:", policy.DefaultMode),
			"  state stays unknown, because silence is not evidence of a miss")
	default:
		mode := fmt.Sprintf("  caching is %s", policy.DefaultMode)
		if policy.MinTokens > 0 {
			mode += fmt.Sprintf(", prefixes below %s tokens are not cached", compact(policy.MinTokens))
		}
		if len(policy.TTLs) > 0 {
			mode += fmt.Sprintf(", stated retention %s (a floor, not a promise)", strings.Join(policy.TTLs, "/"))
		}
		lines = append(lines, mode)
	}

	if exp, ok := cache.Expectation(now); ok {
		switch {
		case exp.State == cachestate.Unknown || exp.HitProbability == 0 && exp.Age == 0:
			lines = append(lines, "  next send: "+exp.Reason)
		default:
			lines = append(lines, fmt.Sprintf("  next send: %s; modeled hit chance ~%.0f%% (%s)",
				exp.State, exp.HitProbability*100, exp.Reason))
		}
	} else {
		lines = append(lines, "  next send: nothing has been sent yet, so a miss is the correct outcome")
	}

	if health, ok := cache.SessionHealth(); ok && health.EligibleRequests > 0 {
		lines = append(lines, fmt.Sprintf("  this session: %d of %d eligible requests hit",
			health.Hits, health.EligibleRequests))
		if health.Alarm {
			lines = append(lines, "  △ "+health.Detail)
		}
	}

	entries := cache.Snapshot()
	if len(entries) > 4 {
		entries = entries[:4]
	}
	for _, e := range entries {
		seen := "never seen"
		if last := lastActivity(e); !last.IsZero() {
			seen = fmt.Sprintf("last seen %s ago", now.Sub(last).Truncate(time.Second))
		}
		lines = append(lines, fmt.Sprintf("  prefix %.8s  %s tokens  %s  %s",
			e.PrefixHash, compact(e.Tokens), e.State, seen))
	}
	return lines
}

func lastActivity(e cachestate.Entry) time.Time {
	if e.LastReadSeen.After(e.LastWriteSeen) {
		return e.LastReadSeen
	}
	return e.LastWriteSeen
}

// cmdCache is deliberately not busy-safe: the expectation reads the hash of
// the last planned request, which is the loop goroutine's to write during a
// turn.
func cmdCache(m *tuiModel, _ string) tea.Cmd {
	var policy catalog.CachePolicy
	binding := m.app.loop.Binding()
	if info, _, ok := m.app.catalog.Lookup(binding.Target); ok {
		policy = info.Cache
	}
	header := "cache on " + binding.Target.Display()
	body := cacheLines(binding.Cache, policy, time.Now())
	m.addInfo(header + "\n" + strings.Join(body, "\n"))
	return nil
}
