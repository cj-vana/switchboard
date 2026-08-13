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

**Phases 0 and 1 complete.** What exists is the agent loop and the machinery
under it (canonical provider types, a crash-safe session log, the four core
tools, the permission model, verified sandboxing on macOS and Linux, a plain
REPL), plus all of phase 1: a versioned target catalog with price bands and
cache mechanics, manual tiers, observed cost accounting, two adapters that
reach the same model over different wire formats, and credential storage.

The phase-1 exit gate has been run rather than described. Identical tasks
complete on both pinned targets, and the token estimator's error is measured
and written down in [docs/estimator.md](docs/estimator.md): it undercounts by
18 to 24 percent, always in that direction, and worse as a conversation grows.
Cost reconciliation stays unexercised, because every target this build can
reach is free.

Next: the cache and breakpoint machinery, the router, the eval harness, MCP,
hooks, the Bubble Tea interface, and telemetry. **The routing this is named for
does not exist yet** - tiers are selected by hand.

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

A tier can also bind a model through an OpenAI-compatible endpoint. The surface
names the profile, and there is no default: price, cache behavior, and which
capabilities are real all differ per server, so guessing one would attach the
wrong catalog entry.

```toml
[tiers.t3]
label = "compat"
model = "openaicompat/qwen3.5:9b-mlx"
surface = "ollama"
```

The same model reached two ways is two targets, with two catalog entries and
two costs. That is the point rather than an inconvenience: the compatibility
format discards cache breakpoints and reports no per-model capabilities, so
what it can promise is a property of the route, not of the model.

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

## Credentials

Nothing here needs one yet: every target this build can reach is a local server.
The machinery exists so that the first paid provider does not arrive alongside a
rushed decision about where secrets live.

```
sb auth status             where each configured tier's credential comes from
sb auth login <provider>[/<surface>]    store one
sb auth logout <provider>[/<surface>]   remove one
```

A credential is read from standard input, never from the command line, and is
handed to the platform store over a pipe. It is never written to the config
file, never written to the session log, and has no rendering that shows it: a
credential that reaches a log line or a formatted error prints as a placeholder.

Three places are consulted, in order:

1. **An environment variable.** `SB_<PROVIDER>_<SURFACE>_API_KEY`, then
   `SB_<PROVIDER>_API_KEY`, then the vendor's own name where the provider *is*
   that vendor. An OpenAI-compatible endpoint is not OpenAI, so a target on that
   provider will not pick up `OPENAI_API_KEY`.
2. **A credential helper**, if configured. Its standard output is the
   credential and is never logged or quoted back, even on failure.
3. **The operating system's credential service**: the login Keychain on macOS,
   the Secret Service keyring on Linux.

The environment comes first because a variable is set deliberately, for one
process, usually to override what is on the machine.

```toml
[auth.anthropic]
helper = ["op", "read", "op://vault/anthropic/credential"]
```

There is no encrypted-file fallback and no plaintext one. A mode 0600 file is
access control, not encryption, and on a machine with no keyring the honest
answer is the environment variable or the helper.

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
