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
reports `"stop"` on a turn that ended in a call. The OpenAI-compatible adapter
does the same, for the same reason. Check this first when adding a provider.

**A serving surface is part of target identity, not a label on it.** The same
model reached through a different endpoint is a different target: different
adapter, different capability evidence, different catalog entry, different
price. `openaicompat/ollama/qwen3.5:9b-mlx` and `ollama/local/qwen3.5:9b-mlx`
are the same weights and are not interchangeable to the router.

For the OpenAI-compatible adapter the profile name *is* the surface, because a
profile is a claim about one server's behavior. There is no default: `New` on
an unknown profile is an error rather than a fall back to the generic floor,
since a typo would otherwise quietly disable the capabilities the user asked
for. A profile nobody has run against a real server does not belong in the map.

**One component owns cache-marker placement.** `internal/breakpoint` decides
where markers go and whether to place any at all, because the four reachable
surfaces want four different things: explicit markers with a limit and a
minimum, a routing key, nothing at all, and no cache whatsoever. Spread that
across call sites and each one grows its own wrong assumption.

A declined marker is recorded rather than dropped. A marker below a target's
minimum is accepted by the server and silently does nothing, with no error
either way, so a logged reason is the only thing separating an expected miss
from a bug.

**Cache state comes from what the provider reported, never from what was
sent.** `internal/cachestate` records observations and nothing else. Sending a
marker is not evidence anything was cached: a marker below the minimum is
accepted and does nothing, an entry can be evicted early, and a target may
report nothing at all. All three look identical from the request side.

A write observation and a read observation are different facts, and retention
is modelled rather than known: providers describe a TTL as a floor, so the
tracker reports a probability that decays instead of asserting an expiry it
cannot see. A target with no cache accounting stays Unknown forever, because
silence is not evidence of a miss and recording one would leave the alarm on
permanently.

**A capability claim gets tested against the target, not against its docs.**
Everything the Anthropic adapter asserts was confirmed with a live request
first: that this model rejects `adaptive` thinking and takes a token budget,
that the one-hour cache TTL needs no beta header and bills to its own bucket,
that replaying a thinking block without its signature is refused while dropping
the block is accepted, and that a tool result is a user message because there is
no tool role. Each of those contradicted a reasonable guess.

**Wire formats get captured before they get mapped.** Both adapters were
written against a recorded response from a running server, checked into
`testdata/`. Both captures contradicted the documentation: Ollama reports
`done_reason: "stop"` on tool-call turns, and the compatibility endpoint sends
its usage chunk *after* `finish_reason`, so a terminal event emitted at
`finish_reason` reports zero tokens for the turn.

**A credential has no rendering that shows it.** `credential.Secret` keeps its
value unexported and redacts in `String`, `GoString`, `MarshalJSON`, and
`MarshalText`, so a secret that reaches a log line, a formatted error, or the
JSON session record prints as a placeholder. Reading it takes `Expose()`, which
is deliberately easy to grep for. Do not add a field, an accessor, or a struct
tag that would let one out by accident.

Secrets go to the platform store over a pipe, never in argv, because every
process's command line is readable by every user on the machine. Both backends
have a test that fails if the value appears in a recorded argv; that test is the
guarantee, not the comment above the code.

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

**A pid that exists is not a process that is running.** Signal 0 asks only
whether the kernel still has an entry, and it keeps one for a zombie until
something reaps it. In a container with no init process nothing does, so a
descendant the runner killed correctly answers "still here" forever. Any test
that probes for a surviving process has to read its state, not just its pid;
`processIsRunning` in `runner_test.go` is the one that does, and it is why the
container loop above needs no `--init`.

The same image carries a Secret Service, so the Linux credential store is
verified against a real keyring rather than a description of one:

    docker run --rm -v "$PWD:/src" -w /src sb-linuxdev bash -c '
      eval "$(dbus-launch --sh-syntax)"; export DBUS_SESSION_BUS_ADDRESS
      printf "p\n" | gnome-keyring-daemon --unlock --components=secrets >/dev/null 2>&1 &
      sleep 2; SB_LIVE=1 go test ./internal/credential/'


The phase-1 exit gate lives in `internal/gate` and is run, not described:

    SB_LIVE=1 go test ./internal/gate/ -run TestExitGate -v -timeout 40m

It runs the same corpus on both pinned targets and measures the token estimator
against what each server reported. Its companion,
`TestEstimatorStaysWithinTheDocumentedBound`, defends the numbers in
`docs/estimator.md`; if a change to the system prompt, the tool schemas, or the
estimator moves the ratio, that test fails and the document is what has to be
updated. Do not widen the band to make it pass.

Tests must pass without network access or an API key. Provider behavior is
tested against recorded fixtures served by `httptest`; tests that need a live
model are guarded by `SB_LIVE=1` and skipped otherwise.

Planning documents, status summaries, and handoff notes do not get committed.
