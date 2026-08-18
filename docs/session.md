# Sessions and command reference

Switchboard's default interactive surface is a terminal UI. The line-oriented
REPL remains available with `-repl`, and `-p` runs one headless turn.

## TUI state

The prompt frame shows the permission mode when it differs from `default`. The
cursor and tier labels use the active tier's color. The status area shows the
active ladder position, route moves, streaming token rate, cost in its native
metering, context occupancy, session age, and current verifier state. Less
important decoration disappears first when the terminal becomes narrow.

Tool rails and route records expand with Ctrl+O or a mouse click. Ctrl+F
searches the transcript from newest match to oldest and marks every match in
the margin.

## Input

| Input | Behavior |
| --- | --- |
| `@path` | Completes a workspace path and attaches its contents |
| Image path or screenshot mention | Attaches an image only when the selected target has live or catalog-verified vision support |
| `!cmd` | Runs a command immediately as the user and carries its output into the next turn |
| Trailing `\` | Continues the prompt on another line |
| Ctrl+G | Opens the current prompt in `$VISUAL`, falling back to `$EDITOR` |
| Ctrl+R | Searches prompt history for the workspace |
| Ctrl+F | Searches the transcript |

If the target cannot accept an image, Switchboard refuses the attachment and
states the missing capability. It does not send an image to a target that may
ignore it.

Prompts entered while a turn is running join a queue. `/queue` shows them and
`/queue clear` removes them. They run after the active turn completes.

## Questions and approvals

The `ask` tool can present up to several choices plus a free-text answer. Arrow
keys and Enter choose one item, digits select by number, and Space selects
multiple items when allowed. Escape declines the question; the model receives
that decline as an answer and continues.

An approval or question can ring the terminal bell. In a headless run,
delegated task, or race branch, no listener exists. `ask` then tells the model
to choose and state an assumption instead of waiting. Free-text answers are
scanned for credentials before they enter the log.

## Common commands

| Command | Purpose |
| --- | --- |
| `/models` | Browse available models and bind tiers |
| `/tiers` | Show the ladder and active profile |
| `/t3` | Pin the session to tier 3 |
| `/tier auto` | Resume automatic per-turn routing |
| `/why` | Explain routing decisions and reprice the session on other tiers |
| `/think high` | Change reasoning effort for the active target |
| `/context` | Show estimated system, tool, and conversation use separately from provider-reported usage |
| `/compact [preview]` | Preview or perform context compaction |
| `/budget 2.50` | Set the persistent dollar ceiling |
| `/estimate <prompt>` | Estimate the next assembled request on every tier |
| `/cache` | Show the cache belief used by routing |
| `/doctor` | Run startup, credential, sandbox, tool, and MCP checks |
| `/setup` | Reopen provider setup |
| `/mode <plan|default|acceptEdits|auto|yolo|bypass>` | Change the permission policy |
| `/sandbox on|off|auto|status` | Change or inspect command confinement for this process |
| `/theme <dark|light|auto>` | Set the persistent TUI theme |
| `/notify [on|off]` | Control completion and approval notifications |

Routing, budget, cache, and cost semantics are detailed in
[Routing and the model ladder](routing.md).

## Advisor and compaction

`/advisor` reports the current state; `/advisor on` assigns a second model to
observe loop events. It reacts to the same stuck-agent signals as the router
and injects advice at the next safe seam. It cannot edit files and has a
per-turn limit. Set `[slots] advisor = "t2"` to enable it by default.

`/compact` replaces older conversation with a summary in a fresh context. It
runs automatically after the last provider request crosses 85 percent of the
context window. `/compact preview` reports the messages and estimated tokens
that would be replaced, the content that remains fixed, and the model that
will summarize. `[slots] summarizer` assigns a dedicated tier to this work.

## Session history

| Command | Purpose |
| --- | --- |
| `/export` or `sb export [id]` | Write a Markdown timeline with route decisions, race verdicts, warnings, and messages |
| `/recap` or `sb recap [id]` | Summarize the opening prompt, turns, cost, route movement, touched files, race verdicts, and next resume/blame actions |
| `/find <text>` or `sb find <text>` | Search recorded prompts and responses case-insensitively |
| `/find all <text>` or `sb find all <text>` | Search every workspace and group matches by the workspace name stored in each log |
| `/fork [turns|pin]` | Continue from an earlier prefix in a new session log |
| `/pin [name]` | List pins or name the current point for a later fork |
| `/retry [tier]` | Revert the last captured turn and replay its recorded opening, optionally on another tier |
| `/resume` | Open a recorded session |

Inside a running session, bare `/recap` reads the previous log because the
current session is not where the user left off. `sb recap <id>` reads a
specific session.

Forking does not rewrite the original log. The copied message prefix remains
byte-identical, so a provider that still holds it may serve the branch from
cache. Files are not rewound by a fork.

`/retry` uses a fork at the last turn's opening and replays the already expanded
opening bytes. The discarded answer remains resumable with a `user_corrected`
label. File changes recorded by write and edit are reverted first. Shell side
effects remain. A tier argument runs the replay there and then returns.

## File history and verification

`/changes` lists files captured by write and edit, grouped by the turn that
changed them. It does not claim to see shell-command side effects.

`/undo <path>` restores one file to its state before the newest turn that
captured it. The checkpoint is consumed only after a successful restore.
`/undo` restores every captured file from the newest turn. Conversation
messages remain unchanged to preserve the sent prefix and its cache identity.
A restored file must be read again before a later edit.

`/blame <path>` and `sb blame <path>` replay recorded write and edit operations
against the current file. Each explained line includes the turn, tier, target,
and prompt. Lines created by a shell, typed by hand, or predating the logs are
reported as unknown rather than guessed. The replay spans recorded workspace
sessions, including delegate sessions.

Bare `/blame` summarizes surviving lines by target and metering. A location such
as `/blame cache.go:42` reports the line's writing turn, prompt, other files,
final response, and resume command.

`/mistakes` and `sb mistakes` group repeated test-shaped command failures by a
digit-normalized signature. Each result lists the sessions that encountered
it. A copied fork prefix counts as one observation. Failures printed outside
the exec tool are outside this record.

### Watch

`/watch <command>` arms a user-selected verifier. It runs after edit rounds and
again at turn end. Only a changed result is reported: a new failure or a
transition from red to green. The current status remains visible even when the
result is unchanged.

A new mid-turn failure can trigger escalation. A turn-end result informs the
user and the next prompt because the completed turn can no longer move.
`/watch off` disables the verifier. The setting survives `/clear`, `/fork`, and
`/resume` within the process.

The verifier runs unconfined as the user. Repositories cannot declare it. Bare
`/watch` may suggest a command from a real Makefile test target, Go module, or
npm test script, but the user must arm it.

### Bisect

`/bisect` finds the turn that changed an armed verifier from green to red. It
binary-searches per-turn checkpoints, reconstructs each candidate state, and
runs the verifier. `/bisect <command>` supplies a verifier for that run.

The original tree is restored on success, failure, or cancellation. The search
covers only changes captured by write and edit; current shell and manual
changes remain in every reconstructed state. Prompts queue while the bisect is
busy, and Escape cancels it. The result is attached to the next prompt so a
follow-up such as `fix it` carries the failing turn and first error.

## Accounting and routing records

| Command | Purpose |
| --- | --- |
| `/cost` | Current session cost |
| `/cost turns` | Cost by user turn, plus labeled compaction, learning, advisor, and command-approval work |
| `/cost rungs` | Cold counterfactual cost on every tier |
| `/stats` or `sb stats` | Workspace lifetime accounting |
| `sb stats all` | Accounting across all workspaces |
| `/ladder` or `sb ladder` | Opening and ending tier distribution |
| `/races` or `sb races` | Paired race verdicts |

The outputs preserve local, plan, and dollar units. See
[Routing and the model ladder](routing.md) for the accounting rules.

## Clipboard and notifications

`/copy` copies the last response. `/copy code` copies its newest fenced block,
and `/copy code 2` selects the preceding block, counting newest first across
session responses.

`/notify` controls the terminal bell for completed turns and waiting approvals.
The terminal title also marks active work. Notifications are enabled by
default, and `/notify off` persists the setting.

## Tool surface

The core registry includes read, write, edit, exec, glob, grep, todo, ask,
websearch, and webfetch. A second read of an unchanged file returns a short
marker because the bytes already exist in the model context.

Web tools ask before contacting a new host, reject cross-host redirects, and
scan outbound URLs and queries for known credential forms. See
[Security](security.md).

If `ast-grep` is installed, session assembly adds `astgrep` for syntax-tree
search. Install it on macOS with `brew install ast-grep`. Inside verified
confinement it runs as a read effect. Without confinement, it is classified as
execution and follows the active permission mode. Switchboard does not ship a
semantic code index; an external index is a service destination and belongs
behind MCP.

Language-server tools join when the project type, installed server, and
workspace trust agree. Supported mappings are `gopls` for Go, the TypeScript 7
compiler's native server for TypeScript, `pyright` for Python, and `clangd`
when `compile_commands.json` supplies flags. `definition` and `references`
accept a file, line, and symbol and return exact file-and-line results. The
server starts on first use.

On macOS, the optional `computer` tool controls applications through the
Accessibility API. Its permission model and tested limits are in
[Computer use](computer.md).

MCP, hooks, skills, plugins, custom commands, and delegation are documented in
[Native extension compatibility](extensions.md).

## Scripting

`sb -p "prompt"` runs one turn and exits. Piped stdin becomes an attachment:

```sh
git diff | sb -p "review this"
```

Because stdin supplied content, it cannot answer an approval prompt. A tool
that needs approval is refused and the reason is returned to the model. Bypass
is prompt-free only when verified confinement isolates both host network and
host IPC. The current macOS and Linux profiles retain host IPC, so command
approvals still fail closed in a headless bypass run.

`-output json` writes exactly one JSON object on stdout while the transcript
goes to stderr. The object contains the result, outcome, final tier and target,
tokens, and a cost object with separate local, plan, and dollar forms.

`sb -sessions` lists recorded sessions. `-resume` and `-continue` reopen one
after a script exits or a process crashes.

Repository instructions in `AGENTS.md` or `CLAUDE.md` enter the system prompt.
`/init` creates an instruction file when a repository has none. Custom command
files are covered in [Native extension compatibility](extensions.md).
