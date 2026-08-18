package main

// /why: the router explains itself. Every neighboring tool is opaque about
// why it did what it did; this one's whole thesis is the choice of model, so
// the choice has to be inspectable at any moment — what is running, how it
// was chosen, every move this session made, and what the same work would
// have cost on each other rung.

import (
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/switchboard-code/switchboard/internal/catalog"
)

func cmdWhy(m *tuiModel, _ string) tea.Cmd {
	var b strings.Builder

	tier := m.app.tier
	fmt.Fprintf(&b, "running    %s", tier.Target.Display())
	if tier.Label != "" {
		fmt.Fprintf(&b, "  (%s, %q)", tier.ID, tier.Label)
	} else if tier.ID != "" {
		fmt.Fprintf(&b, "  (%s)", tier.ID)
	}
	b.WriteString("\n")

	if d := m.app.route; d != nil {
		fmt.Fprintf(&b, "opening    %s chose %s: %s (confidence %.2f)\n", d.Source, d.Tier, d.Rationale, d.Confidence)
		for _, why := range d.Infeasible {
			fmt.Fprintf(&b, "ruled out  %s\n", why)
		}
	} else if m.app.sticky != nil && !m.app.sticky.Pinned() {
		b.WriteString("opening    automatic routing will choose from the next user turn; no prompt decision is pending\n")
	} else {
		b.WriteString("opening    picked by you, not the router\n")
	}

	if len(m.routeLog) == 0 {
		b.WriteString("history    no route changes this session\n")
	} else {
		for i, entry := range m.routeLog {
			if i == 0 {
				fmt.Fprintf(&b, "history    %s\n", entry)
			} else {
				fmt.Fprintf(&b, "           %s\n", entry)
			}
		}
	}

	// Races are the session's paired evidence: the same prompt judged across
	// two rungs, which is a stronger fact about model choice than any single
	// turn's outcome, so the explanation owes it a line.
	for i, entry := range m.raceLog {
		if i == 0 {
			fmt.Fprintf(&b, "races      %s\n", entry)
		} else {
			fmt.Fprintf(&b, "           %s\n", entry)
		}
	}

	// The counterfactual: this session's token counts priced on every rung.
	// Same tokens is the honest caveat — a different model writes different
	// tokens — but it is the comparison the invoice makes, and it is the one
	// the user can act on.
	state := m.app.loop.Session.State()
	if state.Usage.InputTokens+state.Usage.OutputTokens > 0 && len(m.app.config.Tiers) > 1 {
		b.WriteString("\nthis session's tokens, priced on each rung (same tokens, which no other model would have produced exactly):\n")
		for _, t := range m.app.config.Tiers {
			marker := "  "
			if t.ID == tier.ID {
				marker = "▸ "
			}
			info, _, ok := m.app.catalog.Lookup(t.Target)
			if !ok {
				fmt.Fprintf(&b, "%s%-4s %-40s not in the catalog, unpriced\n", marker, t.ID, t.Target.Display())
				continue
			}
			cost, _, priced := info.Cost(state.Usage)
			switch {
			case info.Metering == catalog.Local:
				fmt.Fprintf(&b, "%s%-4s %-40s free (local)\n", marker, t.ID, t.Target.Display())
			case info.Metering == catalog.Plan:
				fmt.Fprintf(&b, "%s%-4s %-40s $0 (plan quota pays)\n", marker, t.ID, t.Target.Display())
			case priced:
				fmt.Fprintf(&b, "%s%-4s %-40s %s\n", marker, t.ID, t.Target.Display(), cost)
			default:
				fmt.Fprintf(&b, "%s%-4s %-40s unpriced\n", marker, t.ID, t.Target.Display())
			}
		}
	}

	m.addInfo(strings.TrimRight(b.String(), "\n"))
	return nil
}
