# Contributing

Patches are welcome. This file says how to work in the repo without fighting
it; the design reasoning lives in `switchboard-design.md` and the standing
constraints in [AGENTS.md](AGENTS.md). When those documents and a change
disagree, the documents win until they are themselves changed.

## Building and testing

Go 1.26, no other toolchain.

```
go build ./...
go vet ./...
go test ./...
```

That runs offline with no API keys. Provider behavior is tested against
recorded captures of real streams, not against mocks of what the docs claim.

Tests that talk to real servers are gated:

```
SB_LIVE=1 go test ./...                       # needs a local Ollama server; some spend money
docker build -f Dockerfile.linuxdev .          # Linux sandbox and keyring tests
```

Cross-platform correctness is part of the ordinary bar: `GOOS=windows go vet
./...` and `GOOS=linux go vet ./...` both have to pass.

## The constraints that are not up for debate

The full list with reasoning is in [AGENTS.md](AGENTS.md). The four that
catch most newcomers:

1. Nothing under `internal/` may know that terminals exist. The TUI and the
   REPL are consumers of the library, and every turn event crosses into the
   UI as a message.
2. Adapters never silently drop requested semantics. When a provider cannot
   do what was asked, the adapter returns a typed error; emulating the gap is
   a visible policy decision, not an adapter's quiet favor.
3. An adapter that emits tool-use blocks must report `StopToolUse`, whatever
   the server's own stop reason says. The loop executes tools only on that
   signal, and getting it wrong corrupts every later request in the session.
4. A serving surface is part of target identity. The same model through a
   different endpoint is a different target with its own price, capability
   record, and cache behavior.

## Adding a provider

Read `internal/provider/kimi/kimi.go` first; it is small and its comments
explain what a new adapter owes. In short: establish behavior by asking the
real endpoint, record a capture for the offline tests, add a catalog entry
with honest metering (local, plan, and per-token are three different facts),
and wire the credential through `internal/credential` so resolution stays in
one place. A live test gated on `SB_LIVE` proves the adapter against the
real server.

## Commit messages

Look at `git log` before writing one. Messages here explain why the change
is shaped the way it is, in prose, and the subject line is a sentence about
intent rather than a category tag. A message that would not help someone
reading the change in two years is not done.

## What does not go in the repo

Planning artifacts: implementation plans, status summaries, handoff notes,
per-PR evidence dumps. The `.gitignore` blocks the common shapes on purpose.
If part of a plan is still true after the work ships, it is documentation;
rewrite it under `docs/` and drop the plan. Everything else belongs in the
pull request description.

Secrets, obviously. `git grep` for your key before you push, and know that
the credential machinery is designed so a key never lands in a file this
repo tracks.

## Evals

The routing claims are measured, not asserted. `internal/eval` holds the
corpus and the harness; runs journal to disk as they happen, and a verdict
can be recomputed from a journal without paying for the corpus again.
[docs/eval.md](docs/eval.md) shows the reporting conventions, including how
to say what a run cannot establish. If your change touches routing, say
what you measured.

## Releases

Cutting a tag is cutting a release: `v*` builds all six platforms, packages
them beside a `checksums.txt`, and publishes. `install.sh` and the
self-updater consume exactly that layout, so changes to either have to keep
the other working. Every release so far has been proven by installing the
previous version and letting it update itself; keep that true.
