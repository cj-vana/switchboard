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
Cost reconciliation was unexercised at the time, because every target this
build could then reach was free.

Phase 2a is underway. The Anthropic adapter is the target the cache work needs:
it is the first one here that can render a cache plan, and the first that
reports cache writes and reads as separate observations rather than not at all.
It is also the first whose token count is exact, because that API will answer
the question rather than leaving it to be estimated.

Phase 2a is built: context zones, the breakpoint manager, the cache tracker,
the cost model, and the heuristic router with a sticky primary. `sb` now picks a
tier and tells you why, and a tier you name still wins.

**The routing is rules, not a model, and that is the design's own sequencing.**
§8.2 defines every classifier dimension by a measurement against an eval corpus
that phase 2b builds, states the null hypothesis that a task profile loses to a
plain scalar, and gates a learned router on beating heuristics once runtime and
distribution costs are counted. Running the heuristic is what produces the
evidence to settle it.

**Phase 2b is run, and the gate failed.** A thirty-task hand-written corpus
with executable verifiers, twenty tasks at three seeds across three arms, and
the verdict is recorded in [docs/eval.md](docs/eval.md).

The failure is about the ladder, not the thesis. The rung assumed to be
stronger solves less on 13 of 20 tasks and is twice as slow, so every escalation
moved work from a model solving 97% to one solving 58%. §8.6 says tier labels
are derived from the Pareto front and "not assigned from model reputation";
this ladder was assigned from reputation and the first run falsified it.

That is the harness working. A ladder ordered wrongly makes routing worse than
not routing, and nothing short of running the corpus would have shown it.

The cost half of the gate is still unmeasured: both rungs are plan-metered, so
every arm bills zero and the cost condition cannot separate them.

Next: derive the ladder empirically from the data now in hand, then MCP, hooks,
and telemetry. The Bubble Tea interface is built: phase 3's TUI is the default
surface, with the REPL behind `-repl`. **The routing this is named for
does not exist yet** - tiers are selected by hand.

## Running it

Install the latest release (macOS and Linux; verifies the release checksums
and installs to `~/.local/bin`):

```
curl -fsSL https://raw.githubusercontent.com/cj-vana/switchboard/main/install.sh | bash
```

Or build from source. Needs Go 1.26 and a local [Ollama](https://ollama.com) server.

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

OpenAI is its own provider rather than a profile of the compatible adapter,
because target identity is provider, surface, and model: sharing a decoder is a
fact about this codebase, not about where a request goes or whose key pays for
it.

```toml
[tiers.t4]
model = "openai/gpt-5-mini"
```

Nothing in that adapter has been run against the live API yet, so it claims no
reasoning support and there is no catalog entry to price it with. Both get
filled in from a capture rather than from documentation, and until then `sb
-tiers` reports it as unpriced instead of guessing.

The same model reached two ways is two targets, with two catalog entries and
two costs. That is the point rather than an inconvenience: the compatibility
format discards cache breakpoints and reports no per-model capabilities, so
what it can promise is a property of the route, not of the model.

Kimi Code serves the Messages API, so it is driven by the same adapter as
Anthropic and needs only a key:

```toml
[tiers.t5]
model = "kimi/k3-256k"
```

```
sb auth login kimi/coding
```

```
sb                         start on the lowest tier
sb -tiers                  show the ladder and what each tier costs
sb -tier t2                start on a specific tier
sb -model <model>          bind an Ollama model directly, bypassing tiers
sb -repl                   the line-oriented shell instead of the TUI
sb --continue              resume the most recent session here
sb --resume <id>           resume a specific session
sb --sessions              list sessions for this directory
sb -p "<prompt>"           run one prompt and exit
sb -mode plan              read-only: no writes, no commands
sb -think high             ask for reasoning output
```

## Credentials

Local targets need none. Anything billed does, and the machinery was built
before the first paid provider arrived rather than alongside it.

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
   that vendor. `openai/first-party` picks up `OPENAI_API_KEY`; a target on the
   `openaicompat` provider does not, because a compatible endpoint can point at
   any server and a vendor's key should not follow it there.
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

### Pointing a provider somewhere else

A provider can be reached at a different address without becoming a different
provider. A gateway, an Azure deployment, a corporate egress proxy, and a
self-hosted endpoint all need this.

```toml
[providers.openai]
base_url = "https://gateway.example.com/v1"
```

This does not change target identity: the credential and the catalog entry
still follow the provider name, so redirecting somewhere that prices
differently is you asserting you know that.

### OAuth

A provider that publishes an authorization-code flow can be logged in to
instead. The tokens go to the same platform store as an API key, under their
own name so a login cannot overwrite a key, and are refreshed automatically a
minute before they expire.

```toml
[auth.<provider>.oauth]
client_id = "..."
authorize_url = "https://.../authorize"
token_url = "https://.../token"
scopes = ["openid", "offline_access"]
```

```
sb auth oauth login <provider>[/<surface>]
sb auth oauth logout <provider>[/<surface>]
```

There is no `client_secret` field. A command-line program cannot keep a secret,
so this is a public client and PKCE stands in for one. Adding the field would
only invite storing a secret in the config file, which is the thing the rest of
this section exists to prevent.

### Using a ChatGPT plan

`openai/subscription` reaches the backend behind a ChatGPT plan with an OAuth
token instead of the developer API with a key. It is a separate serving surface
because it is a separate endpoint, a separate credential, and a flat
subscription rather than per-token billing.

```toml
[tiers.t2]
model = "openai/gpt-5"
surface = "subscription"
```

```
sb auth oauth login openai/subscription
```

Models are whatever the plan offers, and the slugs are not the developer API's
names. `sb -tiers` will not guess them; the endpoint lists them.

**Read this before using it.** The OAuth client this presents is the one
OpenAI's own Codex CLI registers. Switchboard is not affiliated with or endorsed
by OpenAI, and this is not a flow OpenAI publishes for third-party clients: it
works because the authorization server accepts that registration, not because
anyone granted permission to use it. OpenAI's Terms of Use govern your account
whatever a program claims to be, and accounts have been actioned for this. The
risk is yours and it is not hypothetical.

A client you register yourself always wins. Anything under
`[auth.openai.oauth]` overrides the bundled one.

Inside a session the default surface is the TUI: streaming markdown, a
virtualized transcript, an always-on status line with the tier, target,
permission mode, session cost, and a context-window gauge, and interactive
permission prompts. Router decisions render inline, collapsed to one line;
ctrl-o expands the last route or tool entry.

Commands: `/help` lists them all. `/t1`, `/t2`, … switch tier, and
`/t2 <prompt>` runs one prompt on a tier and returns. `/tiers` shows the
ladder, `/mode`, `/cost`, `/session`, `/sandbox` do what they say, `/resume`
picks up an earlier session, `/clear` starts fresh, `/diff` reviews
uncommitted changes, `/copy` puts the last response on the clipboard,
`/theme` switches dark and light, `/update` self-updates, `/exit` leaves.
Typing `/` opens autocomplete. Shift-tab cycles the permission mode, ctrl-t
opens the tier picker, esc interrupts the turn, ctrl-c twice exits, and
sending a message mid-turn queues it. The line-oriented REPL remains behind
`-repl` for scripting and gates; `-p` keeps the plain renderer either way.

The TUI checks for a newer release once at startup and says so in the
transcript when one exists. The check names nothing but the running version,
and `[updates] check = false` in the config or `SB_NO_UPDATE_CHECK=1` turns it
off. `/update` downloads the build for this platform, verifies it against the
release's checksums, and replaces the binary atomically; installs managed by a
package manager defer to it. Signed update metadata is §18's bar and arrives
with the release pipeline — until then the checksum proves integrity, not
authenticity.

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
