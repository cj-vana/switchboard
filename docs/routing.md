# Routing and the model ladder

Switchboard routes work across an ordered ladder of model targets. The current
router is deterministic. It chooses a feasible tier before each user turn and
can move upward when the running turn produces evidence that the current tier
is stuck.

Tier configuration, profiles, and provider targets are covered in
[Installation and configuration](configuration.md).

## Opening a turn

An interactive process starts on the first reachable tier because no user
request exists yet. That bootstrap is not recorded as a routing decision.

Immediately before a user turn, Switchboard assembles the request it would
send and routes that request. The inputs include:

- the frozen system prompt and tool schemas;
- replayed messages and the new user message;
- file and image attachments;
- current provider capabilities, including tools and vision;
- context-window fit;
- cache state;
- the remaining dollar budget, including retry reserve.

A tier that fails a capability, context, availability, or hard-budget check is
not eligible. A user pin still passes these checks.

Use `/t3` to pin the session to tier 3. `/tier auto` removes the pin. A command
such as `/t3 fix the flaky test` runs one prompt on that feasible tier, then
returns to the previous routing state.

## Movement during a turn

The sticky escalation policy watches repeated identical tool calls, tool error
spikes, new test-failure signatures, and hedging. A new failure reported by an
armed `/watch` command can contribute the same signal as a test run by the
model.

Signals may arrive while tools are running, but a move is evaluated only after
the model round and its tool work finish. Switchboard first prepares the new
provider binding and rechecks capabilities, context, and budget for the request
the destination would receive. The provider and sticky tier change together.
A failed probe, stale proposal, or rejected hard check leaves both unchanged.

Every applied move appears in the transcript. `/why` reports the opening
decision, rejected candidates, later moves, and the session cost repriced on
the other tiers.

## Fallbacks

A tier can list fallback targets. If the primary server is unavailable or the
model is missing, Switchboard probes the fallbacks in order and records the
substitution before sending content.

A fallback is an availability substitution. It does not change the tier's
meaning or count as a router move. Each fallback must pass the same capability,
context, and budget checks as the primary.

## Cache state

The router tracks the prefix a target is likely to hold. `/cache` reports the
active target's eligible prefix, modeled hit probability, reason, observed hit
count, and repeated-miss warnings. Modeled values are labeled as estimates.
Providers that do not report cache accounting remain unknown.

When a tier change abandons a warm target, the transcript reports the observed
warm prefix, modeled hit probability, and estimated value of resending that
prefix cold. It omits a number when the inputs cannot support one.

The token estimator and its measured error bounds are documented in
[Token estimator error](estimator.md).

## Budgets and metering

Switchboard keeps three forms of metering separate:

- local execution, which has no provider bill;
- plan or subscription quota;
- per-token dollar billing.

`/budget 2.50` sets a persistent dollar ceiling. The gate uses a conservative
upper bound. It applies before the opening route, before an escalation, and
before each provider call. Lowering the ceiling during a turn constrains later
rounds in that turn. Local and plan targets are not converted into dollar
values.

The accounting commands use the session log rather than reconstructed guesses:

| Command | Output |
| --- | --- |
| `/estimate <prompt>` | Low, expected, and high cost for the next assembled request on each tier; the active tier includes modeled cache warmth and other tiers are priced cold |
| `/cost` or `sb cost` | Current or recorded session totals, preserving local, plan, and dollar units |
| `/cost turns` | Billed calls, tokens, and dollars grouped by user turn; compaction, learning, advising, and command-approval calls remain in labeled purpose buckets; non-dollar work keeps its real metering |
| `/cost rungs` | The recorded session repriced cold on every tier, with context-infeasible calls reported instead of priced |
| `/stats` or `sb stats` | Workspace lifetime totals as routed and repriced on the current ladder |
| `sb stats all` | Totals across every recorded workspace, grouped by the workspace names stored in log headers |
| `/ladder` or `sb ladder` | Where turns opened, whether they stayed, and where moved turns ended |

Race losers and forks count in cost and stats because they made provider calls.
Subagents use their own session stores. Rung counterfactuals stay scoped to one
workspace and ladder.

## Paired routing evidence

`/race review this diff` forks the current session and runs the prompt on the
current tier and the next tier in parallel. `/race t3 ...` chooses the other
lane, and `/race t2 t3 ...` names both. Both branches are enforced read-only
until the user selects a result. The selected branch becomes the session; the
other remains resumable.

A tie keeps the lower configured tier. The verdict and both routes are
recorded. `/races`, `sb races`, and `/races all` or `sb races all` aggregate
those paired results at session, workspace, or global scope.

The production router does not train on race results automatically. Collection
and evaluation are separate so a partial corpus cannot change live behavior.

## Why there is no learned router

The historical evaluation journal does not currently provide a clean
multi-tier capability frontier. Its diagnostic projection has only one useful
tier, so it contains no routing choice worth fitting.

A learned model can ship only after a clean, harder corpus produces at least
two useful tiers and the candidate beats the deterministic heuristic after
runtime and distribution costs. The current implementation therefore keeps the
rules-based router and records the evidence needed to test a future candidate.

See [Routing evaluation](eval.md), [Head-to-head results](head-to-head-2026-08-16.md),
and [Product comparison](comparison.md) for the measured evidence.
