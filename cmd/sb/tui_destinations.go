package main

// /destinations: which providers this workspace's turns may reach.
//
// The router has always had the check. `Requirements.ApprovedProviders` is
// filtered before economics for the §8.1 reason — a target excluded by policy
// must never be reported as one that was out-priced — and it has its own
// exclusion sentence so /why says "not an approved destination" rather than
// "too expensive". Nothing outside the tests ever set it, so a complete,
// enforced policy sat behind no surface at all. This is that surface.
//
// It is a hard requirement, and that is the whole point. A preference would be
// a policy the escalation detector could talk its way past on a bad turn; a
// requirement excludes the target before cost is considered, on the opening
// route, on a mid-turn move, on a /tN pin, on a retry, and on resume. Anything
// less is a rule with a hole, which for this rule is worse than no rule.
//
// Provider names, not a "local only" switch. Where a server runs is not a fact
// this program can read off a target: an OpenAI-compatible endpoint is a
// laptop or a data centre and the identity says neither. Naming the providers
// is the honest form of the same intent, and the ladder is short enough that
// naming them is not a burden.
//
// The command refuses a policy that would exclude every configured rung. A
// ladder with no reachable rung fails on the next turn with an exclusion for
// each entry, which reads as a broken program rather than as the policy doing
// its job, and the moment to say so is when it is typed.

import (
	"slices"
	"sort"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
)

const destinationsUsage = "/destinations lists the providers this workspace may reach; " +
	"/destinations ollama anthropic restricts it to those; /destinations any removes the restriction"

func cmdDestinations(m *tuiModel, args string) tea.Cmd {
	fields := strings.Fields(strings.ToLower(strings.TrimSpace(args)))
	switch {
	case len(fields) == 0, len(fields) == 1 && fields[0] == "status":
		return noticeCmd("", m.destinationsStanding())
	case len(fields) == 1 && (fields[0] == "any" || fields[0] == "off"):
		return m.setDestinations(nil)
	case fields[0] == "help":
		return noticeCmd("", destinationsUsage)
	}

	wanted := slices.Clone(fields)
	sort.Strings(wanted)
	wanted = slices.Compact(wanted)

	// A policy the ladder cannot satisfy is refused here rather than on the
	// next turn. The router would exclude every rung correctly and the session
	// would look broken, which is the wrong lesson to teach about a rule that
	// is working.
	var reachable []string
	for _, tier := range m.app.config.Tiers {
		if slices.Contains(wanted, tier.Target.Provider) {
			reachable = append(reachable, tier.ID)
		}
	}
	if len(reachable) == 0 {
		return noticeCmd("error", "no configured rung is served by "+strings.Join(wanted, ", ")+
			"; the ladder runs "+strings.Join(m.ladderProviders(), ", ")+
			", so this policy would leave every turn with nowhere to go")
	}
	return m.setDestinations(wanted)
}

func (m *tuiModel) setDestinations(providers []string) tea.Cmd {
	previous := m.app.config.Destinations
	m.app.config.Destinations = providers
	if err := m.app.config.Save(); err != nil {
		m.app.config.Destinations = previous
		return noticeCmd("error", "saving the destination policy failed, nothing changed: "+err.Error())
	}
	if len(providers) == 0 {
		return noticeCmd("", "destinations: any provider on the ladder; routing is governed by capability, context, and budget alone")
	}
	return noticeCmd("", "destinations: "+strings.Join(providers, ", ")+
		" only. Every other target is excluded before cost is considered, on the opening route, on a move, on a pin, and on resume; /why names the exclusion.")
}

func (m *tuiModel) destinationsStanding() string {
	ladder := strings.Join(m.ladderProviders(), ", ")
	if len(m.app.config.Destinations) == 0 {
		return "destinations: unrestricted. The ladder reaches " + ladder +
			". " + destinationsUsage
	}
	return "destinations: " + strings.Join(m.app.config.Destinations, ", ") +
		" only; the ladder reaches " + ladder +
		". The restriction is checked before cost, so /why reports an excluded target as policy rather than price."
}

// ladderProviders names what the configured rungs are served by, which is the
// list a user is choosing from and the one a refusal has to quote back.
func (m *tuiModel) ladderProviders() []string {
	var names []string
	for _, tier := range m.app.config.Tiers {
		if !slices.Contains(names, tier.Target.Provider) {
			names = append(names, tier.Target.Provider)
		}
	}
	if len(names) == 0 {
		return []string{"no configured rung"}
	}
	sort.Strings(names)
	return names
}
