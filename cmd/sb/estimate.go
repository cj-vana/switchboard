package main

// /estimate: the next turn priced on every rung before it is sent. The
// product's bet is that reasoning about what a choice costs beats always
// calling the same model, and every piece of that reasoning already runs
// inside the loop — the zone split, the §6.4 estimator, the cache
// belief, the three meterings. This surface turns the machinery outward:
// type the prompt after the command and see what each rung would charge
// for it, before anything leaves the machine. The neighbors ask you to
// pick a model; this prices the pick first.
//
// Deliberately not busy-safe, the /cache posture: the expectation reads
// state the loop goroutine owns during a turn.

import (
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/cj-vana/switchboard/internal/catalog"
	"github.com/cj-vana/switchboard/internal/config"
	"github.com/cj-vana/switchboard/internal/costmodel"
	"github.com/cj-vana/switchboard/internal/prefix"
	"github.com/cj-vana/switchboard/internal/provider"
)

// estimateOutputAllowance keeps the rungs comparable: output length is
// unknown before the turn runs, so every rung gets the same flat
// allowance rather than a prediction nothing could back.
const estimateOutputAllowance = 512

func cmdEstimate(m *tuiModel, args string) tea.Cmd {
	prompt := strings.TrimSpace(args)

	sys := prefix.RequestTokens(provider.Request{System: m.app.loop.System})
	tools := prefix.RequestTokens(provider.Request{Tools: m.app.loop.Tools.Definitions()})
	conv := prefix.RequestTokens(provider.Request{Messages: m.app.loop.Session.State().Messages})
	promptTokens := len(prompt) / 4

	var hit float64
	if m.app.loop.Cache != nil {
		if exp, ok := m.app.loop.Cache.Expectation(time.Now()); ok {
			hit = exp.HitProbability
		}
	}

	var b strings.Builder
	b.WriteString("the next turn, priced on every rung before it is sent\n")
	up := sys + tools + conv + promptTokens
	line := fmt.Sprintf("  ~%s tokens would go up: system %s · tools %s · conversation %s",
		compact(up), compact(sys), compact(tools), compact(conv))
	if promptTokens > 0 {
		line += fmt.Sprintf(" · your prompt %s", compact(promptTokens))
	}
	b.WriteString(line + "\n")
	fmt.Fprintf(&b, "  chars-over-four estimates and a flat %d-token output allowance, so the rungs compare on the same turn\n",
		estimateOutputAllowance)

	width := 0
	for _, t := range m.app.config.Tiers {
		if w := len(t.String()); w > width {
			width = w
		}
	}
	for _, t := range m.app.config.Tiers {
		marker := "   "
		if t.ID == m.app.tier.ID {
			marker = " * "
		}
		fmt.Fprintf(&b, "%s%-*s  %s\n", marker, width, t.String(),
			estimateWord(m.app.catalog, t, sys+tools+conv, promptTokens, t.ID == m.app.tier.ID, hit))
	}
	b.WriteString("an estimate against the catalog, not a quote; /why prices the turn after it runs")
	m.addInfo(strings.TrimRight(b.String(), "\n"))
	return nil
}

// estimateWord prices one rung in its own metering, never collapsed into
// free. Only the active rung carries the tracker's belief — a tracker
// belongs to a target, and the other rungs have nobody watching them, so
// they price cold and say so.
func estimateWord(cat *catalog.Catalog, tier config.Tier, prefixTokens, freshTokens int, active bool, hit float64) string {
	info, _, ok := cat.Lookup(tier.Target)
	switch {
	case !ok:
		return "no catalog entry, so there is nothing to price against"
	case info.Metering == catalog.Local:
		return "runs locally — nothing to bill"
	case info.Metering == catalog.Plan:
		return "bills a plan, consuming quota rather than dollars"
	case info.Free():
		return "no price on record"
	}

	eligible := info.Cache.UsageAccounting == catalog.AccountingSeparate
	probability := 0.0
	if active && eligible {
		probability = hit
	}
	est := costmodel.Estimator{}.Turn(costmodel.Inputs{
		Target:         tier.Target,
		Info:           info,
		PrefixTokens:   prefixTokens,
		FreshTokens:    freshTokens,
		OutputTokens:   estimateOutputAllowance,
		Eligible:       eligible,
		HitProbability: probability,
	})
	word := fmt.Sprintf("between %s and %s, expected %s", est.Low, est.High, est.Expected)
	switch {
	case probability > 0:
		word += fmt.Sprintf(" — modeled hit chance ~%.0f%% folded in, the tracker's belief rather than a promise", probability*100)
	case eligible:
		word += " — priced cold; no cache observations for this rung"
	}
	return word
}
