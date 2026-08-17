# Where Switchboard stands

A comparison is a dated document. This one describes August 2026, names its
sources, and separates three kinds of claim: what every terminal agent now
does, what only Switchboard does and where in this repository to verify it,
and what the neighbors do better. "Better than X" as a blanket sentence is
marketing; what follows is the axes this tool bets on and the evidence per
axis.

## The converged baseline

Claude Code, Codex CLI, OpenCode, and Switchboard have all converged on the
same skeleton: an agent loop over read/write/edit/exec plus file search, web
fetch (and in most, search), MCP for the long tail of tools, hooks at
tool-call boundaries, custom slash commands, repo instructions read from
`AGENTS.md` or equivalent, subagents, permission modes, model fallbacks for
availability, and session resume. On
this skeleton none of the four is interesting; the differences are in what
each tool believes about models, money, and safety.

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
bill", not "$0.00". On top of that sits `/budget`: a hard dollar ceiling
checked against a conservative preflight bound in three places — the
router refuses rungs whose upper bound could cross it, the escalation
policy cannot move onto one, and the loop stops before the call that
would (`cmd/sb/budget.go`, §15). The neighbors report spend after the
fact; none enforces a ceiling the model-selection machinery itself obeys.
And the receipt scales: `/cost rungs` reprices one session cold on every
rung, and `/stats` prices the workspace's whole recorded history the same
way — as routed, then pinned to each rung of today's ladder
(`cmd/sb/stats.go`) — an answer no neighbor can produce, because none has
a ladder to price a history against.

**Cache state as a belief with a surface.** Provider cache is tracked
from what providers report, never from what was sent, as a probability
that decays rather than an expiry the tracker cannot observe
(`internal/cachestate`); `/cache` is that belief's surface — the modeled
hit chance for the next send with its reason, the session's eligible-hit
count, the alarm when a written prefix keeps coming back cold — and
every number keeps the tracker's honesty: modeled says modeled, and a
surface that reports no cache accounting stays unknown, because silence
is not a miss (`cmd/sb/cache.go`). The neighbors' pricing pages mention
cache discounts; none models whether the cache is actually holding, and
none can be asked.

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
context; here the same want is answered by `/fork`
(`internal/session/fork.go`, §12): branch the session at a turn boundary
into a new log and continue there, the original untouched and the fork's
prefix byte-identical to what was already sent — so a provider still
holding it warm serves the fork warm, and going back costs nothing in
cache. Rewind mutates history to move; fork moves without mutating. And
`/pin` gives the cut a name: mark a point once, `/fork <name>` returns to
it whenever, the pin a plain record that survives resume and rides any
fork containing it.

**An outbound credential gate.** The credential posture points both ways:
the keys the tool holds are unprintable by type
(`internal/credential/credential.go`), and the keys the user is about to
leak — pasted into a prompt, riding in on an @mentioned `.env` or a
`!env` transcript — are caught before the send
(`internal/credential/scan.go`, `cmd/sb/tui_secretgate.go`). Known issuer
prefixes only, no entropy guessing, so a warning always means something;
the send holds behind redact, send-as-typed, or drop; a `-p` run with no
one to ask is refused, `-allow-secrets` being the stated widening. The
neighbors' guidance for this is hooks the user writes themselves; none
ships the gate, and none promises that the warning itself cannot quote
the key. The same gate faces the web tools' egress: `websearch` and
`webfetch` carry a permission effect no mode auto-allows, an approval
covers a host for the session, a redirect off that host is refused
before anything is dialed, and a key-shaped string in an outbound URL or
query is refused with the refusal itself masked
(`internal/tools/web.go`) — a fetch is the classic exfiltration channel,
and here the gate that faces outward for prompts faces outward for URLs.

**A declared verifier wired into routing.** `/watch go test ./...` makes
the user's own check ambient: it runs after the model's edits — the
checkpoint recorder's capture count is the trigger, so a delegate's edits
count too — and only the delta travels, a failure no earlier run produced
or the run going green, because a verifier that repeats itself teaches
its reader to stop reading it (`internal/watch`, `cmd/sb/tui_watch.go`).
A new failure feeds the same escalation evidence as a test run the model
made itself, which is §8.4's claim — a task-specific verifier outranks
the agent's own sense that things went well — given a place to be
declared. The neighbors' answer to "run my tests after edits" is a hook
the user writes, whose output replays in full every time and informs no
routing, because there is no routing to inform. None ships a verifier
that speaks in deltas, colors the status bar, and moves the ladder.

**A rerun that is a controlled experiment.** `/retry` takes the last turn
back — files revert through the undo checkpoints, the conversation forks
at the turn's opening — and replays the recorded opening byte-for-byte,
optionally one rung up (`cmd/sb/tui_retry.go`). Because nothing is
re-expanded, the second rung reads exactly what the first one read: same
input, different model, the user's judgment as the verdict, and the
set-aside answer's log labelled `user_corrected` for the same corpus the
race verdicts feed. The neighbors can edit a message and regenerate, at
the cost of rewriting history; none can hand the identical turn to a
different model and keep both outcomes as routing evidence.

**A paired trial the user judges.** `/race` runs one prompt on two rungs
at once, each arm a fork of the session riding the same prefix, both
read-only until the user picks which branch continues (`cmd/sb/race.go`,
`internal/tools/branch.go`). The pick is recorded on the session as a
paired, human-judged comparison — same task, same context, two targets —
which is a stronger fact about model choice than any single turn's
outcome, and a tie is recorded as the cheaper rung sufficing: direct
evidence of necessity, the thing watching one model succeed can never
establish. The record is collected and deliberately not consumed by
routing, because acting on it is gated behind the eval that has not run.
The neighbors can switch models between turns; none can run the same turn
on two models, show both answers, and keep the verdict as evidence — and
none needs it less, because none has a routing thesis to falsify.

**Delegation priced on the same ladder.** Subagents exist everywhere now;
Switchboard's `delegate` takes a rung, defaults to the cheapest, and its
trailer names what the errand cost (`internal/delegate`). Named
definitions exist here too — a markdown file in `.switchboard/agents/`
with a charter, a default rung, and a tool grant that can only narrow —
but the structural difference stands: Claude Code pins a model per agent
definition and Codex fans out to a mini model, both static configuration,
while here the per-task choice stays the model's, an explicit rung
outranks the charter's default, and either is priced in the same catalog
the router uses — the routing bet applied to orchestration. The design
plan gates any claim that this *wins* on an eval against single-primary
baselines (§19.2 phase 6), and that eval has not run, so the honest
statement is: the mechanism ships, the verdict is pending.

## What the neighbors do better

Claude Code has the deepest skills and plugin ecosystem — the skills
mechanism now exists here and its packs load by copying the folder, but
the library and the community writing it are theirs — plus agent teams,
IDE integrations, and MCP OAuth flows; it is the deepest single-vendor
experience. Codex CLI has
the most configurable profile-per-workload setup, several sandbox postures
to choose between, and cloud execution. OpenCode has the broadest provider
matrix, LSP coverage across more languages than Switchboard's verified
three (Go, TypeScript, Python), a desktop app, and the largest
open-source community. A user
whose work never leaves one frontier model, or who needs an IDE surface
today, is well served there.

## In practical use

Capability axes are not the whole of a tool, so the practical claims get
their evidence too. Setup is one checksum-verified install command and one
checklist: first run detects every reachable provider, takes keys into the
OS keychain, wires an existing Codex login in one pick, and binds t1 — no
file is ever edited by hand (`cmd/sb/tui_onboard.go`, tested in
`tui_onboard_test.go`). The §14 performance discipline is tested rather
than asserted: completed entries render once per width and streaming text
never touches the renderer cache (`tui_test.go`), and the transcript
renders the viewport rather than the session —
`BenchmarkTranscriptView50Turns` and `BenchmarkTranscriptView500Turns`
exist to be compared, and a 500-turn session views no slower than a
50-turn one, microseconds against the 16ms input-latency target. The affordances around a
session hold the same standard: `/find` greps what past sessions
actually said and hands back the ids `/resume` takes; ctrl+f searches
the transcript itself, because the alternate screen hides the
conversation from the terminal's own search — a gap every terminal
agent shares and none of the others closes (`cmd/sb/tui_search.go`);
`/changes` maps files to the turns that touched them from the same
captures `/undo` restores (`cmd/sb/tui_commands.go`); `/queue` names
what waits behind a running turn; and a bell with a marked terminal
title says when a parked session needs its person. What this
document deliberately does not claim is user-adoption evidence: the
neighbors have communities and this tool is new, and no benchmark
substitutes for that.

## The verdict this document will stand behind

Separate capability from breadth and the picture is clean. On breadth —
provider count, IDE surfaces, plugin ecosystems, community — the neighbors
lead, and those leads are functions of scale and time, not of design. On
the capability of the core, the thing a terminal coding agent is for,
Switchboard now concedes nothing: the converged skeleton is fully present
(tools, MCP on both transports, hooks, subagents with named definitions,
custom commands, skills that load the neighbors' own packs, availability
fallbacks, per-turn undo, session fork with named pins, structural search,
web search and fetch, and language-server symbol lookup), and
on top of it sit twelve axes — evidence-based routing with `/why`,
three-way cost honesty, a hard budget the machinery itself obeys, the
measured estimator, the falsification harness with its runs in the tree,
verified-or-absent sandboxing, delegation priced on the ladder, the
`/race` paired trial whose verdicts feed that harness, the `/watch`
verifier that speaks in deltas and moves the ladder, the byte-identical
`/retry` whose verdicts join the same corpus, the outbound credential
gate that covers the web tools' egress, and the tracked cache belief
with its `/cache` surface — where the neighbors have no counterpart at
all.

By the measure this product defines — capability per dollar, safely, with
every model decision visible and explainable — Switchboard is the
strongest tool in its class, and it is the only one that ships the
instrument that could prove that sentence wrong. The instrument has now
been pointed at the sentence once: on the eleven-task head-to-head of
2026-08-16, completion tied at 11/11 across all three tools while only
this one noticed that four of the tasks were reachable from rungs that
bill nothing — the measured shape of the claim, at the small scale one
machine and one day allow. The neighbors ask you to pick a model; this
tool treats the pick as the product, and measures it.

## Reproduce it

The claims above are argued from the code; the head-to-head that would
test them is runnable, not hypothetical. The eval corpus doubles as a
baseline instrument (§8.6): `internal/eval/bench_test.go` materialises a
deterministic one-task-per-package cut of the corpus — chosen in corpus
order, fixed before any tool runs, so results cannot pick their tasks —
and judges any tool's attempts with the same verifier the exit gate uses,
the package tests plus the checks that keep "delete the failing test"
from counting as a solve. `SB_BENCH_CUT=all` widens the cut to every
spec, for a run that wants the corpus's full breadth rather than its
spread; both the materialise and the verify read it, so the two cannot
disagree about which tasks a lane holds.

One lane, any agent CLI:

    SB_BENCH_MATERIALIZE=/tmp/bench/sb go test ./internal/eval/ -run TestMaterializeBench

Then, per task directory and its prompt from `manifest.jsonl`, run the
tool under trial headless — the postures that give each tool
edit-and-run-tests capability non-interactively are
`sb -p "<prompt>" -mode bypass -output json`,
`claude -p "<prompt>" --permission-mode acceptEdits --allowedTools
"Bash(go test:*)" "Bash(go build:*)" --output-format json`, and
`codex exec --sandbox workspace-write --skip-git-repo-check "<prompt>"` —
and judge the lane:

    SB_BENCH_VERIFY=/tmp/bench/sb go test ./internal/eval/ -run TestVerifyBench -v

Verdicts land in `verdicts.jsonl`, one line per task, from the same code
for every lane. This procedure has been run:
[head-to-head-2026-08-16.md](head-to-head-2026-08-16.md) records all
three lanes on one machine — completion tied at 11/11 for every tool,
switchboard alone put four of the eleven tasks entirely on local rungs
and spent zero API dollars against claude code's $22.35 — with the raw
journal beside it and the caveats stated in full. What this document
refuses to ship is a comparison that could not be rerun; this one can be,
with three commands.

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
