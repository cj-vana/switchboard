# Phase 2b measurement, 2026-08-14

The original routing falsification gate has been run. It **failed**, and the
failure is about the ladder rather than the routing premise. This report keeps
the failed result because a failed gate narrows the product claim.

## Corpus

Thirty hand-written tier-1 tasks against this repository, each breaking a real
invariant and verified by the repository's own suite. Every task is checked to
fail before it is attempted, so none is handed out already solved; that check
caught four that were.

Twenty tasks at three replicate indices were run per arm. The journal field and
environment variable retain the legacy name `Seed`, but no provider in this
evaluator accepts that value as a sampling seed; these are repeated attempts, not
deterministically replayable generations.

## The verdict

| arm | solved | median latency |
|---|---|---|
| `always-lowest` (kimi-for-coding-highspeed) | 66/68 (97%) | 156s |
| `always-highest` (k3-256k) | 38/65 (58%) | 301s |
| `routed` | 52/68 (76%) | 192s |

The routed arm escalated on 47 of 68 runs.

Against the best fixed baseline, verified solve rate fell 20.6 points, well past
the two-point allowance. That was the historical arithmetic result; the
integrity checks below now supersede it with a refusal.

## Integrity addendum, 2026-08-17

The append-only journal behind the table contains 201 rows, not the 180 cells
defined by twenty tasks, three replicates, and three arms. Twenty-one
task/arm/replicate cells were run twice. Three disagree on solve outcome, while
seven differ in solve outcome, failure category, or final target. The table
above is the historical raw report; it is not a clean matrix and the
strengthened gate now refuses it rather than counting reruns as independent
evidence. It also predates per-row evaluation identities, so its rows cannot be
relabelled with a later commit, catalog, prompt, ladder, or snapshot set.

Keeping the first record for each cell only as a diagnostic gives 58/60 for
`always-lowest`, 35/60 for `always-highest`, and 44/60 for `routed`. Keeping the
last record changes `always-highest` to 34/60. Either projection makes the
original conclusion stronger, but neither is a replacement verdict: choosing
which rerun counts after seeing its result would be post-selection. A new clean
journal is required before quoting exact rates.

The failure categories narrow the result further. All 45 unsolved rows in the
free-ladder journal are serving/runtime failures: 44 eight-minute timeouts and
one tool-round limit. Every attempt that reached the verifier passed it. The
GPT-5.6 journal's eight unsolved rows are also all timeouts. The paid journal
contains 132 unsolved rows, all caused by insufficient Anthropic credit; its 48
solves establish observed successful-run cost, not a cache comparison or a
model-quality solve rate.

## Why it failed: the ladder is upside down

The rung assumed to be stronger solves less. On 13 of 20 tasks `k3-256k` did
worse than `kimi-for-coding-highspeed`, and it is also roughly twice as slow.

So escalating was actively harmful: every escalation moved work from a model
that solves 97% to one that solves 58%.

A configured ladder records the user's intent; it does not prove that model
capability is globally one-dimensional. Tier labels should be derived by
running pinned targets and finding the Pareto front, not assigned from model
reputation. This ladder was assigned from reputation because a model called k3
sounds stronger than one called highspeed. The measurement falsified that in
the first run.

That is the evaluation doing its job. A ladder ordered wrongly makes routing worse
than not routing, and nothing short of running the corpus would have shown it.

## What this does not establish

The cost half of the routing premise is unmeasured. Both rungs are
plan-metered, so every arm bills zero, cost per solved task is zero for all
three, and the cost condition cannot separate them. A saving reported on this
ladder would be an artefact of metering.

The cost condition needs two paid models. One paid arm produced 48 verified
solves before the credit ran out:

| | |
|---|---|
| `claude-haiku-4-5`, no cache markers | 48 verified solves; 12 credit failures |
| median cost per solved task | $0.2296 |
| spread | $0.0875 to $0.9067 |
| cache reads | 0 (control arm, correctly) |

## What to do next

Derive the ladder empirically instead of assuming it. Run the pinned targets
across the corpus and read the Pareto front from the result. That data now
exists for these two rungs, and it says the order should be reversed.

The cache comparison remains the cheapest experiment that tests the routing
premise: one model, one corpus, with cache-marker placement as the only
difference. At the observed rate that is roughly $14.

## Addendum, 2026-08-15: the ladder, derived

The pinned-target procedure has now been run for a fourth target:
`gpt-5.6-sol` on the ChatGPT-plan surface, pinned across the same twenty tasks at three seeds
(journal: `pin-gpt-5.6-sol-2026-08-15.jsonl`, two workers after six proved to
throttle the plan into deadline failures). The original raw projection over
every journal in hand was:

| target | solved | median latency | position |
|---|---|---|---|
| `kimi/coding/kimi-for-coding-highspeed` | 66/68 (97%) | 2m36s | projected front |
| `openai/subscription/gpt-5.6-sol` | 52/60 (87%) | 2m49s | dominated |
| `kimi/coding/k3-256k` | 38/65 (58%) | 5m3s | dominated |
| `anthropic/first-party/claude-haiku-4-5` | 48/120 (40%)* | 42s | not placeable* |

\* pooled across both cache-comparison arms. The 132 unsolved attempts in that
journal are all credit failures, so neither 48/120 nor 48/60 is a model-quality
rate and this target cannot be placed on a capability front from that run.

That raw projection leaves one plan-metered rung. Because its source journal
fails the matrix-integrity checks above, this is a diagnostic, not a releasable
ladder result. It nevertheless contains no evidence that climbing helps:
the cheapest plan-metered rung solves more than the reputed flagships and is
faster than all but haiku, which solves half as much.

**What that establishes, and what it cannot.** This corpus is thirty tier-1
tasks against this repository, and the diagnostic projection suggests the
strongest cheap target saturates it at 97%. A corpus the bottom rung nearly aces
cannot rank the rungs above it. There is no headroom in which a stronger model
could show value. A clean rerun can establish the bottom of the ladder; deriving
the upper rungs also needs a corpus hard enough that the bottom rung fails
routinely. Tier-2 tasks are the recorded next step.

**On a learned router.** The null hypothesis stands unchallenged. There is no
valid clean matrix yet, and even the diagnostic projection has only one rung,
so there is no routing decision for a model to learn. Sixty-odd attempts per
target is not training data past a logistic regression anyway. The release
gate requires a learned router to beat the deterministic policy after runtime
and distribution costs. It cannot be attempted until a clean, harder corpus
produces a front with at least two rungs. Training now would fit weights to a
decision that does not exist. See
[Why there is no learned router](routing.md#why-there-is-no-learned-router).

## Reproducing

```
SB_LIVE=1 SB_EVAL_TASKS=20 SB_EVAL_SEEDS=3 SB_EVAL_WORKERS=6 \
  SB_EVAL_COMMIT=<exact-harness-commit> \
  SB_EVAL_SNAPSHOTS='{"target/id/model":"resolved-snapshot"}' \
  SB_EVAL_JOURNAL=free-ladder.jsonl \
  go test ./internal/eval/ -run TestLiveBaselineRuns -v -timeout 240m
```

Every attempt is written and synced as it finishes, so a run that dies leaves a
partial measurement rather than nothing. Repeating the command against a clean
partial journal skips completed task/arm/replicate cells. A journal that already
contains duplicates or conflicts is preserved and refused; resumption never
silently selects one result. An invalid unterminated final record left by a
killed write is removed before appending; malformed complete records stop the
run instead of hiding the rows after them. Every new row carries an evaluation
identity over the pins, selected tasks, replicates, worker concurrency, arms,
and fixed-arm target bindings. Reusing a journal from any other configuration
stops before making a provider call.

A verdict can be recomputed from a recorded run without paying for the corpus
again. The report requires the exact commit, catalog revision, worker
concurrency, and a resolved snapshot for every target present in the journal.
Legacy journals without an evaluation identity remain readable as raw records,
but cannot pass the gate:

```
SB_EVAL_COMMIT=<exact-harness-commit> \
  SB_EVAL_CATALOG=<exact-catalog-revision> \
  SB_EVAL_TASKS=20 \
  SB_EVAL_WORKERS=6 \
  SB_EVAL_SNAPSHOTS='{"target/id/model":"resolved-snapshot"}' \
  SB_EVAL_JOURNAL=free-ladder.jsonl \
  go test ./internal/eval/ -run TestReportJournal -v
```
