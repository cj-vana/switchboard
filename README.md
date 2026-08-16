# Switchboard

A terminal coding agent where the model is a slot, not a fixed property of
the tool.

<img src="docs/tui.svg" alt="Switchboard running a session: the ladder in heat colors, tool rails, a route escalation, and the status bar" width="812">

You configure a ladder of models, cheapest at the bottom: a small local model
on t1, a bigger one on t2, a subscription model on t3. Switchboard starts
work low, watches how the work is going, and moves up the ladder when the
evidence says the current rung is stuck. Every move is shown as it happens,
`/why` explains any decision after the fact, and the color system is the
ladder itself: each rung has a stable heat color, cool teal at t1 running
warmer toward amber, worn by every surface that touches routing.

The bet behind it, stated up front so you can judge it: an agent that
reasons about which model to use, and what that choice costs given provider
cache state, produces better outcomes per dollar than one that always calls
the same model. The bet gets measured, not assumed. The eval harness in this
repo has already falsified one assumed ladder, and that story is in
[docs/eval.md](docs/eval.md).

## Install

macOS and Linux. The script verifies release checksums before installing to
`~/.local/bin`:

```
curl -fsSL https://raw.githubusercontent.com/cj-vana/switchboard/main/install.sh | bash
```

Or build from source with Go 1.26: `go build -o sb ./cmd/sb`.

Switchboard keeps itself current. The TUI checks for a release at startup
and, by default, installs it in the background with the same checksum
verification; the running process is untouched and the next start runs the
new binary. Installs owned by a package manager are detected and left alone.
`sb update` does the same from a script, `/update channel beta` follows
prereleases, and `/update auto off` reduces the whole thing to a notice.

## First run

Run `sb` in a project. With nothing configured, it opens a setup checklist:
every provider it can reach, each with its live standing. The local Ollama
server reports how many models it has pulled. Providers that need a key take
one in a masked prompt and store it in the OS keychain. If Codex CLI is
signed in on the machine, one pick wires that login in as OpenAI's
credential. Then you pick the model t1 starts on, and you are in a session.

Nothing requires editing a file, then or later. `/models` browses what your
server and the catalog offer and binds rungs. `/login` and `/logout` manage
keys. `/setup` reopens the checklist. `/theme`, `/think`, `/update`, and
`/compact` settings persist themselves. The config is ordinary TOML at
`~/.switchboard/config.toml` and hand-editing still works, but the tool
writes it, and the file's own header says so.

## The ladder

```toml
[tiers.t1]
label = "light"
model = "ollama/qwen3.5:9b-mlx"

[tiers.t2]
label = "deep"
model = "ollama/qwen3.8:27b-mlx"
effort = "high"

[tiers.t3]
label = "kimi"
model = "kimi/kimi-for-coding-highspeed"

[tiers.t4]
label = "codex"
model = "openai/gpt-5.6-sol"
surface = "subscription"
```

Sessions start on the bottom rung; a visible escalation beats a silent
spend. `/t3` switches, `/t3 fix the flaky test` runs one prompt there and
returns, and mid-task the escalation policy moves the primary on its own
signals: repeated identical tool calls, error spikes, new failure
signatures, hedging. Each move renders inline with its reason.

`/why` answers the question no other tool can be asked: how the current tier
was chosen, what was ruled out, every move this session made, and this
session's tokens priced on every other rung.

Cost stays honest about what money is. A local model consumes nothing
scarce, a plan-metered model consumes quota, and a per-token model consumes
dollars. The three are never collapsed into "free," because telling a router
they are the same thing teaches it the wrong lesson about two of them.

## In a session

The input grammar is the one the neighboring tools converged on. `@path`
completes file names and attaches contents. `!cmd` runs a shell command as
you, immediately, no model in the loop, with the output carried into the
next turn. A trailing `\` continues the line, ctrl+g opens the prompt in
`$EDITOR`, prompt history persists per workspace, and ctrl+r searches it.
Messages sent mid-turn queue and run when the turn finishes.

Beyond the expected commands, a few are Switchboard's own:

- `/advisor` sets a second model watching the session through the loop's
  own event stream. It triggers on the same stuck-agent signals as the
  router, consults off the worker's goroutine, and injects its advice into
  the running turn at the next safe seam. Advice, never edits, bounded per
  turn. `[slots] advisor = "t2"` turns it on for every session.
- `/compact` summarizes the session into a fresh context, and does it
  automatically when the last request crosses 85% of the window, measured
  from what the provider actually saw rather than an estimate. A
  `[slots] summarizer` binding lets one model own the summaries whichever
  rung is active, so a session riding a small local model still gets a good
  one.
- `/think high` changes reasoning effort for the running model, this
  session, visible immediately in the status bar.
- `/context` draws the window filling before it is fatal, and `/export`
  writes the session record as markdown.

Custom commands are markdown files in `.switchboard/commands/` (project) or
`~/.switchboard/commands/` (global): `$ARGUMENTS` and `$1..$9` substitute,
`@file` attaches, and a backtick-quoted `` !`cmd` `` runs at expansion time.
Files written for other tools port by copying them. One trust rule: inline
shell runs only from your home directory's commands, never from a cloned
repository's.

Repository instructions in `AGENTS.md` or `CLAUDE.md` are read into the
system prompt on every session, and `/init` writes one for a repo that
lacks it.

## Credentials

Local targets need none. For everything else, the resolution order is: an
environment variable (`SB_<PROVIDER>_API_KEY`, or the vendor's own name
where the provider is that vendor), a configured credential helper, an OAuth
login, then the OS credential service (Keychain on macOS, Secret Service on
Linux). A credential is read from stdin or a masked prompt, never the
command line; it is never written to the config, the session log, or any
error, and its only rendering is a placeholder.

There is no encrypted-file fallback. A mode 0600 file is access control, not
encryption, and on a machine with no keyring the honest answer is the
environment or a helper:

```toml
[auth.anthropic]
helper = ["op", "read", "op://vault/anthropic/credential"]
```

A machine already signed in to Codex CLI can hand that token over with a
helper, and `/setup` offers to wire it in one pick. Two edges, stated
rather than discovered: the helper is provider-wide, and the token expires
on Codex's schedule; running `codex` once refreshes it.

There is also a direct login for the ChatGPT-plan surface, `sb auth oauth
login openai/subscription`, and it deserves a plain warning. The OAuth
client it presents is the one OpenAI's own Codex CLI registers. Switchboard
is not affiliated with or endorsed by OpenAI, this is not a flow OpenAI
publishes for third-party clients, and accounts have been actioned for it.
OpenAI's terms govern your account whatever a program claims to be; the
risk is yours, and a client you register yourself, configured under
`[auth.openai.oauth]`, always wins over the bundled one.

## The sandbox

Switchboard does not present a permission prompt as though it were
isolation. Automatic execution is granted only where confinement has been
demonstrated by a self-test on that machine: Seatbelt on macOS, bubblewrap
on Linux. A confined command writes only inside the workspace and build
caches, cannot read the credential stores or reach the daemons that serve
them, and cannot reach the network beyond loopback. Windows has no verified
containment, so every command there is approved individually, in every
mode. [docs/sandbox.md](docs/sandbox.md) documents the profile, including
what it deliberately does not protect against.

## Targets, not models

The same weights reached through two endpoints are two targets, with two
prices, two cache behaviors, and two capability records. `ollama/local/
qwen3.5:9b-mlx` and the same model through an OpenAI-compatible proxy are
not interchangeable to the router, and the catalog prices them separately.
A provider can be redirected at a gateway with `[providers.<name>]
base_url`, which changes the address and deliberately nothing else.

## Documentation

[docs/eval.md](docs/eval.md) records what the routing gate measured,
including the run that falsified a reputation-ordered ladder and the
derivation that replaced it. [docs/estimator.md](docs/estimator.md)
measures the token estimator's error instead of describing it, and
[docs/sandbox.md](docs/sandbox.md) documents the confinement profile. The
section references scattered through the code comments (§6, §8.3, and so
on) point into the maintainers' design document, which is not part of the
public tree; the constraints it imposes on the code are restated in
[AGENTS.md](AGENTS.md).

## Contributing

`go build ./... && go vet ./... && go test ./...` runs offline with no keys;
`SB_LIVE=1` gates the tests that talk to real servers. Constraints that hold
across the codebase are in [AGENTS.md](AGENTS.md), and
[CONTRIBUTING.md](CONTRIBUTING.md) has the rest. Security reports go
through [SECURITY.md](SECURITY.md).

## License

MIT. See [LICENSE](LICENSE).
