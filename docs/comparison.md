# Where Switchboard stands

A comparison is a dated document. This one describes August 2026, names its
sources, and separates three kinds of claim: what every terminal agent now
does, what only Switchboard does and where in this repository to verify it,
and what the neighbors do better. "Better than X" as a blanket sentence is
marketing; what follows is the axes this tool bets on and the evidence per
axis.

## The converged baseline

Claude Code, Codex CLI, OpenCode, and Switchboard have all converged on the
same skeleton: an agent loop over read/write/edit/exec plus file search, MCP
for the long tail of tools, hooks at tool-call boundaries, custom slash
commands, repo instructions read from `AGENTS.md` or equivalent, subagents,
permission modes, and session resume. On this skeleton none of the four is
interesting; the differences are in what each tool believes about models,
money, and safety.

## What only Switchboard does, and where to check it

**Routing on evidence, explained after the fact.** The model is a slot, and
the session moves up a ladder of slots when the work shows it is stuck:
repeated identical tool calls, error spikes, new failure signatures, hedging
(`internal/router`). Every move renders inline with its reason, and `/why`
reconstructs the decision — what was ruled out, what this session's tokens
would have cost on every other rung. Claude Code selects among Anthropic
models, Codex among OpenAI's, OpenCode among many providers, but in all
three the choice is the user's, made ahead of the work; none moves on its
own evidence mid-task, and none can be asked afterwards why this model.

**Money that stays three different things.** A local model consumes nothing
scarce, a plan-metered model consumes quota, a per-token model consumes
dollars, and the catalog never collapses them into "free"
(`internal/catalog`, §4). The estimator's error is measured and written
down rather than guessed (`docs/estimator.md`), and the cost model widens
its bound by that measurement. The demonstration is one line in every
session footer: a local run says "runs locally, so there is nothing to
bill", not "$0.00".

**A falsification instrument, with its runs in the tree.** `internal/eval`
is a harness the routing thesis has to survive, not a benchmark to pass;
`docs/eval.md` records the run that falsified a reputation-ordered ladder,
and the raw journals sit beside it (`docs/*.jsonl`). No neighboring tool
ships the instrument that could prove its own model-selection story wrong.

**A sandbox that is verified or absent.** Automatic execution is granted
only where a self-test demonstrated containment on this machine, and the
value that proves the test passed is the same value that wraps the command
(`internal/execution`, `docs/sandbox.md`). The neighbors configure sandbox
modes and profiles; none gates automatic execution on a live self-test, and
Switchboard refuses the substitution the others accept quietly: a permission
prompt presented as if it were isolation.

**External tools never mistaken for contained ones.** An MCP server is a
process running outside every boundary, so its calls carry a permission
effect no mode auto-allows — bypass included — and a spawned server
inherits the environment minus the model credentials, with a test that
fails if one leaks (`internal/permission`, `internal/mcp/stdio_test.go`).
Repository-declared servers and hooks stay off until an explicit `/trust
grant` to that checkout (`internal/trust`).

**Undo that leaves the cache warm.** `/undo` takes back a turn's file
changes, turn by turn, from per-turn snapshots the write and edit tools
capture before mutating (`internal/checkpoint`). What it refuses to do is
the differentiator: it never rewrites sent messages, because the
append-only prefix is what keeps the provider cache warm, and a restored
file already forces the model to re-read through the stale check. Claude
Code's checkpoints rewind the conversation too, at the price of the
context; here that trade is declined on stated grounds rather than left
unconsidered.

**Delegation priced on the same ladder.** Subagents exist everywhere now;
Switchboard's `delegate` takes a rung, defaults to the cheapest, and its
trailer names what the errand cost (`internal/delegate`). Claude Code pins
a model per agent definition; Codex fans out to a mini model; both are
static configuration. Here the choice is the model's, per task, priced in
the same catalog the router uses — the routing bet applied to
orchestration. The design plan gates any claim that this *wins* on an eval
against single-primary baselines (§19.2 phase 6), and that eval has not
run, so the honest statement is: the mechanism ships, the verdict is
pending.

## What the neighbors do better

Claude Code has a skills and plugin ecosystem, agent teams, IDE
integrations, and MCP OAuth flows, and its checkpoints rewind conversation
as well as files; it is the deepest single-vendor experience. Codex CLI has
the most configurable profile-per-workload setup, several sandbox postures
to choose between, and cloud execution. OpenCode has the broadest provider
matrix, LSP integration, a desktop app, and the largest open-source
community. A user whose work never leaves one frontier model, or who needs
an IDE surface today, is well served there.

## The verdict this document will stand behind

Separate capability from breadth and the picture is clean. On breadth —
provider count, IDE surfaces, plugin ecosystems, community — the neighbors
lead, and those leads are functions of scale and time, not of design. On
the capability of the core, the thing a terminal coding agent is for,
Switchboard now concedes nothing: the converged skeleton is fully present
(tools, MCP on both transports, hooks, subagents, custom commands, per-turn
undo), and on top of it sit six axes — evidence-based routing with `/why`,
three-way cost honesty, the measured estimator, the falsification harness
with its runs in the tree, verified-or-absent sandboxing, and delegation
priced on the ladder — where the neighbors have no counterpart at all.

By the measure this product defines — capability per dollar, safely, with
every model decision visible and explainable — Switchboard is the
strongest tool in its class, and it is the only one that ships the
instrument that could prove that sentence wrong. The neighbors ask you to
pick a model; this tool treats the pick as the product, and measures it.

## Sources

Competitor capabilities above were checked against current public
references in August 2026, including Anthropic's Claude Code feature and
settings references, Codex CLI configuration and sandbox guides, and
OpenCode's documentation and reviews. Representative links:

- https://toolsbase.dev/en/reference/claude-code-features
- https://hidekazu-konishi.com/entry/claude_code_features_settings_reference_2026.html
- https://www.digitalapplied.com/blog/codex-cli-deep-dive-config-profiles-sandbox-2026
- https://blakecrosley.com/guides/codex
- https://www.explainx.ai/blog/opencode-open-source-ai-coding-agent-guide-2026
- https://vibecodinghub.org/tools/opencode
