# Switchboard

A terminal coding agent whose model is a configurable slot rather than a fixed
property of the tool.

The bet is that an agent which reasons about *which* model to use, and about
what that choice costs given provider cache state, produces better outcomes per
dollar than one that always calls the same model. Everything else is
infrastructure for testing that claim, and the claim gets a predeclared
falsification gate in phase 2 before the rest is built.

`switchboard-design.md` is the design of record. It is long, it is meant to be
attacked, and it is ahead of the code.

## Status

**Phase 0 of the build sequence.** What exists is the agent loop and the
machinery under it: canonical provider types, an Ollama adapter, a crash-safe
session log, the four core tools, the permission model, and a plain REPL.

Deliberately absent, each waiting for its phase: the target catalog, the cache
and breakpoint machinery, tiers and routing, the eval harness, MCP, hooks, the
Bubble Tea interface, and telemetry. The routing this is named for does not
exist yet.

## Running it

Needs Go 1.26 and a local [Ollama](https://ollama.com) server.

```
go build -o sb ./cmd/sb
ollama pull qwen3.5:9b-mlx
./sb -model qwen3.5:9b-mlx
```

The model has to support tool calling or it cannot drive the loop; `sb` checks
and says so rather than failing halfway through a turn. Run `sb` with no
`-model` to see what your server has.

```
sb -model <model>          start a session in the current directory
sb --continue              resume the most recent session here
sb --resume <id>           resume a specific session
sb --sessions              list sessions for this directory
sb -p "<prompt>"           run one prompt and exit
sb -mode plan              read-only: no writes, no commands
sb -think high             ask for reasoning output
```

Inside a session: `/help`, `/mode`, `/cost`, `/session`, `/sandbox`, `/exit`.
Ctrl-C cancels the current turn and returns you to the prompt; the session
stays resumable.

## There is no sandbox yet, and that matters

Switchboard will not present a permission prompt as though it were isolation.
No profile has passed the containment spike on any platform, so **every command
requires per-action approval**, in every mode, including `bypass`. File edits
can be auto-approved with `-mode acceptEdits`; commands cannot.

This is not an oversight to be worked around. A sandbox has to be shown to
confine a real build while denying credential stores, agent sockets, and
network egress, against the toolchains developers actually run. Until that is
demonstrated, automatic execution is not offered, because shipping it on
isolation nobody has verified is exactly the substitution the design forbids.

Commands run with the harness's own provider credentials stripped from their
environment. That is credential hygiene, not containment, and it does not
change anything above.

On Windows there is additionally no process-group cleanup, so a timed-out
command may leave descendants running. Windows is a plan-and-approve
environment until native containment meets the same bar as the other platforms.

## Working on it

```
go build ./...
go vet ./...
go test ./...
```

Tests run offline with no API key. Provider behavior is checked against a
recorded capture of a real Ollama stream; `SB_LIVE=1` adds a test against a
running server.

`AGENTS.md` has the constraints that hold across the codebase. The short
version: nothing under `internal/` may know that terminals exist, adapters
return typed errors instead of quietly dropping semantics they cannot honor,
and no code path grants automatic execution without verified containment.

## License

Open source; license not yet chosen.
