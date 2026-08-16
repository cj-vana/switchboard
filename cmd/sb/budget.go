package main

// The /budget ceiling. §15 is specific about what a hard budget is checked
// against: a conservative preflight bound, not the expectation, because a
// turn affordable on average is not a turn under a ceiling. The check runs
// in three places — the router refuses rungs whose upper bound could cross
// it, the escalation policy cannot move onto one, and the loop stops before
// the call that would — and all three price the same way, through the §6.4
// estimator with its measured upward widening.
//
// The ceiling is dollars, and only dollars. A local rung consumes nothing
// scarce and a plan rung consumes quota; neither is governed, because the
// three meterings are never collapsed (§4).

import (
	"fmt"
	"sync"

	"github.com/cj-vana/switchboard/internal/agent"
	"github.com/cj-vana/switchboard/internal/catalog"
	"github.com/cj-vana/switchboard/internal/config"
	"github.com/cj-vana/switchboard/internal/costmodel"
	"github.com/cj-vana/switchboard/internal/provider"
)

// budgetState holds the ceiling behind a lock because two goroutines meet
// here: the loop reads it before every model call, and the UI writes it when
// /budget changes mid-turn — which is exactly how a runaway turn gets reined
// in without waiting for it to finish.
type budgetState struct {
	mu      sync.Mutex
	ceiling catalog.Money // zero means no ceiling
}

func (b *budgetState) set(c catalog.Money) {
	b.mu.Lock()
	b.ceiling = c
	b.mu.Unlock()
}

func (b *budgetState) get() catalog.Money {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.ceiling
}

// preflightBound prices the §15 worst case for one call: the whole request
// cold, the target's maximum output, widened by the estimator's measured
// bias. Eligibility mirrors candidatesFor, because a target that places
// markers pays the write rate on a miss and that is the larger number.
func preflightBound(info catalog.ModelInfo, promptTokens int) catalog.Money {
	est := costmodel.Estimator{}.Turn(costmodel.Inputs{
		Info:         info,
		PrefixTokens: promptTokens,
		OutputTokens: info.MaxOutput,
		Eligible:     info.Cache.UsageAccounting == catalog.AccountingSeparate,
	})
	return est.High
}

// budgetGate builds a loop's pre-call check. Target and spent are read at
// call time because both move under the gate: an escalation rebinds the
// loop's target, and every priced call raises the spend. An unpriced target
// passes — a ceiling cannot govern what has no price, and /budget says so
// when it is set.
func budgetGate(bs *budgetState, cat *catalog.Catalog, target func() provider.RouteTarget, spent func() catalog.Money) func(int) error {
	return func(promptTokens int) error {
		ceiling := bs.get()
		if ceiling == 0 {
			return nil
		}
		info, _, ok := cat.Lookup(target())
		if !ok {
			return nil
		}
		have := spent()
		bound := preflightBound(info, promptTokens)
		if have+bound > ceiling {
			return fmt.Errorf("stopped before the call: %s spent and the next call could cost up to %s, "+
				"which would cross the %s ceiling; /budget raises or clears it", have, bound, ceiling)
		}
		return nil
	}
}

// primaryGate wires the gate to the primary loop: its own moving target,
// its own session's priced record — the same number /cost shows.
func primaryGate(bs *budgetState, loop *agent.Loop, cat *catalog.Catalog) func(int) error {
	return budgetGate(bs, cat,
		func() provider.RouteTarget { return loop.Target },
		func() catalog.Money { return catalog.Money(loop.Session.State().CostMicroUSD) })
}

// budgetBlocksMove answers whether an escalation may land on a rung. §8.3
// lets a quality trigger override a cost preference and never a hard
// ceiling, so a destination whose upper bound does not fit is refused with
// the reason, and the primary stays where it is.
func budgetBlocksMove(bs *budgetState, cat *catalog.Catalog, dest config.Tier, spent catalog.Money, promptTokens int) (string, bool) {
	ceiling := bs.get()
	if ceiling == 0 {
		return "", false
	}
	info, _, ok := cat.Lookup(dest.Target)
	if !ok {
		return "", false
	}
	bound := preflightBound(info, promptTokens)
	if spent+bound > ceiling {
		return fmt.Sprintf("a turn on %s could cost up to %s against %s already spent, crossing the %s ceiling",
			dest.ID, bound, spent, ceiling), true
	}
	return "", false
}
