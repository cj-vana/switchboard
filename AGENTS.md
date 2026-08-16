# Switchboard

Terminal coding agent whose model is a configurable slot rather than a fixed
property of the tool. The design of record is the maintainers' design
document, kept outside the public tree; the § references in code comments
point into it, and this file restates the constraints that bind the code.

## Where things are

    cmd/sb/              CLI entry point, the phase-0 REPL, and the phase-3 TUI
    internal/provider/   canonical message types, Provider interface, adapters
    internal/session/    append-only event log, replay, resume
    internal/execution/  process runner and sandbox capability reporting
    internal/permission/ modes and rules
    internal/tools/      the core tool suite
    internal/agent/      the loop
    internal/advisor/    §9.2 run continuously: a second model that watches
                         the loop's observer stream and injects advice at
                         round boundaries; advice, never edits
    internal/mcp/        MCP client over stdio and Streamable HTTP, and the
                         bridge that puts each discovered tool in the registry
                         as mcp__server__tool
    internal/hooks/      user commands at the seams of a tool call; a pre_tool
                         hook blocks on non-zero exit and on timeout
    internal/delegate/   the delegate tool: one level of subagent on a chosen
                         ladder rung, sharing the permission engine; named
                         agent definitions load from .switchboard/agents/
    internal/trust/      per-workspace grants that gate repository-declared
                         MCP servers, hooks, and the language server
    internal/lsp/        a deliberately narrow LSP client: initialize,
                         didOpen, definition, references; the tools take
                         {path, line, symbol} and resolve the column
    internal/checkpoint/ per-turn file snapshots behind /undo; files are
                         restored, messages never are
    internal/config/     the ladder and settings; the TUI owns the file and
                         Save regenerates it, so nothing may depend on
                         comments in config.toml surviving

## Constraints that are not negotiable

**The core knows nothing about terminals.** Nothing under `internal/` may import
a TUI library, write to stdout, or assume a tty. The TUI and the REPL in `cmd/sb`
are consumers of the library: the TUI talks to the loop through `agent.Observer`
and `permission.Asker`, and every turn event crosses as a Bubble Tea message, so
the loop's goroutine never touches UI state directly. Retrofitting this is
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

**The router is rules, and a learned one is not a near-term option.** §8.2
defines every classifier dimension by a measurement procedure against the §8.6
eval corpus, and that corpus is phase 2b. Weights cannot be fit against data
that does not exist, the same section records the null hypothesis that a task
profile loses to a plain scalar, and §19.2 gates a learned router on beating
heuristics after runtime and distribution costs. Running the heuristic is what
produces the evidence to settle it.

**A trigger that needs state the loop does not keep is absent, not guessed.**
`internal/router` detects repeated tool calls, tool error spikes, new test
failure signatures, and hedging, because the observer already carries what each
needs. §8.3 also lists an edit reverted twice and a diff crossing a threshold;
neither is emitted, because the loop keeps no edit history or running diff and
approximating them would escalate on evidence that does not exist.

A failure signature is the first line that looks like a failure, with digits
stripped. Comparing whole outputs would make every retry look new, because
timings and counts differ between two runs of the same broken thing.

**Feasibility is not economics.** A target that cannot hold the context, lacks a
capability, or is not an approved destination is infeasible, not expensive. The
filter checks those before budget so that a target excluded by policy is never
reported as one that was out-priced, and a ceiling is checked against the upper
bound rather than the expectation: a turn affordable on average is not a turn
under a ceiling.

The /budget ceiling is enforced in three places and all three price the same
way, through the §6.4 estimator's upper bound (`cmd/sb/budget.go`): the router
excludes rungs it does not fit, `moveTo` refuses an escalation onto one, and
`Loop.Budget` gates each call before it goes out (§15). The loop's hook takes
a token count and returns an error; the ceiling itself lives with the surface,
because the surface knows what the session has spent and what a dollar is. A
ceiling governs dollars only — a local or plan rung passes the gate, because
the three meterings are never collapsed — and an unpriced target passes too,
with /budget saying so, since a ceiling cannot govern what has no price.

**Fallback is availability, never routing.** A tier's `fallback` list
(§5.4, `probeTierFallback`) is consulted only when the primary cannot be
probed, the rung's identity does not change, and each candidate passes the
same probe a primary does — a fallback that cannot call tools is refused
the same way. Every entry was written into the user's own config, which is
what makes it an approved destination; what the design still demands is
that the substitution renders before content is sent and is recorded on
the session, so every call site of `probeTierFallback` must surface the
note it returns rather than dropping it. Entries resolve the provider's
default serving surface; a non-default-surface fallback is not expressible
and that is a stated limit, not an oversight.

**An outcome is worth less as evidence than it looks.** §8.4's labelling rules
are in `internal/router` because each prevents a specific failure. A clean
completion is weak evidence of sufficiency and none of necessity, which is the
main way a naive router learns to over-provision. An escalation is not a
negative label: provider failure, a phase change, and a bad rule produce the
same event. Abandonment is censored rather than negative.

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

The same rule prices a first-party subprocess tool. `astgrep` wraps the
user's own ast-grep binary, and its permission effect follows confinement:
inside a demonstrated sandbox the call is read-effect and runs wrapped —
the confinement consulted is the confinement applied — while without one it
is execute-effect and approved per call. It is never read-effect unwrapped.
The binary is looked up once at session assembly (`cmd/sb/astgrep.go`), so
the frozen zone never changes shape mid-session, and the tool is absent
rather than broken on a machine without it. `CoreNames()` deliberately
excludes it: a named agent's tool grant must not depend on another
machine's binaries, so a restricted agent loses astgrep with everything
else it did not name, while unrestricted subagents get it. The JSON it
parses was captured against ast-grep 0.45.1, exit 1 is the no-match
convention rather than a failure, and the runner's combined output means
the binary's warnings ride beside the JSON line — the parser separates
them and hands the warnings to the model on a miss, because "your pattern
did not parse cleanly" is the difference between tightening a pattern and
abandoning the tool.

**An external tool is never inside the sandbox.** An MCP server is a process
this program started un-confined, acting wherever it acts, so a bridged call
carries `permission.EffectExternal`: no mode auto-allows it, bypass included,
because bypass suppresses prompts inside a granted sandbox and an external
tool was never inside one. Only an explicit rule (the server's `allow` list)
or a remembered answer lets one run without asking, and the remembered answer
covers the tool, not one byte-exact invocation — that is what the display-only
`Request.Detail` field exists for. A spawned server inherits the parent
environment minus the model credentials; the test that fails if one leaks is
in `internal/mcp/stdio_test.go`.

**A repository's configuration may speak; only a trusted checkout executes.**
`.switchboard/mcp.toml` and `.switchboard/hooks.toml` in a repository are read
only after the user grants trust to that resolved path (`/trust grant`,
`internal/trust`). The same files under ~/.switchboard always run, because
that is the user speaking. Do not add a repository-provided input that starts
a process without routing it through this gate.

The language server sits behind the same gate even though the binary is
the user's own, because the code it chews is the repository's: building
the module graph runs what the workspace directs (toolchain directives,
plugins), unconfined — confinement would deny the caches and network a
server needs. Opening a repository is not permission to run what its
module implies. The client (`internal/lsp`) is deliberately narrow —
initialize, didOpen, definition, references — and answers every
server-initiated request with null rather than leaving the server waiting
on a client that has no configuration to give. The candidate table in
`cmd/sb/lsp.go` holds only servers verified live on a real workspace
(gopls, TypeScript 7's native `tsc --lsp`, pyright), which is the §5.2
profile rule applied to language servers: the TS5-era wrapper is absent
because no TS5 existed on the verification machine to run it against, not
because it was forgotten. The tools' {path, line, symbol} input shape
exists because models copy file:line reliably and invent column numbers
freely. Server start is lazy; tool presence is decided at assembly, which
is what the frozen zone requires.

**MCP discovery is once, at session assembly.** Tool definitions sit in the
frozen zone (§6.1), so a server that changes its tool list mid-session is
noted and deliberately not followed; the next session lists again. Bridged
names are sorted before registration so the frozen-zone ordering never
depends on which server answered first.

**A hook that hangs has answered.** A pre_tool hook blocks the call on
non-zero exit and on timeout both, because a gate that fails open the moment
it hangs is not a gate. Hooks run unconfined and unprompted — they are the
user's standing policy — which is exactly why the repository's hooks file
sits behind the trust grant.

**Delegate depth is one.** A subagent's registry has no delegate tool; an
agent that can recurse is an agent whose cost has no ceiling. Subagents share
the primary's permission engine and asker, their rails render through the raw
observer rather than the watcher so a subagent's stumbles never escalate the
primary, and their sessions live in their own store so /resume never offers a
context that was never the user's. §19.2 phase 6 expects delegation evaluated
against sticky single-primary baselines; that eval has not run, and the tool's
own description does not claim it has.

A named agent is a definition, not a capability. The files under
`.switchboard/agents/` (§13) load without a trust grant because nothing
executes at read time: a definition is a prompt, a default rung, and a tool
grant, and the grant can only narrow — `Restrict` errors on a name outside
the suite, and the sub-registry never held delegate or the bridged MCP tools
to begin with. Discovery is once, at session assembly, sorted by name,
because the definitions ride the delegate tool's schema into the frozen
zone. A session with no definitions renders the schema byte-identical to
what it was before the feature existed; the test that guards that is the
cache promise, not the comment. A definition naming a rung the ladder lacks
runs on the default rung with a note, rather than erroring on every call.

There is deliberately no exported boolean for this. `execution.Capability`
carries a `*Confinement`, which is produced only by a self-test that passed on
this machine and is also the thing that wraps the command. Do not add a
`Verified bool` beside it and do not let a caller consult one without applying
the other: "we verified containment" and "we applied containment" have to be the
same fact, or the product reports a sandbox it is not using. `Run` fails closed
when a confinement is set and cannot be applied. See `docs/sandbox.md`.

**The prefix is append-only.** Context layout exists to keep provider caches
warm. Anything that rewrites history is a cache-invalidating event and is
scheduled deliberately (§6.1). This is why /undo restores files and never
messages: `internal/checkpoint` snapshots what write and edit are about to
change, per turn, and a restored file already forces a re-read through the
stale check, while the conversation that produced the change stays exactly
as sent. Do not add an undo path that mutates already-sent messages.

An unchanged file is not read into the context twice. A full, uncapped
read arms a per-file record, and a later full read of byte-identical
content answers with a short marker instead of the bytes, which is §6.7's
own framing: hashing prevents re-injection, never relocation — the content
already sits in the prefix, exactly where the cache wants it. The skip is
armed only by a complete read (a partial read updates the stale check and
proves nothing about what the context holds), a mutation or external
change disarms it by hash inequality, and /undo and every session swap
clear it alongside the read versions, in the same struct so the two cannot
drift. A skipped read still refreshes the stale check, so write-after-read
behaves identically either way.

Going back in the conversation is /fork, for the same reason:
`internal/session/fork.go` copies a log's prefix into a new session and
never writes the source, so the fork's messages are byte-identical to what
was already sent and a warm provider prefix stays warm. The cut has to land
on a turn boundary — the first dropped message must be the user message
that opened its turn — because a conversation cut mid-turn leaves tool
calls without results and every request built from it is malformed (§10.3).
Fork branches the log only: files are /undo's job, and the checkpoint
recorder is process-scoped, so it keeps working across the swap.

## Build phase

Phase 0 of §19.2: minimal loop, streaming, `read`/`write`/`edit`/`exec`,
permission model, sandbox capability report, crash-safe session log, one
provider, minimal REPL. The exit gate is that a small verified task corpus
completes safely and sessions resume after forced interruption.

Phase 3's TUI is built and is the default surface. Its phase-3 obligations from
§14 hold: streaming text renders through a plain fast path and is re-rendered
once per completed block through glamour, completed entries cache per width so
repaints never re-render markdown, and diffs highlight once at load. Keep it
that way.

Phase 4's extensibility has landed — MCP over stdio and Streamable HTTP,
hooks, the workspace-trust flow, named subagent definitions — along with
the `glob`/`grep`/`todo` tools and phase 6's `delegate`, each under the
constraints above. Deliberately absent until their phases: the learned
router (phase 7 gates it on beating the heuristic) and everything in the
phase 8 platform program.

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
