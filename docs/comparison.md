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

Claude Code has checkpoints with rewind, a skills and plugin ecosystem,
agent teams, IDE integrations, and MCP OAuth flows; it is the deepest
single-vendor experience. Codex CLI has the most configurable
profile-per-workload setup, several sandbox postures to choose between, and
cloud execution. OpenCode has the broadest provider matrix, LSP
integration, a desktop app, and the largest open-source community. None of
that is dismissed by the axes above; a user whose work never leaves one
frontier model, or who needs an IDE surface today, is well served there.

## The claim, scoped

Switchboard's claim is not "better at everything"; it is that an agent that
reasons about which model to use, and what that choice costs given cache
state, produces better outcomes per dollar than one that always calls the
same model — and that this repository contains the instrument that will
prove that claim wrong if it is wrong. That is the difference in kind: the
neighbors ask you to pick a model; this tool treats the pick as the
product, and measures it.

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
