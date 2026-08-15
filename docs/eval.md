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

## Addendum, 2026-08-15: the ladder, derived

§8.6's procedure has now been run for a fourth target: `gpt-5.6-sol` on the
ChatGPT-plan surface, pinned across the same twenty tasks at three seeds
(journal: `pin-gpt-5.6-sol-2026-08-15.jsonl`, two workers after six proved to
throttle the plan into deadline failures). The front over every journal in
hand:

| target | solved | median latency | position |
|---|---|---|---|
| `kimi/coding/kimi-for-coding-highspeed` | 66/68 (97%) | 2m36s | **the front** |
| `openai/subscription/gpt-5.6-sol` | 52/60 (87%) | 2m49s | dominated |
| `kimi/coding/k3-256k` | 38/65 (58%) | 5m3s | dominated |
| `anthropic/first-party/claude-haiku-4-5` | 48/120 (40%)* | 42s | dominated |

\* pooled across both cache-comparison arms, one of which was cut short when
the credit ran out; the completed no-markers arm alone measured 48/60 (80%).
Either figure is dominated.

One rung survives domination. On this corpus there is no ladder to climb:
the cheapest plan-metered rung solves more than the reputed flagships and is
faster than all but haiku, which solves half as much.

**What that establishes, and what it cannot.** This corpus is thirty tier-1
tasks against this repository, and the front says the strongest cheap target
saturates it at 97%. A corpus the bottom rung nearly aces cannot rank the
rungs above it — there is no headroom in which a stronger model could show
value. So the derivation settles the bottom of the ladder empirically
(`kimi-for-coding-highspeed`, and `k3-256k` off the ladder entirely, on every
journal since the first run), and says nothing about ordering above it.
Deriving the upper rungs needs a corpus hard enough that the bottom rung
fails routinely: tier-2 tasks are the recorded next step.

**On a learned router.** §8.2's null hypothesis stands unchallenged: with
one rung on the measured front, there is no routing decision for a model to
learn, and 60-odd attempts per target is not training data past a logistic
regression anyway. The gate in §19.2 — a learned router must beat the
heuristic after runtime and distribution costs — cannot even be attempted
until a harder corpus produces a front with at least two rungs. Training
now would be fitting weights to a decision that does not exist.

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
