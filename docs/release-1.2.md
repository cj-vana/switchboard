# Switchboard 1.2

Version 1.2 turns the default TUI into a coding workbench while keeping the
model ladder and append-only session record at the center. The editor features
share one rule: stale or partial evidence must look stale or partial on screen.

## Workbench navigation

- Ctrl+P opens a searchable command palette built from the same registry as
  `/help` and slash-command completion.
- `/files [query]` quick-opens a bounded workspace index. `/search <literal>`
  finds text and opens an exact source location.
- Search labels partial coverage and counts skipped, oversized, or truncated
  files.
- Results carry a content revision. Editor handoff refuses a result when its
  file changed after the search instead of opening a stale location.
- Wide terminals show list and source preview side by side. Narrow terminals
  keep the same source lens in one pane. Both can copy a location or hand it to
  `$VISUAL` or `$EDITOR`.

The index is for file names and literal text. It does not call itself semantic.
That work belongs to an installed language server.

## Semantic views without fake certainty

Trusted Go, TypeScript, Python, and C or C++ projects can use `gopls`, the
TypeScript 7 native server, `pyright`, or `clangd` where the project mapping
applies.

| Command | Result |
| --- | --- |
| `/lsp` | Show configuration, process state, and advertised capabilities without starting the server |
| `/outline <path>` | Declarations in one source file |
| `/symbols <query>` | Workspace declarations by semantic name |
| `/definition ...` | The symbol's definition |
| `/references ...` | Semantic references to the symbol |
| `/problems [path]` | Published diagnostics with freshness and coverage |

Outline, symbol, definition, and reference queries start the server lazily;
`/lsp` and `/problems` do not. Results inside the workspace can open in an
editor and are marked stale after later Switchboard tool batches. External
results are copy-only.

Problems uses the server's push diagnostics. Rows are labeled fresh, stale,
unversioned, or pending, while the panel labels its push coverage partial. An
empty panel says that it does not prove the workspace clean. A repository
verifier remains the authority for a passing build.

## Two read-only views of change evidence

`/diff` is a read-only view of the workspace against `HEAD`. It combines staged
and unstaged tracked patches with explicit untracked-file patches, including an
unborn repository. It does not change the index or run external diff drivers.
Binary, non-regular, unmerged, or output-bounded changes stay named when no
text patch can be shown.

The diff is scoped to the workspace even when Switchboard starts inside a
larger Git worktree. Output is bounded. A truncated patch includes a sorted,
bounded inventory of changed paths not fully shown plus the remaining count.

`/review [turn]` answers a narrower question: what did the agent record through
`write` and `edit` in one turn? Bare `/review` selects the currently open turn
only and never falls back to older work. A positive number selects a retained
mutation turn, one-based and oldest first. Shell, hook, MCP, and manual changes
remain outside this checkpoint view.

Current bytes appear only after exact existence, mode, size, digest, target,
parent, and ancestor identity checks. A stale, unsafe, or redirected path is
refused without showing its current content. Created, deleted, truncated,
empty, mode-only, binary, oversized, and omitted states stay explicit.

The selected-turn load is capped at 256 paths and 256 KiB of aggregate content,
with an omitted-path count. Rendering is capped at 1,200 lines and 256 KiB and
keeps file sections whole. Async results remain bound to the launching session,
workspace generation, checkpoint revision, selected turn, and invocation. The
80x24-safe TUI panel is available only while the agent is idle. A late result is
discarded. It has no rollback, apply, editor, per-file, or per-hunk action and
does not mutate the checkpoint, Git index, or worktree. `/diff` remains the
repository view against `HEAD`.

## Atomic, revision-checked edits and undo

First-party write and edit calls now use a per-path transaction. They validate
the exact bytes the agent read immediately before publication, publish a
complete replacement atomically, and preserve the file mode. A mismatch already
present at that check makes the mutation refuse.

Undo applies the same check in reverse. Immediately before restore, it compares
current existence, mode, and exact bytes with the successful post-edit state and
rechecks the captured parent-directory identity. A mismatch leaves the current
file untouched and keeps the checkpoint available. The comparison and final
rename are not one portable atomic pathname CAS, so a simultaneous external
replacement at that seam is outside this guarantee. Files too large for an
exact pre-image are reported as skipped, not recoverable.

## Continuity across process and context boundaries

The session write-ahead log can store a bounded continuity capsule with a
successful todo state and its derived next action. The payload is canonicalized,
size-limited, and credential-scanned before it is written. A successful todo
result and its updated capsule are one atomic log record, so a crash cannot
replay one without the other.

After a restart or session swap, the newest undelivered valid capsule is stamped
into the next user opening before routing, estimation, and provider send, then
consumed. Pending and delivered state survives the swap, so an already-delivered
capsule is not injected again. It stays bound to the message boundary that
produced it through fork and retry. The UI and session labels still show only
the prompt the user wrote.

Compaction carries the live recorded todo state and derived next action when
present, plus immediate parent lineage. It stamps that capsule once into the
compact seed without duplicating it in the generated summary, so the next real
user opening is clean. Retry replays the exact recorded message, including image
and reference metadata, and refuses to continue after a partial or failed file
restore.

Continuity is context, not authority. A resumed loop forgets old file-read
tokens and restores todo display state, while permissions, routing, workspace
trust, and verification keep their normal gates.

## Bounded startup extension diagnostics

A full startup diagnostic stream can flood a small terminal or hide the failure
that matters. A risk-first summary fits at most three 79-column ASCII lines.
Routine problems are deduplicated into at most five noncritical highlights;
every retained `fatal`, `critical`, `high`, or `required` failure stays visible.
The layout is tested at 80x24 without hiding the banner, target, or prompt under
ordinary failure volume.

`/doctor extensions` in the TUI and REPL shows every retained diagnostic,
terminal-sanitized, in discovery order, with duplicates intact. The initial
buffer is capped at 200 entries. Overflow adds a mandatory high-severity notice
with the exact dropped count and says those texts are unavailable; it never
pretends the drill-down is complete. Later notices still reach the live TUI,
but the REPL report remains a startup snapshot rather than a health dashboard.

## Parallel delegate tasks with visible control

One provider response containing only independent `delegate` calls can run up
to four at once. Results rejoin the provider loop in original call order. Mixed
delegate/read/write batches and batches with applicable hooks remain serial.
First-party writes share the primary turn checkpoint; same-path contenders
serialize, and a stale contender fails rather than merging concurrent content.

The busy-safe TUI command `/tasks` shows current-session task ID, name, status,
serving tier, live call count and observed cost, and parent and delegate session
IDs.
`/tasks cancel <id>` stops one queued or running task without canceling its
siblings. Approval prompts serialize and name the task asking. A partial usable
answer can remain available while the task status records `failed`.

Task control is provider-driven; there is no direct `/task <prompt>` launcher.
The process-wide memory-only status history is capped at 100 and its IDs do not
survive a restart. Delegate session logs do survive for accounting and blame.

## Shell behavior you can script

`sb help` is a pure startup path. It runs before update checks, configuration,
sessions, extension inventory, or provider probes. Root help, subcommand help,
and nested plugin or MCP action help work even when that runtime state is
broken.

Generated bash, zsh, and fish completion now follows the same bounded grammar
as dispatch, including leading global flags, nested actions, attached values,
terminal flags, and `--`. Parse errors print usage and exit 2; ordinary command
failures exit 1; cancellation exits 130.

Shell cancellation terminates the active process group on macOS and Linux, and
the visible transcript keeps the exit code, signal, timeout, or cancellation.

The TUI is the complete interactive surface. `-repl` remains a deliberately
reduced line-oriented mode, and `-p` remains the headless one-turn path. REPL
`/help` lists only the commands it implements. Fullscreen file search, `/diff`,
`/review`, `/tasks`, and semantic views are TUI-only.

## What this fixes

These are product requirements, not claims about how often another tool fails.
The linked [comparison](comparison.md#what-this-fixes) separates issue reports,
community anecdotes, and unsourced engineering hazards.

| Failure mode | 1.2 response | Boundary |
| --- | --- | --- |
| A session resumes with the conversation but not the live task | Exactly-once continuity capsules carry recorded todo state and the next action | The capsule is advisory and cannot preserve unsupported native workflow controls |
| A diff says clean while an untracked file contains the work | `/diff` includes staged, unstaged, and untracked state | Ignored files stay ignored; bounded or binary changes may be named without a text patch |
| A turn review presents later or redirected bytes as the agent's work | `/review` revalidates exact checkpoint and path evidence before showing current bytes | It covers recorded `write` and `edit` mutations only; shell, hook, MCP, and manual changes stay outside it |
| An agent edit or undo starts from file state that has already changed | Atomic publication plus an immediate pre-publication comparison refuses an observed mismatch | The comparison and rename are not an atomic pathname CAS; a simultaneous external replacement at that seam is outside the guarantee |
| A diagnostics list looks complete when the server saw only part of the project | Problems labels freshness and partial push coverage | The repository verifier, not Problems, decides whether the build passes |
| Extension discovery either floods startup or hides a required failure | A bounded risk-first summary keeps retained mandatory severity visible; `/doctor extensions` preserves retained detail | The startup buffer is capped; overflow reports the exact dropped count but cannot recover the dropped text |
| Parallel delegate work hides status, cost, or approval ownership | `/tasks` shows current-session work and cancels one task; approval prompts serialize with task identity | Task IDs and status are process-local; the delegate session logs remain durable |
| Basic navigation keeps sending the user out of the terminal | Palette, file search, source preview, two change-review views, and semantic views live in the TUI | This is a focused terminal workbench, not a claim of full IDE parity |
| Broken setup makes help unavailable | Static help and completion do not open runtime state | Commands that do real work still load the state they need and fail honestly |

## Upgrade notes

Existing configuration and session logs remain readable. Older session schemas
upgrade before they append continuity records. No new provider credential is
required.

The sandbox still starts off unless configuration or a launch flag requests a
verified profile. Permission `auto` uses a low-cost reviewer only while that
verified confinement is active; host-direct execution asks the human. A
confined explicit full-network request can remain reviewable, while shared
loopback, opaque, sensitive, and external effects stay human-gated. `yolo`
behavior is unchanged. See [Confining commands](sandbox.md).

The static site now deploys through Netlify. The repository-root
`netlify.toml` publishes `docs/`; the GitHub Pages workflow and `docs/CNAME`
are gone. The intended custom domain is considered live only after the Netlify
project, deployment, DNS, and TLS have all been verified.
