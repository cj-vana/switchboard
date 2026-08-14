# Phase 2b measurement, 2026-08-14

The §7.1 falsification gate has been run. It **failed**, and the failure is
about the ladder rather than the thesis. This records both, because §7.1 says a
failure narrows the product rather than being a reason to look away from it.

## Corpus

Thirty hand-written tier-1 tasks against this repository, each breaking a real
invariant and verified by the repository's own suite. Every task is checked to
fail before it is attempted, so none is handed out already solved; that check
caught four that were.

Twenty tasks at three seeds were run per arm.

## The verdict

| arm | solved | median latency |
|---|---|---|
| `always-lowest` (kimi-for-coding-highspeed) | 66/68 (97%) | 156s |
| `always-highest` (k3-256k) | 38/65 (58%) | 301s |
| `routed` | 52/68 (76%) | 192s |

The routed arm escalated on 47 of 68 runs.

Against the best fixed baseline, verified solve rate fell 20.6 points, well past
the two-point allowance. The gate fails.

## Why it failed: the ladder is upside down

The rung assumed to be stronger solves less. On 13 of 20 tasks `k3-256k` did
worse than `kimi-for-coding-highspeed`, and it is also roughly twice as slow.

So escalating was actively harmful: every escalation moved work from a model
that solves 97% to one that solves 58%.

§3.1 says the ladder is the user's intent and not a claim that capability is
globally one-dimensional, and §8.6 says tier labels are derived by running
pinned targets and finding the Pareto front, "not assigned from model
reputation". This ladder was assigned from reputation, because a model called
k3 sounds stronger than one called highspeed. The measurement falsified that in
the first run.

That is the harness doing its job. A ladder ordered wrongly makes routing worse
than not routing, and nothing short of running the corpus would have shown it.

## What this does not establish

The cost half of §7.1 is unmeasured. Both rungs are plan-metered, so every arm
bills zero, cost per solved task is zero for all three, and the cost condition
cannot separate them. A saving reported on this ladder would be an artefact of
metering.

The cost condition needs two paid models. One paid arm was measured before the
credit ran out:

| | |
|---|---|
| `claude-haiku-4-5`, no cache markers | 48/60 solved |
| median cost per solved task | $0.2296 |
| spread | $0.0875 to $0.9067 |
| cache reads | 0 (control arm, correctly) |

## What to do next

Derive the ladder empirically instead of assuming it. §8.6 already says how:
run the pinned targets across the corpus and read the Pareto front off the
result. That data now exists for these two rungs, and it says the order should
be reversed.

The cache comparison remains the cheapest experiment that tests the thesis §7 is
named for: one model, one corpus, and the only difference is whether §6 places
markers. At the observed rate that is roughly $14.

## Reproducing

```
SB_LIVE=1 SB_EVAL_TASKS=20 SB_EVAL_SEEDS=3 SB_EVAL_WORKERS=6 \
  SB_EVAL_JOURNAL=free-ladder.jsonl \
  go test ./internal/eval/ -run TestLiveBaselineRuns -v -timeout 240m
```

Every attempt is written and synced as it finishes, so a run that dies leaves a
partial measurement rather than nothing. A verdict can be recomputed from a
recorded run without paying for the corpus again:

```
SB_EVAL_JOURNAL=free-ladder.jsonl go test ./internal/eval/ -run TestReportJournal -v
```
