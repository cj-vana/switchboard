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
