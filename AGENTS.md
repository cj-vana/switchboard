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

**An adapter that emits tool-use blocks must report `StopToolUse`.** The loop
executes tool calls only on that stop reason, so an adapter reporting
`end_turn` or `max_tokens` alongside tool-use blocks leaves those calls in the
session with no results, and every later request built from it is malformed.
The Ollama adapter derives the stop reason from whether calls were emitted
rather than trusting the server's `done_reason`, precisely because the server
reports `"stop"` on a turn that ended in a call. Check this first when adding a
provider.

**A permission prompt is not a sandbox.** Where OS isolation is unavailable or
unverified, automatic execution is disabled rather than approximated by
prompting (design principle 4, §11).

There is deliberately no exported boolean for this. `execution.Capability`
carries a `*Confinement`, which is produced only by a self-test that passed on
this machine and is also the thing that wraps the command. Do not add a
`Verified bool` beside it and do not let a caller consult one without applying
the other: "we verified containment" and "we applied containment" have to be the
same fact, or the product reports a sandbox it is not using. `Run` fails closed
when a confinement is set and cannot be applied. See `docs/sandbox.md`.

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

Platform-specific files carry build tags that a host-only build never
exercises, so check the other targets before claiming a change is portable:

    GOOS=windows GOARCH=amd64 go vet ./...
    GOOS=linux GOARCH=amd64 go vet ./...

Tests that drive a POSIX shell or signal a process group are tagged `unix`.

The Linux confinement cannot be exercised from macOS, so changes to it are
verified in a container:

    docker build -f Dockerfile.linuxdev -t sb-linuxdev .
    docker run --rm --privileged -v "$PWD:/src" -w /src sb-linuxdev go test ./...

`--privileged` is needed because Docker's kernel blocks the unprivileged user
namespaces bubblewrap depends on. See `docs/sandbox.md`.

Tests must pass without network access or an API key. Provider behavior is
tested against recorded fixtures served by `httptest`; tests that need a live
model are guarded by `SB_LIVE=1` and skipped otherwise.

Planning documents, status summaries, and handoff notes do not get committed.
