package main

// /race assembly: the same prompt, from the same prefix, on two rungs at
// once, judged by the user. §8.4's complaint about natural outcomes is that
// they are weak evidence — a clean completion says nothing about necessity —
// and its shadow-routing answer is gated on verifiers and sandboxes that do
// not exist yet. A race is the interactive form of the same counterfactual:
// both outcomes are independently judged by the person whose task it is,
// which is the strongest label class ordinary use can produce. The verdict
// is recorded and deliberately never consulted by routing; collecting the
// corpus is phase 2b's job, acting on it is gated behind phase 7.
//
// Each arm is a fork of the session (§12), so its messages are
// byte-identical to the prefix a provider may still hold warm: the arm on
// the sitting rung rides that prefix warm, and the challenger pays the cold
// read any first contact pays. The asymmetry is real and stays in the
// record, because hiding it would misprice the comparison.

import (
	"fmt"
	"time"

	"github.com/cj-vana/switchboard/internal/agent"
	"github.com/cj-vana/switchboard/internal/catalog"
	"github.com/cj-vana/switchboard/internal/config"
	"github.com/cj-vana/switchboard/internal/permission"
	"github.com/cj-vana/switchboard/internal/prefix"
	"github.com/cj-vana/switchboard/internal/provider"
	"github.com/cj-vana/switchboard/internal/session"
)

// A raceArm is one branch of the trial: its rung, the client that probed
// for it, the forked session it runs in, and the loop assembled around
// them. base* is the forked prefix's accounting, so the arm's own spend is
// what grew past it.
type raceArm struct {
	tier   config.Tier
	client provider.Provider
	sess   *session.Session
	loop   *agent.Loop

	baseCost  int64
	baseUsage provider.Usage

	status  string // "completed", "error", "cancelled", "round_limit"
	failure string // the error text behind a status of "error"
	started time.Time
	wall    time.Duration
}

// record prices the arm's own work: the fork copied the prefix's usage
// records, so the branch total minus the base is what this arm added.
func (a *raceArm) record() session.RaceArm {
	state := a.sess.State()
	return session.RaceArm{
		Tier:         a.tier.ID,
		Target:       a.tier.Target.ID(),
		SessionID:    state.ID,
		Status:       a.status,
		Usage:        state.Usage.Sub(a.baseUsage),
		CostMicroUSD: state.CostMicroUSD - a.baseCost,
		WallTimeMS:   a.wall.Milliseconds(),
	}
}

// assembleRaceArm forks the session onto the arm's rung and builds its
// loop. The loop is the primary's in every byte that reaches the provider —
// the same system blocks, and a Branch of the same registry so the tool
// schemas render identically (§6.1) — and not in what it may do: the
// permission engine is a fresh one in plan mode, which denies every
// non-read effect outright before rules or remembered answers are
// consulted, whatever mode the session itself runs in. That is the §8.4
// isolation rule for counterfactual runs, enforced where no mode can route
// around it. The asker is deliberately nil: plan mode never asks for what
// it denies, and anything that somehow reached an ask would be refused with
// the reason rather than answered by a dialog the user thinks is about the
// session. Mutation is what the winner does after the pick.
func assembleRaceArm(app *tuiApp, tier config.Tier, client provider.Provider, obs agent.Observer) (*raceArm, error) {
	state := app.loop.Session.State()
	var sess *session.Session
	var err error
	if n := len(state.Messages); n > 0 {
		sess, err = app.store.ForkOnto(state.ID, n, tier.Target.ID())
	} else {
		// A race on an empty session has no prefix to fork; two fresh logs
		// share the same nothing, which is the same guarantee trivially held.
		sess, err = app.store.Create(app.workspace, tier.Target.ID(), app.catalog.Revision)
	}
	if err != nil {
		return nil, err
	}

	branchState := sess.State()
	arm := &raceArm{
		tier:      tier,
		client:    client,
		sess:      sess,
		baseCost:  branchState.CostMicroUSD,
		baseUsage: branchState.Usage,
	}
	arm.loop = &agent.Loop{
		Provider: client,
		Target:   tier.Target,
		Tools: app.loop.Tools.Branch(map[string]string{
			"delegate": "delegate is unavailable in a race branch: an errand spawned here would outlive the pick; the branch that wins can delegate after it continues",
		}),
		Perms:    permission.NewEngine(permission.ModePlan, app.capability),
		Session:  sess,
		Catalog:  app.catalog,
		Cache:    cacheFor(tier.Target, app.catalog),
		System:   app.loop.System,
		Observer: obs,
		Hooks:    app.loop.Hooks,
	}
	return arm, nil
}

// raceGates wires the shared ceiling across both arms: each gate charges
// the pre-race session plus what both branches have added, so two arms
// cannot each spend up to the ceiling by not counting the other.
func raceGates(bs *budgetState, cat *catalog.Catalog, before session.State, a, b *raceArm) {
	spent := func() catalog.Money {
		return catalog.Money(before.CostMicroUSD +
			(a.sess.State().CostMicroUSD - a.baseCost) +
			(b.sess.State().CostMicroUSD - b.baseCost))
	}
	a.loop.Budget = budgetGate(bs, cat, func() provider.RouteTarget { return a.loop.Target }, spent)
	b.loop.Budget = budgetGate(bs, cat, func() provider.RouteTarget { return b.loop.Target }, spent)
}

// racePreflight refuses a race the ceiling cannot hold. §15's rule applied
// twice over: the arms run at once, so both upper bounds have to fit at
// once, and a race affordable arm-by-arm is not a race under a ceiling.
// Unpriced and non-dollar rungs pass the way they pass everywhere — a
// ceiling governs dollars only.
func racePreflight(bs *budgetState, cat *catalog.Catalog, before session.State,
	system []provider.Block, defs []provider.ToolDefinition, opening provider.Message,
	a, b config.Tier) (string, bool) {
	if bs == nil {
		return "", false
	}
	ceiling := bs.get()
	if ceiling == 0 {
		return "", false
	}
	tokens := prefix.RequestTokens(provider.Request{
		System:   system,
		Tools:    defs,
		Messages: append(append([]provider.Message(nil), before.Messages...), opening),
	})
	var bound catalog.Money
	for _, tier := range []config.Tier{a, b} {
		if info, _, ok := cat.Lookup(tier.Target); ok {
			bound += preflightBound(info, tokens)
		}
	}
	spent := catalog.Money(before.CostMicroUSD)
	if spent+bound > ceiling {
		return fmt.Sprintf("both arms together could cost up to %s against %s already spent, crossing the %s ceiling; /budget raises or clears it",
			bound, spent, ceiling), true
	}
	return "", false
}

// raceRecord assembles the verdict. outcome and kept follow the Race type's
// vocabulary; the prompt rides along so the record reads on its own.
func raceRecord(prompt string, a, b *raceArm, outcome, kept string) session.Race {
	return session.Race{
		Prompt:  prompt,
		A:       a.record(),
		B:       b.record(),
		Outcome: outcome,
		Kept:    kept,
	}
}
