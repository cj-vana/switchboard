package main

// /stats and `sb stats`: the ladder's receipt at lifetime scale. /cost rungs
// prices one session against the rungs it did not run on; this reads every
// session recorded for the workspace and prices the whole history the same
// way, with the same honesty rules — cold counterfactuals, the three
// zero-dollar meterings kept apart, and an unpriceable rung saying so
// rather than showing a partial dollar figure.
//
// The scope is stated because it has edges: race losers and forks are
// counted, since their calls were real calls, while subagent sessions live
// in their own store and are not. Yesterday's calls are priced against
// today's catalog and today's ladder, because the question the table
// answers is "what would this history cost on the ladder I have now" —
// each session's own record keeps the revision that priced it at the time.

import (
	"fmt"
	"io"
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/cj-vana/switchboard/internal/catalog"
	"github.com/cj-vana/switchboard/internal/config"
	"github.com/cj-vana/switchboard/internal/session"
)

// statsLines reads every session log for the workspace and renders the
// lifetime receipt.
func statsLines(tiers []config.Tier, cat *catalog.Catalog, activeTier string, store *session.Store, workspace string) []string {
	infos, err := store.List(workspace)
	if err != nil {
		return []string{"  " + err.Error()}
	}
	if len(infos) == 0 {
		return []string{"  no sessions recorded for this workspace yet"}
	}

	var all []session.Usage
	read, unreadable := 0, 0
	for _, info := range infos {
		usages, err := session.ReadUsages(info.Path)
		if err != nil {
			unreadable++
			continue
		}
		read++
		all = append(all, usages...)
	}

	var in, out, cacheRead, cacheWrite int
	for _, u := range all {
		in += u.Usage.InputTokens
		out += u.Usage.OutputTokens
		cacheRead += u.Usage.CacheReadTokens
		cacheWrite += u.Usage.CacheWriteTokens
	}

	head := fmt.Sprintf("  %d sessions, %d model calls: ↓%s ↑%s tokens", read, len(all), compact(in+cacheRead+cacheWrite), compact(out))
	if cacheRead > 0 {
		head += fmt.Sprintf(", %s of that served from cache", compact(cacheRead))
	}
	lines := []string{head}
	if unreadable > 0 {
		lines = append(lines, fmt.Sprintf("  %d logs could not be read and are not counted", unreadable))
	}
	if len(all) == 0 {
		return append(lines, "  no model calls recorded, so there is nothing to price")
	}

	lines = append(lines, "  "+asRoutedLine(cat, all), "")

	if len(tiers) == 0 {
		return append(lines, "  no ladder is configured, so there are no rungs to compare")
	}
	lines = append(lines,
		"  the whole history priced as if every call had gone to one rung —",
		"  no cache assumed, every input token at the rung's ordinary input",
		"  rate, today's catalog and ladder, and these sessions' own token",
		"  counts, though another model would tokenize the same text differently:")
	width := 0
	for _, t := range tiers {
		if w := len(t.String()); w > width {
			width = w
		}
	}
	for _, t := range tiers {
		marker := "   "
		if t.ID == activeTier {
			marker = " * "
		}
		lines = append(lines, fmt.Sprintf(" %s%-*s  %s", marker, width, t.String(), rungWord(cat, t, all)))
	}
	lines = append(lines,
		"  race losers and forks count — their calls were real; subagent",
		"  sessions keep their own store and do not",
		"  an estimator and reconciliation aid, not the provider's invoice (§15)")
	return lines
}

func cmdStats(m *tuiModel, _ string) tea.Cmd {
	// Read-only over the workspace's logs, the current one included; its
	// open log reads the way `sb cost` reads it, which is what makes this
	// busy-safe.
	m.addInfo(strings.Join(statsLines(m.app.config.Tiers, m.app.catalog, m.app.tier.ID, m.app.store, m.app.workspace), "\n"))
	return nil
}

func runStatsCLI(w io.Writer, store *session.Store, cat *catalog.Catalog, cfg *config.Config, workspace string) error {
	// No session is active in a CLI run, so no rung wears the marker.
	for _, line := range statsLines(cfg.Tiers, cat, "", store, workspace) {
		fmt.Fprintln(w, strings.TrimRight(line, " "))
	}
	return nil
}
