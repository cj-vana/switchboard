# Product comparison

This comparison is dated 2026-08-17. It separates repository-backed
Switchboard behavior, measured results, and external product reports.
Competitor behavior changes quickly, so external claims should be rechecked
before use in a release announcement.

## Scope

Claude Code, Codex CLI, OpenCode, and Switchboard all provide the basic coding
agent loop: file operations, shell commands, repository instructions, session
resume, and extensibility through MCP or an equivalent mechanism. The useful
differences are in routing, cost controls, state recovery, verification, and
the authority granted to extensions.

The table summarizes Switchboard's current position. “No comparable surface”
means the public material reviewed for this dated comparison did not describe
one. It does not prove that another product lacks an internal mechanism.

| Area | Switchboard | External baseline in this review |
| --- | --- | --- |
| Model selection | Ordered multi-provider target ladder with deterministic per-turn routing and evidence-based moves between completed rounds | User or configuration selects the model before the work |
| Route explanation | `/why` records feasible and rejected targets, moves, and counterfactual cost | No comparable ladder explanation surface found |
| Metering | Local execution, plan quota, and dollar billing remain separate | Usage is usually reported after calls |
| Hard budget | Retry-inclusive dollar ceiling checked before routes, moves, and provider calls | No comparable model-selection budget gate found |
| Cache state | Per-target modeled warmth with observed provider accounting | Cache discounts may be documented without a live routing belief |
| Session branching | Append-only logs with fork, named pins, retry, recap, and line provenance | Resume and checkpoint features vary by product |
| Verification | User-armed watch, turn bisect, paired races, and a router evaluation gate | Hooks and test commands are common; no equivalent combined surface found |
| Command safety | Sandbox off by default; opt-in verified confinement; explicit yolo mode for unconfined host access | Products expose sandbox or approval modes with different guarantees |
| Extensions | Compatible native skills, local plugins, direct and trusted plugin MCP, hooks, and one subagent level | Claude Code leads in plugin and skill breadth; OpenCode leads in provider and LSP breadth |
| Computer control | macOS Accessibility tool under the normal permission engine | Hosted or API computer-use surfaces exist; terminal integration varies |

## Routing, cost, and cache

Switchboard routes the assembled request immediately before each user turn.
Capability, context, availability, and hard-budget checks exclude infeasible
targets. A user pin must pass the same checks. During a turn, repeated tool
calls, error spikes, new failure signatures, and hedging can propose a move.
The provider binding changes only after the current model round and its tool
work finish. See [Routing and the model ladder](routing.md).

Cost keeps three units: local execution, plan quota, and dollars. `/estimate`
prices a prospective request, `/cost` reports the recorded session, `/stats`
aggregates workspace history, and `/cost rungs` reprices the session cold on
each tier. `/budget` sets a persistent dollar ceiling. The conservative gate
is implemented in `cmd/sb/budget.go`; accounting and counterfactual commands
read recorded calls rather than reconstructing them.

Cache state belongs to a target, not a model name. `/cache` reports the
eligible prefix, modeled hit probability, reason, observed hits, and repeated
misses. A target that does not report cache accounting remains unknown. The
token estimator's measured error and the bound used by the cost model are in
[Token estimator error](estimator.md).

The learned router is absent. A model can ship only after a clean evaluation
produces at least two useful tiers and the candidate beats the deterministic
policy after runtime and distribution costs. The current evidence and the
failed historical matrix-integrity check are in
[Routing evaluation](eval.md).

## Session evidence and verification

The session record is append-only. `/fork` branches from an earlier message
prefix without rewriting the source log. `/pin` names a point, and `/retry`
replays the recorded opening bytes on the same or another feasible tier.
`/undo` restores captured write and edit changes without changing the sent
conversation. Shell and manual side effects remain outside that checkpoint.

`/blame <path>` replays recorded write and edit operations against the current
file. A surviving line can therefore be attributed to a session, turn, tier,
target, and prompt. Lines created by a shell, formatter, hand edit, or work
that predates the logs remain unknown. `/recap`, `/find`, `/changes`, and
`/mistakes` expose other parts of the same record.

`/watch <command>` runs a user-selected verifier after edit rounds and reports
only changed results. A new mid-turn failure can contribute routing evidence.
`/bisect` searches captured per-turn checkpoints for the green-to-red
transition and restores the original tree on every exit path. `/race` runs a
read-only prompt on two tiers and records the user's verdict without training
the production router. See [Sessions and command reference](session.md) for
the operational limits of each command.

## Safety and extension authority

Switchboard treats permission, containment, workspace trust, and extension
activation as separate decisions.

- The sandbox starts off. `on` requires Seatbelt on macOS or a provenance-checked
  system bubblewrap on Linux to pass a live self-test. `auto` uses a verified
  profile when present.
- `bypass` suppresses prompts only when verified confinement isolates host
  network and IPC. Both current production profiles retain host IPC, so bypass
  asks today. `yolo` is a separate explicit grant of full host reach.
- MCP and computer-use tools act outside the command sandbox. Neither `bypass`
  nor `yolo` auto-approves them.
- Repository hooks, project MCP, and language servers require Switchboard
  workspace trust. Another client's remembered trust does not transfer.
- Native MCP definitions need explicit Switchboard activation, applicable
  policy, trust where required, and a supported runtime feature set.
- Plugin executable components need independent enablement and trust bound to
  the current plugin-tree digest.
- Prompts, attachments, command output, web requests, and computer-use text
  are checked for known credential forms before egress.

See [Security](security.md), [Confining commands](sandbox.md), and
[Native extension compatibility](extensions.md) for the exact boundaries.

Native compatibility is deliberately narrower than format discovery. Safe
Codex and Claude skill subsets assemble. Claude legacy command files are
manual-only. Enabled plugin skills and compatible trusted plugin MCP assemble.
Plugin hooks, agents, commands, apps, workflows, and LSP declarations remain
inventory-only or unsupported. Native OAuth, SSE, WebSocket, helper,
remote-execution, and approval semantics that the runtime cannot preserve fail
closed.

The optional macOS `computer` tool drives application controls through the
Accessibility API. It requires per-application approval, scans text before
typing, redacts text read back, and does not claim screenshot support. See
[Computer use](computer.md).

## Measured results

The historical routing evaluation cannot support a release verdict. Its
journal contains duplicate cells and predates the identity fields needed to
bind rows to an exact commit, catalog, prompt, ladder, and model snapshot. The
current evaluator refuses such a journal. Diagnostic projections also show
only one useful tier, so there is no learned-routing decision to fit.

The dated head-to-head run used eleven seeded Go defects and the same verifier
for Switchboard, Claude Code, and Codex CLI. All three solved 11 of 11 tasks.
Switchboard completed four entirely on local tiers, used plan quota for the
rest, and billed zero API dollars. Claude Code reported $22.35 over ten
reporting runs; one solved task reached the watchdog limit. Codex CLI reported
641,576 plan-metered tokens. These numbers establish behavior on that corpus,
machine, and day. They do not rank general coding quality. See
[Head-to-head results](head-to-head-2026-08-16.md).

The token estimator measurement covered eighteen calls on one model through
two adapters. It undercounted by as much as 24 percent. The cost model widens
its upper bound from that measurement; it does not treat the result as an exact
provider invoice.

## Current reliability reports

Public issue trackers contain useful failure reports, but an issue is not a
prevalence estimate and may be fixed after this date. Reddit links below are
individual community anecdotes or discussions, not verified incident rates.
The items explain which failure modes Switchboard treats as product
requirements.

| Reported failure mode | External examples | Switchboard response |
| --- | --- | --- |
| Compaction breaks continuity or loses an explicitly selected workflow | [Codex #27555](https://github.com/openai/codex/issues/27555), [Codex #32169](https://github.com/openai/codex/issues/32169), [Claude Code #32407](https://github.com/anthropics/claude-code/issues/32407), [Claude Code #34872](https://github.com/anthropics/claude-code/issues/34872) | The source session stays append-only; compaction has a preview; fork and recap preserve a recoverable path. Switchboard does not claim that summaries can preserve unsupported native workflow controls. |
| Usage is hard to explain or grows unexpectedly | [“300M tokens for a day?”](https://www.reddit.com/r/ClaudeCode/comments/1sgh2dc/300m_tokens_for_a_day/), [“Saying 'hey' cost me 22%”](https://www.reddit.com/r/ClaudeAI/comments/1s3hh29/saying_hey_cost_me_22_of_my_usage_limits/) | `/estimate` gives a pre-send range, `/budget` enforces a hard dollar ceiling, and the durable accounting ledger tags model work by purpose so turns, compaction, learning, advising, and command approval remain distinguishable. |
| Terminal work hangs or cancellation does not settle cleanly | [Cursor terminal-action thread](https://www.reddit.com/r/cursor/comments/1msdwto/i_really_wish_cursor_would_fix_the_agent_choking/) | Cancellation is bounded and reaches active transports. macOS and Linux terminate the process group or tree; Windows terminates the direct child and warns that descendants may survive. Prompts entered during work stay visible in `/queue`; recovery paths drop them when the workspace cannot be restored safely. |
| MCP approval has no usable unattended or parent-visible path | [Codex #18268](https://github.com/openai/codex/issues/18268), [Codex #24135](https://github.com/openai/codex/issues/24135), [Claude Code #61315](https://github.com/anthropics/claude-code/issues/61315) | External calls remain explicit permission effects. Headless and race contexts fail closed instead of waiting. Delegated agents do not inherit bridged MCP tools. |
| Tool and plugin schemas consume context, collide, or load twice | [Claude plugin duplication thread](https://www.reddit.com/r/ClaudeAI/comments/1rij9tr/psa_your_claude_code_plugins_are_probably_loading/), [Cline tool-injection discussion](https://github.com/cline/cline/discussions/8578) | Plugins need explicit Switchboard enablement; executable components also need digest trust. Exact plugin identities and bridged tool-name collisions are resolved deterministically, MCP filters are enforced, and list changes apply on the next run. |
| Repository automation or attached output exposes secrets | General risk, not a prevalence claim | Repository hooks require trust, MCP child environments are scrubbed or restricted, and outbound text passes the credential gate. |
| Agent confidence or instruction following outruns verification | [LocalLLaMA software-engineering thread](https://www.reddit.com/r/LocalLLaMA/comments/1vavh2h/software_engineers_do_you_honestly_get_anything/) | Watch, bisect, race, and the evaluation gate keep test evidence separate from model claims. |

Switchboard still has open limits here. Compaction quality depends on the
configured summarizer. Shell side effects are not captured by undo or bisect.
Unconfined user hooks and armed watch commands run with the user's authority.
Fail-closed MCP behavior can make a native definition unavailable until its
semantics are implemented.

## Where other tools lead

Claude Code has a larger skill and plugin ecosystem, agent-team workflows,
IDE integrations, and MCP OAuth support. Codex CLI has broader configuration
profiles, multiple sandbox postures, IDE and cloud surfaces, and a large
installed extension base. OpenCode supports more providers and language
servers and has a broader open-source community. Those advantages matter when
the work depends on ecosystem breadth or a single-vendor surface.

Switchboard currently supports four language-server families, one subagent
level, and computer control only on macOS. Its plugin installer copies exact
local sources and does not fetch from a marketplace. Advanced native MCP
authentication, transport, and approval features remain disabled unless the
runtime can enforce them. The router remains deterministic until the
evaluation gate passes.

## Reproduce it

The repository provides a deterministic one-task-per-package cut of the eval
corpus. Materialize one lane:

```sh
SB_BENCH_MATERIALIZE=/tmp/bench/sb \
  go test ./internal/eval/ -run TestMaterializeBench
```

For each task directory and prompt in `manifest.jsonl`, run the agent under
test. The 2026-08-16 run used these non-interactive forms:

```sh
sb -p "<prompt>" -mode bypass -output json
claude -p "<prompt>" --permission-mode acceptEdits \
  --allowedTools "Bash(go test:*)" "Bash(go build:*)" --output-format json
codex exec --sandbox workspace-write --skip-git-repo-check "<prompt>"
```

That Switchboard build enabled verified Seatbelt confinement automatically.
Current builds start with confinement off, and both production profiles retain
host IPC authority that keeps bypass approvals with the human. The historical
bypass run therefore has no equivalent prompt-free headless posture under the
current boundary.

Judge the materialized lane with the same verifier:

```sh
SB_BENCH_VERIFY=/tmp/bench/sb \
  go test ./internal/eval/ -run TestVerifyBench -v
```

`SB_BENCH_CUT=all` widens both materialization and verification to every task.
Verdicts go to `verdicts.jsonl`. The completed run and its caveats are in
[Head-to-head results](head-to-head-2026-08-16.md), with the raw JSONL beside
it.

## External references

Competitor breadth claims were checked against public material available in
August 2026. The issue links in [Current reliability reports](#current-reliability-reports)
are primary user reports. They document individual failures or requests, not
product-wide rates.

- [Claude Code feature reference](https://toolsbase.dev/en/reference/claude-code-features)
- [Claude Code settings reference](https://hidekazu-konishi.com/entry/claude_code_features_settings_reference_2026.html)
- [Codex CLI profiles and sandbox guide](https://www.digitalapplied.com/blog/codex-cli-deep-dive-config-profiles-sandbox-2026)
- [Codex CLI guide](https://blakecrosley.com/guides/codex)
- [OpenCode overview](https://www.explainx.ai/blog/opencode-open-source-ai-coding-agent-guide-2026)
- [OpenCode feature summary](https://vibecodinghub.org/tools/opencode)
