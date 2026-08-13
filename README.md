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

**Phase 0 complete; phase 1 in progress.** What exists is the agent loop and
the machinery under it (canonical provider types, an Ollama adapter, a
crash-safe session log, the four core tools, the permission model, verified
sandboxing on macOS and Linux, a plain REPL), plus the first three pieces of
phase 1: a versioned target catalog with price bands and cache mechanics,
manual tiers, and observed cost accounting.

Still ahead in phase 1: an OpenAI-compatible adapter, credential storage, and
the exit-gate measurement. Then the cache and breakpoint machinery, the router,
the eval harness, MCP, hooks, the Bubble Tea interface, and telemetry. **The
routing this is named for does not exist yet** — tiers are selected by hand.

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

Bind a tier ladder in `~/.switchboard/config.toml`:

```toml
[tiers.t1]
label = "light"
model = "ollama/qwen3.5:9b-mlx"

[tiers.t2]
label = "deep"
model = "ollama/qwen3.6:27b-mtp-q4_K_M"
effort = "high"
```

```
sb                         start on the lowest tier
sb -tiers                  show the ladder and what each tier costs
sb -tier t2                start on a specific tier
sb -model <model>          bind an Ollama model directly, bypassing tiers
sb --continue              resume the most recent session here
sb --resume <id>           resume a specific session
sb --sessions              list sessions for this directory
sb -p "<prompt>"           run one prompt and exit
sb -mode plan              read-only: no writes, no commands
sb -think high             ask for reasoning output
```

Inside a session: `/t1` and `/t2` switch tier, `/tiers` shows the ladder, and
`/help`, `/mode`, `/cost`, `/session`, `/sandbox`, `/exit` do what they say.
Ctrl-C cancels the current turn and returns you to the prompt; the session
stays resumable.

## What the sandbox does and does not do

Switchboard will not present a permission prompt as though it were isolation. A
mode can only grant automatic execution on a host where confinement has been
demonstrated, and "demonstrated" means a self-test passed on that machine, at
that OS build, against that profile.

**macOS and Linux** confine commands, with Seatbelt and bubblewrap
respectively. A confined command writes only inside the workspace, the temp
directory, and build caches; cannot read the enumerated credential stores or
reach the daemon that hands them out (the Keychain on macOS, the session bus on
Linux); cannot use the ssh agent; and cannot reach the network beyond loopback.
Loopback is allowed because test suites stand up fixture servers, and that is
most of what an agent runs. With that verified, `-mode bypass` runs commands
without prompting.

Linux needs `bubblewrap` installed and a kernel that permits unprivileged user
namespaces. Without either, it falls back to per-action approval and says so.

Network access off the machine is a separate grant that always prompts, even
when confinement is verified. The sandbox governs what a command reads and
writes; it cannot judge whether sending your workspace to the internet is what
you meant.

`docs/sandbox.md` documents the profile, including what it deliberately
does not protect against: reads leak by default outside an enumerated deny
list, and writable build caches are a persistence vector between commands.

**Windows** has no verified containment, so every command there requires
per-action approval in every mode, `bypass` included. It additionally has no
process-group cleanup, so a timed-out command may leave descendants running. It
stays a plan-and-approve environment until that meets the same bar.

Commands everywhere run with the harness's own provider credentials stripped
from their environment. That is credential hygiene rather than containment, and
it is not what any of the above rests on.

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
