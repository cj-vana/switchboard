# Switchboard

Terminal coding agent whose model is a configurable slot rather than a fixed
property of the tool. `switchboard-design.md` is the design of record; it is
authoritative and this file only says how to work inside it.

## Where things are

    cmd/sb/              CLI entry point and the phase-0 REPL
    internal/provider/   canonical message types, Provider interface, adapters
    internal/session/    append-only event log, replay, resume
    internal/execution/  process runner and sandbox capability reporting
    internal/permission/ modes and rules
    internal/tools/      the core tool suite
    internal/agent/      the loop

## Constraints that are not negotiable

**The core knows nothing about terminals.** Nothing under `internal/` may import
a TUI library, write to stdout, or assume a tty. The REPL in `cmd/sb` is a
consumer of the library, and it is the first of several. Retrofitting this is
expensive, which is why it holds from the first commit (design principle 1).

**Adapters never silently drop requested semantics.** When a provider cannot do
what the request asked for, the adapter returns a typed error. Emulating the
missing capability is a decision for a visible policy layer, not something an
adapter does quietly (§5.2).

**A permission prompt is not a sandbox.** Where OS isolation is unavailable or
unverified, automatic execution is disabled rather than approximated by
prompting. `execution.Capability` separates "the mechanism exists" from "we have
verified a policy that actually contains a build" and the permission engine only
trusts the second (design principle 4, §11).

**The prefix is append-only.** Context layout exists to keep provider caches
warm. Anything that rewrites history is a cache-invalidating event and is
scheduled deliberately (§6.1). The zone machinery arrives in phase 2a; until
then, do not add code that mutates already-sent messages.

## Build phase

Phase 0 of §19.2: minimal loop, streaming, `read`/`write`/`edit`/`exec`,
permission model, sandbox capability report, crash-safe session log, one
provider, minimal REPL. The exit gate is that a small verified task corpus
completes safely and sessions resume after forced interruption.

Deliberately absent until their phases: the target catalog, cache and
breakpoint machinery, tiers and routing, MCP, hooks, the TUI, telemetry, and the
`glob`/`grep`/`todo`/`delegate` tools.

## Working here

    go build ./...
    go vet ./...
    go test ./...

Tests must pass without network access or an API key. Provider behavior is
tested against recorded fixtures served by `httptest`; tests that need a live
model are guarded by `SB_LIVE=1` and skipped otherwise.

Planning documents, status summaries, and handoff notes do not get committed.
