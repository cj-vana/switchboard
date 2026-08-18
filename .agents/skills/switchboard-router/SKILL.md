---
name: switchboard-router
description: Change, debug, or evaluate Switchboard routing, escalation, cache, budget, and route telemetry. Use for work in internal/router, turn_route, watcher/observer lifecycle, routing evals, tier moves, retries, races, or learned-router proposals.
---

# Switchboard Router

Read the routing and budget constraints in `AGENTS.md` and the evidence gate in `docs/eval.md` before editing.

## Work from the production decision seam

1. Trace the actual user-turn path through request assembly, feasibility, selection, provider binding, model calls, tool batches, and route recording. Do not infer production behavior from a unit-only `router.Input`.
2. Build features from the request that will really be sent: replayed messages, prompt, system and tool zones, attachments, live probe capabilities, cache state, and persisted budget debt.
3. Apply hard feasibility before economics. Reject context overflow, missing vision/tools, unapproved destinations, unavailable providers, and worst-case budget overflow before scoring.
4. Make the primary decision once per user turn. Feed escalation evidence at completed model-call boundaries, including zero-tool rounds; never count individual parallel tool results as model calls.
5. Prepare fallible probes and checks first, then atomically commit sticky rank and live provider/target/cache binding. A refused or stale move must not mutate state or render a success notice.

## Preserve accounting and evidence

- Reserve conservative retry cost durably before a provider send. Settle it after the outcome is recorded. Carry debt through retry, fork, delegate, race, restart, and logging failures.
- Keep cache routing keys and warmth attached to the exact served target. A move must rebind both provider and cache state.
- Record structured features from observed state. Use an explicit unavailable value when verification or a feature was not measured; never encode absence as a false zero.
- Keep production and eval routing behind the same injectable interface and event semantics. Reject duplicate/missing matrix cells and stale configuration identities.

## Test the boundaries

Add regressions at the real loop or turn boundary for:

- zero, one, and many tool calls per model round;
- concurrent same-name tool calls and observer replacement;
- canceled and stale TUI planning results;
- context, capability, budget, and fallback boundaries;
- failed probes, stale proposals, and persistence/restart;
- candidate-order permutations and production/eval parity.

Run focused normal and race tests for `internal/router`, `internal/agent`, `internal/session`, `internal/eval`, and `cmd/sb` before the repository-wide shipcheck.

Do not train or ship a learned router unless a clean, configuration-bound corpus contains a real decision frontier and an out-of-sample candidate beats the heuristic after runtime and distribution cost. Report a failed gate as evidence, not as missing implementation.
