package main

// The `sb cost` subcommand: §15's cross-provider accounting, from the
// command line. Per session it reports calls, tokens, and what the catalog
// priced the work at, and the three zero-dollar meterings stay three
// different things here as everywhere: a local session says "local", a plan
// session says "plan", and neither is folded into the dollar total, because
// telling someone their quota burn was free teaches the wrong lesson.

import (
	"fmt"
	"io"
	"sort"

	"github.com/cj-vana/switchboard/internal/catalog"
	"github.com/cj-vana/switchboard/internal/session"
)

func runCostCLI(w io.Writer, store *session.Store, cat *catalog.Catalog, workspace string) error {
	infos, err := store.List(workspace)
	if err != nil {
		return err
	}
	if len(infos) == 0 {
		fmt.Fprintf(w, "no sessions recorded for %s\n", workspace)
		return nil
	}

	fmt.Fprintf(w, "%-22s %-19s %6s %10s %10s  %s\n", "session", "when", "calls", "in", "out", "cost")
	var total catalog.Money
	var priced, local, plan, unpriced int
	for _, info := range infos {
		state, err := session.ReadState(info.Path)
		if err != nil {
			fmt.Fprintf(w, "%-22s unreadable: %v\n", info.ID, err)
			continue
		}
		fmt.Fprintf(w, "%-22s %-19s %6d %10d %10d  %s\n",
			state.ID, info.Modified.Local().Format("2006-01-02 15:04:05"),
			state.Calls, state.Usage.InputTokens, state.Usage.OutputTokens,
			costWord(cat, state, &total, &priced, &local, &plan, &unpriced))
	}

	fmt.Fprintf(w, "\n%d sessions", len(infos))
	if priced > 0 {
		fmt.Fprintf(w, "; %s estimated across the %d that bill dollars, against each session's recorded catalog revision", total, priced)
	}
	if local > 0 {
		fmt.Fprintf(w, "; %d ran locally, so there is nothing to bill", local)
	}
	if plan > 0 {
		fmt.Fprintf(w, "; %d billed a plan, consuming quota rather than dollars", plan)
	}
	if unpriced > 0 {
		fmt.Fprintf(w, "; %d had nothing the catalog could price", unpriced)
	}
	fmt.Fprintln(w)
	fmt.Fprintln(w, "an estimator and reconciliation aid, not the provider's invoice (§15)")
	return nil
}

// costWord renders one session's cost column and files it under the right
// metering for the totals line.
func costWord(cat *catalog.Catalog, state session.State, total *catalog.Money, priced, local, plan, unpriced *int) string {
	target, err := parseRecordedTarget(state.Target)
	if err != nil {
		*unpriced++
		return "unpriced"
	}
	info, _, ok := cat.Lookup(target)
	switch {
	case !ok:
		*unpriced++
		return "unpriced"
	case info.Metering == catalog.Local:
		*local++
		return "local"
	case info.Metering == catalog.Plan:
		*plan++
		return "plan"
	case info.Free():
		// Dollar-metered, priced at zero throughout: rendering the recorded
		// zero as $0.00 would claim a bill where the catalog holds no rates.
		*unpriced++
		return "no per-token cost"
	default:
		*priced++
		*total += catalog.Money(state.CostMicroUSD)
		return catalog.Money(state.CostMicroUSD).String()
	}
}

// costTurnsLines is the per-ask receipt: the session's turns ordered by
// what they billed, each beside its own words, so "which asks cost the
// most" reads straight off the record. Turns that billed nothing fold
// into one closing line — local, plan, and unpriced stay out of the
// dollar rows, because a $0.00 row teaches the wrong lesson here as
// everywhere.
func costTurnsLines(turns []session.TurnCost) []string {
	if len(turns) == 0 {
		return []string{"  no turns recorded yet"}
	}
	billed := make([]session.TurnCost, 0, len(turns))
	var unbilled, unbilledCalls, unbilledIn, unbilledOut int
	for _, t := range turns {
		if t.CostMicroUSD > 0 {
			billed = append(billed, t)
			continue
		}
		unbilled++
		unbilledCalls += t.Calls
		unbilledIn += t.Usage.InputTokens + t.Usage.CacheReadTokens + t.Usage.CacheWriteTokens
		unbilledOut += t.Usage.OutputTokens
	}
	sort.SliceStable(billed, func(i, j int) bool { return billed[i].CostMicroUSD > billed[j].CostMicroUSD })

	var lines []string
	const shown = 8
	for i, t := range billed {
		if i == shown {
			var rest catalog.Money
			for _, more := range billed[shown:] {
				rest += catalog.Money(more.CostMicroUSD)
			}
			lines = append(lines, fmt.Sprintf("  … and %d more billed turns, %s between them", len(billed)-shown, rest))
			break
		}
		in := t.Usage.InputTokens + t.Usage.CacheReadTokens + t.Usage.CacheWriteTokens
		lines = append(lines, fmt.Sprintf("  #%-3d %-10s ↓%s ↑%s  %d calls  %q",
			t.Turn, catalog.Money(t.CostMicroUSD).String(), compact(in), compact(t.Usage.OutputTokens),
			t.Calls, truncate(t.Prompt, 48)))
	}
	if len(billed) == 0 {
		lines = append(lines, "  no turn billed dollars")
	}
	if unbilled > 0 {
		word := "turns"
		if unbilled == 1 {
			word = "turn"
		}
		lines = append(lines, fmt.Sprintf("  %d %s billed nothing — local, plan, or unpriced calls (↓%s ↑%s across %d calls); /cost keeps those meterings apart",
			unbilled, word, compact(unbilledIn), compact(unbilledOut), unbilledCalls))
	}
	return lines
}
