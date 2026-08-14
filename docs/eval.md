# Phase 2b measurement, 2026-08-13

What the eval harness has actually established, and what it has not. §8.6 keeps
the harness from shipping a verdict before its corpus is populated; this records
where that stands rather than leaving the numbers in a terminal.

## The corpus

Thirty hand-written tier-1 tasks against this repository. Each breaks a real
invariant and asks for it back, verified by the repository's own test suite
scoped to the affected package. Every task is checked to fail before it is
attempted, so none can be handed out already solved, and several re-check a
property the suite alone would miss so a task cannot be "solved" by deleting
what fails.

## What was measured

One arm, complete: `claude-haiku-4-5` with no cache markers placed, over twenty
tasks at three seeds each.

| | |
|---|---|
| solved | 48/60 (80%) |
| median cost per solved task | $0.2296 |
| cost spread | $0.0875 to $0.9067 |
| median latency | 41s |
| cache reads | 0 |

The zero is the control working: this arm places no markers, so it reads nothing
back, and any later comparison has a clean baseline to sit against.

Tasks unsolved at least once: breakpoint-automatic, cache-alarm-threshold, cache-silent-target, routing-key-scope, tool-sort-determinism.

## What was not measured, and why

The §7.1 gate needs a second arm. The cache-aware arm on the same model over the
same corpus was queued and never ran: the Anthropic credit balance was exhausted
by the first arm, and every subsequent attempt returned a 400 immediately.

So this establishes a baseline and nothing about the thesis. The gate is refused
rather than failed, and the harness reports it that way, because a gate that
could not be measured has not been failed.

## What it would cost to finish

The cache comparison is the cheap one and the one §7 is named for: same model,
same corpus, and the only difference is whether §6 places markers. At the
observed $0.23 per solved attempt, sixty attempts is
roughly $14.

A tier comparison needs two paid models and roughly a hundred dollars, which is
why it has not been run. A ladder with a free or plan-metered rung cannot be
used as a substitute: those arms bill zero, so cost per solved task is zero and
they win §7.1's cost condition unconditionally. That is an artefact of metering
rather than a result, and the harness says so rather than printing the number.

## Reproducing

```
SB_LIVE=1 SB_EVAL_LADDER=cache SB_EVAL_TASKS=20 SB_EVAL_SEEDS=3 \
  go test ./internal/eval/ -run TestLiveBaselineRuns -v -timeout 170m
```

Each attempt is written to `eval-runs.jsonl` as it finishes. A run that dies
half way through leaves half a measurement, which is what happened to an earlier
three hour run that left nothing at all.
