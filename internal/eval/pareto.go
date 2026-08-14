package eval

import (
	"fmt"
	"sort"
	"time"

	"github.com/cjvana/switchboard/internal/catalog"
	"github.com/cjvana/switchboard/internal/provider"
)

// Deriving a ladder from measurement rather than from reputation.
//
// §8.6 says appropriate-tier labels come from running pinned targets and
// reading the quality/cost Pareto front, and are "not assigned from model
// reputation". The first real gate run showed why that sentence is there: a
// ladder ordered by which model sounded stronger put a target solving 58% above
// one solving 97%, and routing up was worse than not routing at all.
//
// So this reads an ordering off recorded runs. It is not clever. It is the
// difference between a ladder that is measured and one that is guessed.

// Point is one target's measured position.
type Point struct {
	Target provider.RouteTargetID

	Attempts  int
	Solved    int
	SolveRate float64

	MedianCost    catalog.Money
	MedianLatency time.Duration

	// Metering says what the cost figure means. A plan target's zero is not a
	// local target's zero and neither is a paid target's number, so a front
	// drawn across metering kinds is comparing three different currencies.
	Metering catalog.Metering

	// Dominated names the targets that beat this one on both axes. A target
	// with anything here does not belong on a ladder: something else solves
	// more and costs no more.
	Dominated []provider.RouteTargetID
}

// Front is the derived ordering plus what had to be assumed to derive it.
type Front struct {
	Points []Point

	// Ladder is the ordering to configure, cheapest first among the targets
	// that are not dominated.
	Ladder []provider.RouteTargetID

	// Warnings are the reasons a front might be misleading. They are part of
	// the result rather than a log line, because a front computed across
	// metering kinds or from two attempts looks exactly like a good one.
	Warnings []string
}

// DeriveFront reads a ladder off recorded runs.
//
// minAttempts is how many attempts a target needs before it is placed at all.
// A target measured twice has a solve rate of 0, 50, or 100 percent and nothing
// in between, and putting that on a front is how a ladder gets ordered by noise.
func DeriveFront(runs []Run, cat *catalog.Catalog, minAttempts int) Front {
	byTarget := map[provider.RouteTargetID][]Run{}
	for _, r := range runs {
		// Routed runs are excluded: they are the thing a ladder produces, not
		// evidence about a rung. Including them would let the router's own
		// choices shape the ordering it is then judged against.
		if r.Arm == RoutedArm || r.Target == "" {
			continue
		}
		byTarget[r.Target] = append(byTarget[r.Target], r)
	}

	var front Front
	meterings := map[catalog.Metering]bool{}

	for target, group := range byTarget {
		if len(group) < minAttempts {
			front.Warnings = append(front.Warnings, fmt.Sprintf(
				"%s has %d attempts and needs %d to be placed; a rate from that few is noise",
				target, len(group), minAttempts))
			continue
		}

		p := Point{Target: target, Attempts: len(group)}
		var costs []catalog.Money
		var latencies []time.Duration
		for _, r := range group {
			if !r.Solved {
				continue
			}
			p.Solved++
			costs = append(costs, r.Cost)
			latencies = append(latencies, r.Duration)
		}
		p.SolveRate = float64(p.Solved) / float64(p.Attempts)
		p.MedianCost = medianMoney(costs)
		p.MedianLatency = medianDuration(latencies)

		if info, _, ok := cat.Lookup(targetFromID(target)); ok {
			p.Metering = catalog.Metering(info.Metering.String())
			meterings[p.Metering] = true
		}
		front.Points = append(front.Points, p)
	}

	if len(meterings) > 1 {
		front.Warnings = append(front.Warnings,
			"these targets are metered differently, so their cost figures are not the same currency "+
				"and the front is ordered on solve rate and latency alone")
	}

	// Dominance: another target solves at least as often and costs no more,
	// with one of those strict. A dominated target has no place on a ladder,
	// because there is never a reason to choose it.
	sameCurrency := len(meterings) <= 1
	for i := range front.Points {
		for j := range front.Points {
			if i == j {
				continue
			}
			a, b := front.Points[i], front.Points[j]
			better := b.SolveRate >= a.SolveRate
			cheaper := !sameCurrency || b.MedianCost <= a.MedianCost
			strict := b.SolveRate > a.SolveRate || (sameCurrency && b.MedianCost < a.MedianCost)
			if better && cheaper && strict {
				front.Points[i].Dominated = append(front.Points[i].Dominated, b.Target)
			}
		}
	}

	// The ladder is what survives, ordered cheapest first, then by solve rate.
	// A rung is only worth having above another if it solves more.
	var surviving []Point
	for _, p := range front.Points {
		if len(p.Dominated) == 0 {
			surviving = append(surviving, p)
		}
	}
	sort.Slice(surviving, func(i, j int) bool {
		if sameCurrency && surviving[i].MedianCost != surviving[j].MedianCost {
			return surviving[i].MedianCost < surviving[j].MedianCost
		}
		if surviving[i].SolveRate != surviving[j].SolveRate {
			return surviving[i].SolveRate < surviving[j].SolveRate
		}
		return surviving[i].MedianLatency < surviving[j].MedianLatency
	})
	for _, p := range surviving {
		front.Ladder = append(front.Ladder, p.Target)
	}

	if len(front.Ladder) < 2 {
		front.Warnings = append(front.Warnings,
			"fewer than two targets survive domination, so there is no ladder to climb: "+
				"one target is doing everything the others do, at least as well")
	}
	return front
}

// targetFromID reconstructs a route target so a recorded run can be priced and
// its metering read.
func targetFromID(id provider.RouteTargetID) provider.RouteTarget {
	var out provider.RouteTarget
	fields := splitN(string(id), '/', 3)
	if len(fields) < 3 {
		return out
	}
	model := fields[2]
	if i := indexByte(model, '+'); i >= 0 {
		model = model[:i]
	}
	return provider.RouteTarget{Provider: fields[0], Surface: fields[1], ModelID: model}
}

func splitN(s string, sep byte, n int) []string {
	var out []string
	for len(out) < n-1 {
		i := indexByte(s, sep)
		if i < 0 {
			break
		}
		out = append(out, s[:i])
		s = s[i+1:]
	}
	return append(out, s)
}

func indexByte(s string, b byte) int {
	for i := range len(s) {
		if s[i] == b {
			return i
		}
	}
	return -1
}
