# Switchboard: Design Document

**Status:** Reviewed draft; ready for validation spikes, not implementation commitment
**Date:** 2026-08-12
**Last technical verification:** 2026-08-12
**Working name:** Switchboard (binary: `sb`)
**License intent:** Open source
**Implementation:** Go; static default binary, with optional classifier acceleration only

---

## 0. Note for reviewers

This document describes a system that does not exist yet. Nothing in it has been
built or validated. It is a design of record for an initial implementation, written
to be attacked.

Specific things worth attacking:

- Whether the cache-aware routing thesis passes the predeclared gate in §7, rather
  than merely producing compelling anecdotes.
- The context zone model in §6 and whether it survives contact with real agentic
  workloads.
- Whether the task-profile mapping in §8 is learnable with enough calibration to
  handle arbitrary user targets.
- Whether the execution and extension trust model in §11 and §13 is shippable on
  each claimed platform.
- Whether even the reduced v0.1 boundary in §19 is realistic for one developer.

Provider API details (cache pricing, token minimums, breakpoint semantics) were
checked against the primary sources in Appendix A on 2026-08-12 and will drift.
Everything that depends on them is isolated behind the versioned target catalog in
§4 for exactly this reason. Prices quoted in worked examples are illustrative.

### Review disposition

The product thesis is coherent, but the original draft committed too early to a
scalar notion of model capability, a specific classifier architecture, and a broad
platform scope. This revision makes five decisions before implementation starts:

1. A **route target** is a provider, serving surface, model snapshot, and inference
   configuration. Cache state, price, capability, and eval results attach to that
   target, not just to a tier or model name.
2. Tiers remain an understandable user-facing ladder, but hard capabilities are
   constraints and measured task-specific quality is a vector. There is no claim
   that all models admit one universal total order.
3. The cache-aware routing thesis is tested in phase 2, before the full TUI, broad
   extensibility, classifier, orchestration, or hosted platform is built.
4. Permission prompts are not treated as a substitute for isolation. Shadow runs,
   hooks, MCP servers, and shell execution share one explicit trust model.
5. The shipped router starts with heuristics and measured target calibration. A
   learned classifier is selected only after smaller approaches fail an eval gate.

---

## 1. What this is

A terminal coding agent in the same category as Claude Code, Codex CLI, and
opencode, with one structural difference: the model is not a fixed property of
the tool. It is a configurable slot.

Switchboard exposes N ordered **routing tiers**. The user binds each tier to any
model from any provider: a cloud API, a local runtime, or a provider-authorized
account endpoint. A **router** then selects which tier handles each turn, and the
harness escalates or de-escalates during a task based on observed signals.

The claim this design rests on: **a coding agent that reasons about which model to
use, and about what that choice costs given cache state, produces better outcomes
per dollar than one that always uses the same fixed target.** Everything else in the
document is infrastructure for testing that claim.

### Non-goals

- A hosted IDE, a web app, or a GUI. The TUI is the product surface.
- Beating any single frontier model on any single benchmark. The bet is on
  allocation, not on the models themselves.
- Supporting every model on day one. The provider layer is designed for breadth;
  the initial catalog is deliberately narrow.

---

## 2. Design principles

1. **The core knows nothing about terminals.** The agent loop, tool suite, session
   store, and router are a library. The TUI is its first consumer, the headless CLI
   its second, an SDK its third. This constraint is enforced from the first commit,
   because retrofitting it is expensive.

2. **The reusable prefix is append-only between explicit rebuilds.** Every design
   decision about context layout serves cache stability. See §6. If a prefix must
   change, the change is represented as a cache-invalidating event and the cost
   model is updated before the next route decision.

3. **Routing decisions are visible and overridable.** A user who cannot see which
   tier is running, and cannot force a different one, will not trust the router.
   Silent downgrades destroy the product.

4. **Degrade only when the fallback preserves the boundary.** No classifier runtime
   means heuristic routing, and no telemetry consent means no telemetry. A missing
   security control is different: if isolation is unavailable, automatic execution
   is disabled or reduced to per-action approval. The UI must never present a
   permission prompt as equivalent to a sandbox.

5. **Small core, large periphery.** The built-in tool suite stays under ten tools.
   Everything else arrives through MCP. A one-person project cannot build the long
   tail; it can build the socket the long tail plugs into.

6. **Every destination is explicit.** Model requests send task content only to a
   provider or local endpoint the user has configured for that workspace. Fallbacks
   may not cross that approved destination set. Telemetry never carries prompts,
   code, diffs, paths, command strings, or repository identifiers. See §16.

---

## 3. Tiers and slots

Two distinct abstractions, frequently conflated.

### 3.1 Tiers are an ordered routing-policy ladder

Tiers are ordered by the user's intended **quality and compute policy**, ascending.
They are identified as `t1` through `tN` with user-assignable display labels. `N`
defaults to 4 and is configurable.

The ordering is not a claim that model capability is globally one-dimensional.
Vision, context fit, tool reliability, coding quality, latency, and data-residency
constraints are independent. Hard requirements first eliminate infeasible targets;
the router then compares expected quality, cost, and latency among the remaining
targets. This matters because a cloud-hosted small model may outrun a local large
one, and a model that wins on one task class may lose on another.

Tier IDs are numeric because numeric IDs are the only scheme that generalizes over
configurable `N` and does not encode a claim the system cannot guarantee.

A **route target** is the actual unit of execution:

```
target = provider + serving surface + model snapshot + inference parameters
```

Changing effort, reasoning mode, endpoint, or provider creates a different target
when that change affects capability, price, context rendering, or cache identity.

```toml
[tiers]
count = 4

[tiers.t1]
label = "light"
model = "anthropic/claude-haiku-4-5"

[tiers.t2]
label = "standard"
model = "openai/gpt-5.6-terra"

[tiers.t3]
label = "deep"
model = "anthropic/claude-opus-5"
effort = "high"

[tiers.t4]
label = "max"
model = "anthropic/claude-opus-5"
effort = "xhigh"
```

Two tiers may bind the same model with different parameters. Bundled benchmark
priors and local eval results may warn (not error) when the requested ladder appears
inverted for the user's selected task mix. A model-level rank alone is not enough,
because the two targets may differ only by effort or serving surface.

### 3.2 Slots are named roles

Slots are unordered. Each binds one model to a named function. The router is
itself a slot, which falls out of the model cleanly.

| Slot | Purpose |
|---|---|
| `router` | Tier selection. May be a local classifier, a model, or absent (heuristic only). |
| `vision` | Fallback when the active tier cannot accept images. |
| `embed` | Retrieval and semantic tool-result matching. |
| `rerank` | Optional, for retrieval quality. |
| `title` | Cheap session naming and summarization. |
| `image`, `video`, `audio` | Asset generation. See §3.3. |

Slots may alias a tier (`title = "t1"`) or name a model directly.

Vision needs both treatments. It is a capability flag on main-loop models, and a
fallback slot for when the bound model lacks it. When a user pastes an image and
the active tier is blind, the harness routes the image to the `vision` slot and
injects the returned description as a text block. The UI names that provider before
the image is sent, and refuses the fallback if it is outside the workspace's
approved destination set. A generated description is marked as lossy derived
context rather than presented as equivalent to native vision.

### 3.3 Media generation is a tool, not core

Image and video generation are bindable slots in the target catalog, but the
implementation lives outside the harness as MCP tools.

The rationale is scope. ComfyUI in particular is a workflow-graph API with a
websocket queue and a node schema; integrating it natively would consume a
meaningful fraction of the total engineering budget for a feature most users will
not touch. Existing MCP servers cover it.

A note on terminal rendering, since it constrains the feature: inline images work
in kitty, iTerm2, WezTerm, and Ghostty via their graphics protocols, and degrade
to a file path elsewhere. There is no portable native terminal-video path; the
default is a file path or explicit external preview. The realistic use case is
therefore asset generation as part of a coding task
(placeholder images, icons, og:image, test fixtures), not viewing. That is a
legitimate capability and it does not require native integration.

---

## 4. Target catalog

The catalog is the source of truth for what each **route target** can do, how its
requests are rendered, and what it costs. It is versioned data, updatable without a
binary release. Entries are keyed by provider and serving surface as well as model:
the same nominal model can have different IDs, cache behavior, features, prices,
and retention rules on its first-party API, Bedrock, Vertex, or a proxy.

```go
type ModelInfo struct {
    ID             string // provider-qualified, e.g. "anthropic/claude-opus-5"
    Provider       string
    Surface        string // first-party, bedrock, vertex, openrouter, local, ...
    ProviderModelID string
    DisplayName    string
    Snapshot       string // pinned version where the provider exposes one

    ContextWindow int
    MaxOutput     int

    // Pricing may change at context thresholds or by processing mode, region,
    // endpoint, and effective date. Never flatten it into one universal rate.
    Pricing []PriceBand

    // Cache mechanics are a policy object because read/write accounting,
    // defaults, TTL guarantees, and breakpoint semantics differ by surface.
    Cache CachePolicy

    // Capabilities
    Tools          ToolSupport // None | Serial | Parallel
    StrictSchema   bool
    Vision         bool
    VisionMaxPixels int
    Reasoning      ReasoningSupport // None | Budget | Adaptive | Always
    EffortLevels   []string
    StructuredOut  bool
    MidTurnToolChange bool
    Continuation   ContinuationSupport

    // Provenance makes drift auditable.
    VerifiedAt     time.Time
    CatalogRevision string
    SourceURLs     []string
}

type CachePolicy struct {
    Modes             []CacheMode // Automatic | Implicit | Explicit | None
    DefaultMode       CacheMode
    MinTokens         int
    TTLs              []TTL
    MaxBreakpoints    int
    LookbackBlocks    int
    Scope             CacheScope
    RoutingKeySupport bool
    Availability      CacheAvailability
    UsageAccounting   UsageAccounting
}

type PriceBand struct {
    EffectiveAt       time.Time
    MaxInputTokens    int // zero means no upper bound
    ProcessingMode    string
    Region            string
    InputPerMTok      Money
    OutputPerMTok     Money
    CacheReadPerMTok  Money
    CacheWritePerMTok map[TTL]Money
    HostedToolPrices  map[string]Money
}
```

**Why the cache policy is load-bearing.** As of August 2026, the minimum cacheable
prefix on Claude models is 512, 1024, 2048, or 4096 tokens depending on the model
and surface, and the ordering is not monotonic across generations: Claude Opus 5 is
512 on the first-party API while Claude Opus 4.6 is 4096. A 3,000-token prefix
caches on one and silently does not on the other, with no error in either case. Any
router that reasons about cache economics without these numbers is guessing.

Similarly, GPT-5.6 and later OpenAI models bill cache writes and support implicit
and explicit prompt caching. Implicit mode may write a growing prefix on each turn;
explicit breakpoints can avoid writes for suffixes that are unlikely to be reused.
That behavior must be selected by the target's cache policy and confirmed from
usage fields, not generalized to older OpenAI models or other serving surfaces.

Pricing bands are equally important. Current OpenAI model prices change above a
long-context threshold, and providers apply modifiers for processing mode, region,
batching, or hosted tools. The catalog's estimator may be approximate, but it must
say which band and revision produced the estimate.

### Sources, in precedence order

1. User overrides (`~/.switchboard/models.toml`) for local endpoints and
   corrections. The UI shows when an override changes price or privacy behavior.
2. Explicit runtime probe results, cached by target and endpoint revision. Probes
   use synthetic fixtures, disclose that they make billable requests, and never
   upload project content. One successful probe establishes API compatibility, not
   reliable tool-calling quality.
3. A signed, revisioned remote catalog refresh. Community data is advisory until
   corroborated by a provider source or a local probe.
4. A bundled, revisioned catalog vendored at build time. This guarantees the tool
   works offline and on first run.

Catalog updates are verified before activation, retained for rollback, and recorded
in each session so a historical cost or route decision remains reproducible.

---

## 5. Provider layer

### 5.1 Canonical types with per-provider adapters

The harness operates on its own message and content-block types. Adapters translate
to and from each provider's wire format.

The rejected alternative is using an OpenAI-compatible schema as the internal
lingua franca. It is faster to start and it caps the product, because that format
is a lowest common denominator that discards precisely what this design depends on:
cache breakpoints and their per-provider semantics, reasoning blocks and their
replay rules, parallel tool-call fidelity, and per-model capability signals.

```go
type Message struct {
    Role    Role      // User | Assistant | System | Tool
    Content []Block
}

type Block interface {
    Kind() BlockKind
}

// Concrete blocks: Text, Thinking, ToolUse, ToolResult, Image, Document.

type Request struct {
    System    []Block
    Tools     []ToolDefinition
    Messages  []Message
    CachePlan CachePlan // anchors canonical positions; set only by cache manager
}
```

Cache placement is a request-level plan over canonical positions rather than mutable
metadata owned by callers. This covers providers that put markers on content blocks,
tool definitions, or the request itself, while keeping canonical message content
stable for hashing. Only the breakpoint manager creates a `CachePlan`; the adapter
either renders it exactly or returns a typed unsupported-policy error.

### 5.2 Adapter interface

```go
type Provider interface {
    Name() string
    Stream(ctx context.Context, target RouteTarget, req Request) (EventStream, error)
    CountTokens(ctx context.Context, target RouteTarget, req Request) (TokenEstimate, error)
    Probe(ctx context.Context, target RouteTarget) (ProbeResult, error)
}
```

Each adapter owns its provider's quirks:

- Cache breakpoint syntax and mode selection.
- Tool schema translation, including degrading strict schemas where unsupported.
- Streaming event normalization into a common event type.
- Reasoning block handling and replay rules.
- Stateful continuation handles and any encrypted or opaque replay items.
- Detection of provider behavior when a requested capability is absent. The adapter
  never silently drops requested semantics; it returns a typed error. Any emulation
  belongs to the visible policy layer and is rechecked for destination and quality.

Eventual adapters: `anthropic`, `openai`, `google`, and `openaicompat`. Version 0.1
implements only the two route targets chosen for the phase-1 experiment. The
`openaicompat` adapter uses named, tested profiles for Ollama, LM Studio, vLLM,
llama.cpp server, OpenRouter, Groq, Together, and similar surfaces because
"OpenAI-compatible" is a spectrum rather than a specification; an unknown endpoint
starts with the lowest common feature set.

The canonical session log remains the source of truth. An adapter may use a
provider continuation handle as an optimization, but it records the opaque items
needed for supported replay and never treats a server-side ID as portable context.
Routing feasibility is calculated against the full logical conversation. If a
provider cannot export state needed for a faithful switch, the target change is
blocked or uses an explicitly lossy handoff policy; it is not silently attempted.

### 5.3 Auth

Credentials never live in the ordinary config file or session log. Preferred
storage is the OS credential service (macOS Keychain, Secret Service on Linux, or
Windows Credential Manager). Headless environments may use an environment variable
or an explicit credential-helper command whose output is never logged. A file
fallback is offered only when it is encrypted with a user-supplied passphrase or an
OS-protected key; mode 0600 alone is access control, not encryption.

Auth methods are pluggable: API key, cloud-provider credential chain, and an OAuth
or device flow **only where the provider publishes and authorizes that flow for
third-party clients**. Switchboard will not copy tokens from another application,
reverse-engineer a first-party login, or imply that a consumer subscription is a
general-purpose API entitlement.

The plugin boundary is an interface, not Go's in-process plugin mechanism, which is
not portable across all target platforms. Replaceable auth integrations run as a
small, versioned helper process with a narrow protocol and no access to prompts or
workspace files unless their documented flow requires it.

### 5.4 Fallback chains

Each tier may declare an ordered fallback list of route targets. Before advancing,
the harness rechecks context fit, required capabilities, data-residency policy,
approved destinations, remaining budget, and whether the target can faithfully
continue provider-specific reasoning or tool state. Cross-provider fallbacks are
disabled unless the workspace policy explicitly allows them.

The substitution is shown before content is sent when it changes destination, and
is recorded in the turn log and TUI. Fallback is an availability event rather than
a router quality label, although its actual cost and outcome remain part of the
session record.

---

## 6. Context and caching

This is the subsystem the rest of the design depends on, and the one most likely to
be built wrong by default.

### 6.1 Zone model

Context is laid out in four zones, ordered, and append-only above the tail.

| Zone | Contents | Mutation policy |
|---|---|---|
| **Frozen** | System prompt, tool definitions (deterministically sorted), project instructions from AGENTS.md | Never mutated within a session |
| **Stable** | File contents and retrieved documents confirmed unchanged by content hash | Append only |
| **History** | Conversation turns, tool calls, tool results | Append only |
| **Volatile tail** | Current user message, injected dynamic context, per-request state | Rewritten freely |

Rules the layout enforces:

- Nothing above the volatile tail is rewritten mid-session. Dynamic operator context
  (mode changes, time, remaining budget) goes into the tail, or as a mid-conversation
  system message where the provider supports one, never by editing the frozen zone.
- The stable zone is populated at session start, or at an already-scheduled prefix
  rebuild such as compaction. Inserting into it mid-session shifts every history
  block that follows and invalidates the cached prefix from the insertion point,
  which is the exact failure mode this layout exists to prevent. Files read
  mid-session live in the history zone; their content hash serves to avoid
  re-reading them, not to relocate them. If a previously-read file changes, a new
  history block explicitly supersedes the old hash; old content is not mistaken for
  current state.
- The stable zone is append-only **between** rebuilds, not permanently. It carries a
  token budget expressed as a fraction of the target's context window, and it is
  subject to eviction at every scheduled rebuild alongside history compaction.
  Without this it grows monotonically with each session-start file read and becomes
  a context-window problem on a large repository, which is the same defect as an
  unbounded history with a more respectable name. Eviction is by least-recently-
  referenced, with files named in AGENTS.md or pinned by the user exempt. Crossing
  the budget schedules a rebuild rather than silently truncating.
- Tool set changes use a target's **verified** mid-conversation tool-change feature
  where available. Otherwise the change invalidates the tool prefix and is delayed
  until an explicit rebuild when practical. A static tool-discovery broker can keep
  the top-level schema stable for large MCP catalogs, but its quality and schema
  trade-offs must be evaluated rather than assumed.
- Compaction rewrites history, which is a cache-invalidating event by definition.
  It is scheduled deliberately rather than triggered incidentally.

Most harnesses break their own cache by appending into the middle of the prefix.
This layout is not clever; it is discipline, and it is worth more than any of the
optimizations below.

### 6.2 Breakpoint manager

A single component owns all cache-marker placement. It reads the full `CachePolicy`
for the bound route target and:

- Generates breakpoint candidates at the last block of reusable prefixes, including
  zone boundaries and selected history positions. A breakpoint never intentionally
  includes a timestamp or other suffix known to vary on the next request.
- Scores candidates by expected reuse, tokens covered, write cost, TTL, and the
  target's breakpoint and lookback limits. All candidates draw from the same limit.
- Adds history candidates before a growing turn crosses the target's lookback
  window. For example, Claude currently looks back at most 20 block positions; a
  long tool-use turn can cross that window. The safety margin is a policy parameter
  backed by tests, not a universal `LookbackBlocks - 5` constant.
- Declines to place a breakpoint when the prefix is below the target's minimum, and
  logs it, rather than emitting a marker that silently does nothing.
- Selects automatic, implicit, or explicit mode only when supported by that target.
  For write-billed caches, explicit mode excludes low-reuse suffixes when the
  expected saving exceeds the lost hit opportunity.
- Sets a privacy-preserving affinity key derived from the cache lineage and route
  target where supported. It is not derived from the tier alone: fallbacks and two
  tiers bound to the same model would otherwise fragment or misattribute state.

### 6.3 Cache state tracker

Per session, per route target and cache lineage:

```go
type CacheEntry struct {
    TargetID       RouteTargetID
    PrefixHash     string
    Tokens         int
    LastWriteSeen  time.Time
    LastReadSeen   time.Time
    MinimumTTL     time.Duration
    State          CacheState // Unknown | WriteObserved | ReadObserved | Expired
    CatalogRevision string
}
```

Entries are updated from response usage fields rather than assumed from request
construction. A write observation and a read observation are distinct. Provider
TTL language is often a minimum or best-effort retention guarantee, so the tracker
models hit probability and expiry rather than pretending it knows exact server
state. Repeated eligible misses surface a warning instead of letting the estimator
believe in a cache that has not been observed.

### 6.4 Cost model

Actual cost is computed after a response from the provider's usage fields and the
catalog revision active for that request. Pre-request routing uses an estimate:

```
E[turn_cost(target, context)] =
    Σ_outcome P(outcome) * provider_cost(
        cache_read, cache_write, uncached_input,
        output_and_reasoning, hosted_tools, price_bands)

incremental_switch_cost(A, B) =
    E[turn_cost(B)] - E[turn_cost(A)]
  + P(A expires before return) * E[lost_future_warm_value(A)]
```

The provider adapter owns `provider_cost`: usage fields do not have identical
subset or overlap semantics across APIs, and long-context and processing-mode bands
make one universal arithmetic formula misleading.

A latency model of the same shape uses per-target time-to-first-token, output-rate,
queue, and tool-round-trip estimates, maintained as rolling observations rather
than a static table.

**Worked example of the inversion this prevents.** At August 2026 first-party list
prices, reading an 80,000-token warm Claude Opus 5 prefix costs about $0.04
(80k × $0.50/MTok). Cold-writing the same prefix on Claude Haiku 4.5 at
$1/MTok with its 1.25x five-minute write multiplier costs about $0.10 before output:
two and a half times as much for that turn despite the destination's lower base
input price.

Both the multiplier and the read discount are per-provider and in several cases
per-model; 1.25x is one provider's short-TTL write rate, not a universal constant.
The arithmetic above is an illustration of the shape of the inversion. The actual
numbers come from the target catalog (§4) at decision time, which is the entire reason
those fields exist.

A switch does **not** immediately evict the source cache. It can remain usable until
its provider-managed expiry, so adding the entire prior write as a "residual value"
would double-count a sunk cost. The real opportunity cost is probabilistic: whether
the source entry expires before the session returns and what future warm reads would
then have been worth.

A router that ignores cache state can burn money while claiming a saving. A router
that treats server cache state as certain can make the same mistake in the other
direction. Estimates therefore carry a confidence interval and are reconciled with
observed usage.

### 6.5 Fan-out rule

Some providers make a cache entry readable only once the first response begins
streaming. For targets with that policy (including the current Claude API), issuing
N concurrent requests against a cold identical prefix can make all N pay a write.
The orchestrator may issue one request, await the provider's documented availability
event, then issue the remainder. Targets without that guarantee use their own
policy; this is not a universal fan-out rule.

### 6.6 Instrumentation

Per turn, per route target: cache-read tokens, cache-write tokens, uncached tokens,
eligibility, selected mode and breakpoint, estimate versus actual, and a derived
hit rate. A repeated near-zero hit rate is an alarm only when the requests were
eligible and reuse was expected; a new prefix, an expired TTL, or a sub-minimum
prefix makes zero the correct result.

### 6.7 Deferred optimizations

Three ideas evaluated and deliberately scheduled after the above. They are recorded
here with their reasoning so the sequencing is auditable.

**Content-addressed tool-result cache.** Cache tool outputs keyed by content hash.
This is token reduction rather than prompt caching, and the naive implementation
(deduplicating by rewriting history) actively breaks the cache. The correct framing
is that content hashing detects an unchanged file, which permits skipping the
re-read entirely: the file's blocks stay where they already sit in the prefix.
Files known at session start can be placed directly in the stable zone (§6.1);
mid-session, hashing prevents re-injection rather than relocating blocks, which
would itself break the cache. Cheap, useful, unglamorous. Scheduled after the core
caching work.

**Handoff by summary on escalation.** Send a structured handoff (plan, diff, failure
log, relevant file slices) instead of replaying full context to the higher tier.
It can remove most transferred context on a large session, but it is also the option
most likely to make output worse, and it does so at
the moment the task is hardest, which is why it escalated in the first place. Ships
behind a flag with full replay as the default, and only once the eval harness can
demonstrate quality holds. Where a provider ships server-side compaction, use that
rather than building a competing summarizer.

**Speculative cache warming.** Pre-warm a higher tier's prefix when escalation looks
likely, using a zero-output-token prefill request where the provider supports one.
The mechanism is real and cheaper than a full inference, but it still costs a
1.25x-multiplier cache write on the most expensive tier, for a cache that may go
unread. It buys latency and costs money. It also depends on calibrated escalation
confidence, which does not exist until the trained router does. Ships last, as an
opt-in latency mode.

---

## 7. Why cache-aware routing is the thesis

Stated plainly because it drives the build order.

Cache-aware routing is not unique to a multi-vendor harness: a single provider can
offer several models, and a router can switch among them. The relevant contrast is
with a **fixed-target** harness. Once a product can change model, endpoint, or effort,
it must account for target-scoped cache state or its cost comparison is incomplete.

This makes cache awareness more than a generic prompt optimization, but it does not
make it risk-free or provider-identical. The estimator can be wrong, explicit
breakpoints can reduce useful automatic reuse, and provider semantics differ. The
defensible differentiator is the combination of cross-provider target choice,
observed cache state, visible decision economics, and outcome-based evaluation.

There is a supporting architectural point. Cache state is target-scoped, so keeping
a primary loop sticky while delegating scoped work to another target often preserves
more value than repeatedly switching the primary. That is independently the
orchestration shape chosen in §9. It is a strong default, not a universal rule.

### 7.1 Falsification gate

Before phases 3 and later receive substantial investment, a representative eval
must compare cache-aware routing with always-lowest, always-highest, and
cache-unaware routing. The initial product gate is:

- At least a 20% reduction in median cost per verified solved task against the best
  fixed-target baseline, including router calls and cache writes.
- On cache-eligible sessions, the uncertainty interval for cost versus the otherwise
  identical cache-unaware router excludes zero in the beneficial direction.
- No more than a two-percentage-point absolute decrease in verified solve rate, and
  no safety-policy regression.
- Estimate-versus-actual cost error reported by target, with no systematic
  underestimation that would violate a user budget.
- Results reported over multiple runs with uncertainty intervals and pinned model,
  catalog, prompt, and harness versions.

These are provisional product thresholds, but they must be changed before viewing
the decisive results, not afterward. Failing the gate narrows or falsifies the
product thesis; it is not a signal to build the hosted platform first.

---

## 8. The router

### 8.1 Interface

```go
type Router interface {
    Route(ctx context.Context, in RouteInput) (RouteDecision, error)
}

type RouteInput struct {
    Prompt        string
    Session       SessionFeatures
    Candidates    []RouteTarget
    CacheState    map[RouteTargetID]CacheStatus
    Requirements  Requirements // capabilities, destination policy, residency
    Budgets       Budgets      // cost ceiling, latency ceiling
}

type SessionFeatures struct {
    TurnDepth       int
    PriorFailures   int
    FilesInContext  int
    DiffSizeSoFar   int
    RepoLanguages   []string
    TestsInvolved   bool
    LastTurnEscalated bool
}

type RouteDecision struct {
    Tier            TierID
    Target          RouteTargetID
    Confidence      float64
    EstimatedCost   MoneyRange
    EstimatedLatency DurationRange
    Rationale       string
    Source          RouteSource // Heuristic | Classifier | LLM | UserPin
    PolicyRevision  string
}
```

`Rationale` and `Source` are not diagnostics. They are rendered in the TUI, because
principle 3 requires it.

### 8.2 Three implementations, chained

**`HeuristicRouter`** (pure Go, ships first). Rules over the prompt plus structured
features. Handles the clear cases: a short factual question goes low, a multi-file
refactor goes high, a turn immediately following a test failure escalates. Always
available, zero dependencies, no network call, and it is the fallback whenever
anything below is unavailable or uncertain.

**`ClassifierRouter`** (ships only when training data and eval evidence justify it).
Start with lexical features, a linear or tree model, and compact local embeddings.
Benchmark accuracy, calibration, cold-start time, peak memory, artifact size, and
CPU latency on supported machines. A roughly 150M-parameter ONNX encoder is a
candidate, not a design commitment, and no single-digit latency claim is made before
measurement.

**The model emits a fixed-width task profile, not a tier index.** A single scalar
difficulty score assumes the total capability order that §3.1 rejects.

This is the highest-risk component in the document and the one most likely to be
wrong, so the dimensions are committed provisionally here rather than left as prose.
Each is defined with a unit and a measurement procedure against the §8.6 corpus,
because a dimension nobody can measure is not a dimension.

| Dimension | Kind | Unit | Measured by |
|---|---|---|---|
| `context_span` | Hard constraint | tokens | Smallest context window at which solve rate plateaus across pinned targets |
| `visual_input` | Hard constraint | boolean | Task requires image understanding to reach a verified solution |
| `plan_depth` | Scalar need | dependent steps | Median tool-call depth before the first edit that survives to the verified solution |
| `edit_breadth` | Scalar need | files, diff lines | Taken from the verified solution's diff |
| `tool_reliability` | Scalar need | 0-1 | Solve-rate delta on the same task between targets with parallel, serial, and unreliable tool calling |

Constraints run first and eliminate infeasible targets. Scalar needs are then matched
against per-target calibration measured on the same corpus, producing a competence
gap per dimension.

The **mapping layer** scores each feasible target as a weighted combination of those
gaps, penalized by estimated cost and latency and adjusted for cache state from §6.3.
The weights are fit on the eval corpus rather than hand-tuned, which is what makes
the layer thin: it is a small fitted scoring function over measured quantities, not a
second inference stage. Per-user calibration and local-only personalization (§8.5)
adjust the target calibration table, not the profile extractor, which is why they are
cheap.

**The null hypothesis is that this loses to a scalar.** A single difficulty score
plus the hard constraints above may score as well as the full profile at a fraction
of the complexity, in which case the profile is unjustified and the scalar ships. The
profile is prototyped immediately after the heuristic router, evaluated against that
null, and abandoned if it does not clear it. Dimension stability across model
generations remains open question 2.

Unknown targets begin from catalog priors and remain visibly low-confidence until
local or published eval evidence exists.

The architecture choice matters: the common path is a small prediction and policy
problem, not a generation problem. A generative router adds network latency and cost
on every uncertain turn, so its value must include that overhead.

**`LLMRouter`** (cheapest tier or a local model). Used only when the chain above
returns low confidence, and as the bootstrap data generator before a classifier
exists.

Composition is a chain with confidence thresholds. A user pin short-circuits model
selection only after capability, approved-destination, context-fit, and hard-budget
checks; an infeasible pin returns an actionable error rather than bypassing policy.

### 8.3 Escalation and de-escalation

The initial routing decision is worth less than the mid-task adjustments, because
one user message can produce dozens of model calls.

Escalation triggers:

- K consecutive **new** test failures with the same failure signature.
- The same edit reverted or rejected twice.
- Model uncertainty language, as a weak signal only and never by itself.
- Diff crossing a file-count or line-count threshold.
- Loop detection: repeated identical tool calls with identical arguments.
- Tool error rate spike within a turn.

De-escalation triggers:

- A planning or analysis phase completes and remaining work is mechanical
  (bulk edits, formatting, applying an already-decided change).
- Task complexity estimate revised downward after context gathering.

Switches are filtered for capability, context fit, destination policy, and budget
before economics apply. A target that cannot hold the context or is not approved is
not an expensive option; it is infeasible. Remaining switches use hysteresis and a
minimum dwell to prevent oscillation. Changing effort on the same model is still a
switch when it changes rendered context or cache identity.

Expected quality benefit is an estimate with confidence, not a known scalar. A
quality trigger may override the normal cost preference but never a hard user
ceiling or destination policy. When mechanical work can be scoped, delegation is
preferred over de-escalating the sticky primary.

### 8.4 Training signal

Every turn logs: features, chosen tier, decision source, outcome, token spend, and
wall time. Outcomes are `completed`, `escalated`, `user_corrected`,
`reviewer_rejected`, or `abandoned`.

- An escalation is evidence that the initial choice may have been insufficient, not
  automatically a negative label. Provider failure, a planned phase change, or a
  bad escalation rule can produce the same event.
- A clean completion is a **weak positive** for sufficiency. It does not establish
  necessity, and treating it as such is the main way a naive router learns to
  over-provision.
- `abandoned` is censored unless the user supplies a reason; it is not a failure
  label by default.
- A task-specific verifier (tests, static checks, an exact expected artifact, or a
  blinded review rubric) is stronger evidence than the harness's own completion
  signal.
- **Shadow routing** supplies a counterfactual only when both outcomes can be
  independently verified. On a sampled fraction of eligible turns, run an alternate
  target and compare verified outcomes. A worktree alone is insufficient isolation:
  shadow runs receive no production credentials, have network disabled by default,
  cannot call external side-effecting tools, use resource and time limits, and write
  only to a disposable snapshot. Shadowing remains explicit opt-in and off by
  default because it consumes money and compute.

### 8.5 Privacy posture for training

Training a shipped model on user prompts requires explicit opt-in and is a trust
question, not merely a compliance one. Two paths avoid it:

1. The shipped classifier trains on synthetic tasks plus license-compatible public
   benchmark task sets, with contamination and benchmark-version notes.
2. Users may opt into **local-only personalization**: a small adapter trains on
   their own history and never leaves the machine.

Uploading real prompts for centralized training is a separate, explicit, off-by-
default consent, if it is ever offered at all.

### 8.6 Eval harness

Built early, not late. It is the difference between the router being a product and
being a demo.

Contents: tasks spanning trivial edits through multi-file refactors, each with an
independent verifier. "Appropriate tier" labels are derived empirically by running
pinned route targets across multiple seeds and identifying the quality/cost Pareto
front; they are not assigned from model reputation.

**Task generation.** Manual authoring does not scale and synthetic generation risks
contamination, so the corpus is built in three tiers with different trust levels:

1. **Hand-written, from the author's own repositories.** Twenty to thirty tasks, each
   with an executable verifier. Small, uncontaminated, and the author can establish
   ground truth directly. This tier ships with phase 2b and is the only one the gate
   measurement depends on.
2. **Extracted from merged public pull requests** with existing test coverage, where
   the suite fails at the parent commit and passes at the merge commit. This gives
   credible ground truth at volume, at the cost of a contamination problem: any PR
   predating a model's training cutoff may be memorized. Tier 2 results are reported
   separately from tier 1 and annotated with the cutoff of each evaluated target.
3. **Synthetic generation**, only after the heuristic router exists, and only as
   volume on top of the first two. Synthetic tasks validate the harness rather than
   the models, because the generator's notion of difficulty is the thing under test.

**The harness does not ship before tier 1 is populated.** An eval harness with an
empty corpus produces confident numbers about nothing, which is worse than having no
harness, because the numbers get quoted.

Metrics:

- Calibration of predicted success by route target and task class.
- Escalation precision: what fraction of escalations were necessary, measured by
  whether the lower tier's shadow run also succeeded.
- De-escalation safety: did quality hold when the router stepped down.
- Cost per solved task, against always-lowest and always-highest baselines.
- Latency per solved task, same baselines.

Deterministic router math, policy constraints, recorded provider fixtures, and
catalog parsing run on every change. Live model evals are scheduled and run before
a router release; nondeterministic, billable API calls do not block every commit.
Reports pin model snapshots, provider surface, catalog revision, prompt version,
seed where supported, and harness commit, and include uncertainty intervals. A
router release that regresses the predeclared cost-per-verified-task or safety gate
does not ship.

---

## 9. Orchestration

### 9.1 Shape

The router selects **one primary tier per user turn**. The primary model then makes
its own delegation decisions, because it holds the task context and the router does
not.

The primary is given a `delegate(tier, task)` tool. It can farm mechanical work down
to a cheaper tier while keeping reasoning at its own level. Subagents receive a
scoped context, destination policy, budget, and tool grant rather than the full
history and return a structured report. Concurrent writable delegates must receive
disjoint paths or isolated snapshots; overlapping edits are serialized and merged
by the primary.

This is a hierarchy, not a mesh. There is no cross-agent negotiation to design and
no distributed coordination to debug. It is also cache-correct: a subagent on a
different model is cache-isolated by construction, which makes delegation the right
way to use a cheaper model mid-task, as opposed to switching the primary and
risking source-cache expiry while paying to warm the destination.

Fan-out consults the target-specific availability policy in §6.5; it uses the
one-then-many pattern only where the provider documents that behavior.

### 9.2 Reviewer pass

The reviewer is risk-based and user-configurable, not an unconditional top-tier tax.
It runs automatically for high-risk or large changes when the budget permits, and
may be requested explicitly for any task. The default target is one calibrated step
above the primary; the top target is reserved for cases where evals show value.

- **Input:** the diff, the failure log, the original task statement, applicable
  project instructions, and narrowly selected source context. Not the full
  transcript, which is expensive and mostly noise.
- **Output:** advice, not edits. The reviewer does not have write tools.
- **Bounds:** hard cap of two fix rounds. A per-task cost ceiling. On exceeding
  either, the reviewer emits its remaining concerns as text and stops.

The failure mode the bound exists to prevent is specific, and it is not the reviewer
malfunctioning. On a large diff the reviewer finds a genuine issue, the primary fixes
it, and the changed code presents a fresh surface on which the reviewer finds the next
genuine issue. Every round is a call at or above the primary's tier and re-reads the
diff, so cost grows linearly with rounds while the marginal value of each finding
falls. The loop terminates when the reviewer runs out of observations, not when the
code is correct, and on a large enough change those are far apart.

Two rounds is a provisional bound, not a measured optimum. Phase 6 measures the
distribution of rounds-to-no-new-findings across the eval corpus and the marginal
verified-defect yield of round N, and the bound moves to whatever that shows.

---

## 10. Agent loop and tool suite

### 10.1 Loop

Request, stream, collect tool-use blocks, permission check, execute (in parallel
where the tool is marked parallel-safe), append all results in a single message,
repeat until the model ends its turn. Provider-specific pause and resume semantics
are handled inside adapters, not in the loop.

### 10.2 Core tools

Deliberately small. Everything else arrives via MCP.

| Tool | Notes |
|---|---|
| `read` | Files, with offset and limit. Images where the tier supports vision. |
| `write` | Whole-file write. Requires a prior read of an existing file. |
| `edit` | Exact string replacement with a staleness check. |
| `exec` | Direct argv execution by default; explicit shell mode when needed. |
| `glob` | Path pattern matching. Parallel-safe. |
| `grep` | Content search. Parallel-safe. |
| `todo` | Task list state for long tasks. |
| `delegate` | Spawn a subagent at a named tier. |

The edit primitive uses exact string replacement with a staleness check that rejects
a write when the file has changed since the agent last read it. Line-number-based
edits are excluded; they corrupt files whenever anything else touches the workspace
concurrently. Reads return a content version, and writes or edits compare that
version after resolving symlinks and enforcing the workspace boundary.

Direct argv execution avoids accidental shell interpretation for ordinary commands.
Some development workflows genuinely need pipes, redirection, and expansion, so an
explicit shell mode remains available. Shell parsing and binary allowlists improve
prompts and policy matching; they are not a security boundary. An allowed
interpreter, package manager, compiler, or Git hook can execute arbitrary code, so
the sandbox and destination policy remain authoritative.

### 10.3 Failure handling and recovery

An agent loop spends most of its life on the unhappy path. The policies below are
part of the loop's contract, not incident response.

**Provider response validation.** Streamed events are validated against the adapter's
expected shape before entering the canonical log. A malformed tool-call argument
blob, a truncated JSON object, or an unknown block type is a typed adapter error, not
a partially-applied turn. Invalid tool arguments are returned to the model as a tool
error so it can correct them; invalid protocol-level content aborts the turn and
preserves the log.

**Streaming failure.** A dropped connection mid-turn leaves partial content. The
partial assistant message is retained in the session log, marked incomplete, and is
never presented as a finished turn. Recovery is provider-specific and owned by the
adapter: resume where the provider supports continuation, otherwise re-issue the turn
from the last committed message. Retries use bounded exponential backoff with jitter
and a per-turn attempt cap. A retry after partial output is billed twice by most
providers, so the cost model records both attempts rather than only the successful
one, and the estimator's error reporting (§6.6) includes retry overhead.

**Tool execution.** Every tool invocation runs under a wall-clock timeout and an
output size cap, both configurable per tool. Processes are launched in their own
process group and terminated group-wide on timeout or cancellation, escalating from
`SIGTERM` to `SIGKILL` after a grace period, so a shell that spawned children does
not leave orphans holding the workspace. Truncated output is explicitly marked as
truncated in the tool result rather than silently cut, because a model that cannot
tell truncation from completion will draw a confident wrong conclusion. A timeout is
a tool error returned to the model, not a turn abort; the model decides whether to
retry, narrow, or give up.

**Session integrity.** Replay validates each record's checksum and schema version. A
corrupt record truncates the log at that point, and the session resumes from the last
valid state with the loss reported to the user. A schema version newer than the
running binary is a refusal to load rather than a best-effort parse.

**Cancellation.** User interrupt cancels the in-flight provider request and the
running tool group, commits whatever is complete, and returns to the prompt with the
session resumable. Cancellation is not a crash path and does not lose the turn.

---

## 11. Permission and sandbox model

The largest trust surface in the product, and table stakes rather than a feature.

### Modes

| Mode | Behavior |
|---|---|
| `plan` | Read-only. No writes, no execution. |
| `default` | Prompt on write and execute. |
| `acceptEdits` | Auto-approve file edits. Prompt on execution. |
| `bypass` | No prompts inside the granted sandbox. Requires an explicit flag and workspace-scoped confirmation. |

### Rules

An allow/deny/ask rule set matched against tool name and argument shape, stored per
project and globally, with project rules layered over global. `exec` additionally
distinguishes direct argv execution from shell mode. Rules may constrain executable,
path, cwd, environment keys, network access, and argument patterns. A command string
is untrusted model output, and an allowlist is never described as containment.

### Sandbox

OS-level isolation is implemented and tested per platform. The runtime shows which
controls are active. Where required isolation is unavailable, `acceptEdits` does not
imply command execution; each execution is denied or separately approved.

**macOS is a research spike before phase 0 completes, not an implementation detail.**
Seatbelt is a policy language and a private framework, not a drop-in isolation
primitive. The `sandbox-exec` front end has carried a deprecation warning for years
while remaining functional and widely relied upon, and Apple has never committed to
its stability for third parties. The spike answers three questions with running code
before the phase-0 exit gate: whether a profile can be written that permits a real
build-and-test workflow while denying credential stores, agent sockets, and network
egress; whether the profile survives the toolchains developers actually run
(package managers, compilers, language servers); and what the fallback is if Apple
removes the front end. The starting point is a profile derived from published
open-source profiles for comparable tools, narrowed to the workspace boundary.

If the spike fails, macOS ships in per-action approval mode on the same terms as
Windows below. Shipping automatic execution on an isolation mechanism nobody has
verified would violate principle 4.

Linux uses namespaces or bubblewrap, which is the best-understood of the three.
Windows has no automatic execution until a native containment strategy meets the same
gate; see §19.3.

Network egress for executed processes is denied by default and granted separately
from filesystem access. Provider traffic passes through the provider broker and is
not inherited by child processes. The sandbox also enforces workspace path and
symlink resolution, a minimal environment allowlist, process/time/output limits,
and denial of credential stores and agent sockets unless explicitly granted.

Repository configuration is untrusted until the user trusts the workspace. Project
hooks, local MCP server commands, credential helpers, and permission-rule changes do
not execute during discovery or before that trust decision. `bypass` requires an
explicit flag and workspace-scoped confirmation, and the UI keeps it visibly active.

---

## 12. Sessions

- **Storage:** a schema-versioned, append-only event log per session, replayable to
  reconstruct full state. Records are length-delimited or checksummed so a partial
  final write can be detected and truncated after a crash. A lock prevents two
  processes from appending to one session. Sessions are files, not database rows.
- **Resume:** `sb --continue` for the most recent session in the working directory,
  `sb --resume <id>` for a specific one.
- **Fork:** branch a session at any turn into a new session sharing history up to
  that point.
- **Checkpointing:** snapshot in-scope workspace state before each edit batch and
  restore on demand. The implementation may use a content-addressed store or a
  shadow Git repository outside the workspace, but it never mutates the user's Git
  metadata. It has size limits, retention and garbage collection, explicit handling
  for untracked files and symlinks, and configurable exclusions for secrets and
  build artifacts. Restore always shows a preview and requires approval when it
  would overwrite user changes.
- **Compaction:** provider-native compaction where available, or a configured
  summarizer with a deterministic emergency fallback that preserves the task,
  decisions, changed files, failures, and unresolved work. Summaries record their
  source span and generator. Compaction is a cache-invalidating event and is
  scheduled accordingly.

Session and checkpoint directories are mode-restricted, are never telemetry input,
and have `export`, `delete`, and retention controls. Optional at-rest encryption
uses the credential-store design from §5.3; the document does not claim that file
permissions alone encrypt prompts or code.

---

## 13. Extensibility

Prioritized earlier than feels natural, because a one-person project cannot build
the long tail and this is the socket the long tail plugs into.

- **MCP.** Client support for stdio and Streamable HTTP, with legacy HTTP+SSE only as
  a compatibility path. Servers are configured per project and globally, with tool
  namespacing to avoid collisions. Remote servers require HTTPS and authorization;
  plaintext HTTP is limited to loopback endpoints. Any local HTTP listener shipped
  by Switchboard follows MCP's `Origin` validation and loopback-binding rules.
  Project-defined server processes are covered by workspace trust, permissions,
  and sandboxing.
  **Servers are added at session start or at an explicit rebuild.** Mid-session
  addition is not silent: where the target has a verified tool-change feature the
  harness uses it, and otherwise it requires user confirmation that names the cache
  cost, because rebuilding the tool prefix on a long session discards the entire
  cached prefix and pays to rewrite it. Declining the prompt queues the server for
  the next scheduled rebuild. The cache tracker is never updated optimistically.
  This is a deliberate UX decision rather than an implementation consequence: a
  configuration change that quietly costs a dollar is worse than one that asks.
- **AGENTS.md.** Read from the repository root and parent directories, merged
  root-to-leaf with nearer instructions taking precedence when they conflict. The UI
  can show instruction provenance. Use the existing convention rather than
  inventing a `SWITCHBOARD.md`.
- **Slash commands.** Markdown files under `.switchboard/commands/` with frontmatter
  declaring arguments and permitted tools.
- **Subagent definitions.** Markdown with frontmatter: name, description, default
  tier, permitted tools.
- **Hooks.** Shell commands bound to lifecycle events (pre-tool, post-tool,
  session-start, session-end). JSON on stdin; exit code and stdout control flow.
  Hooks are executable code: project hooks are disabled until workspace trust,
  inherit no credentials by default, run with time and output limits, and are
  subject to the same sandbox and egress policy as model-requested commands.

---

## 14. TUI

Bubble Tea, with lipgloss for styling and glamour for markdown.

Decisions that determine whether it feels good:

- **Streaming render path is custom.** Re-running a full markdown renderer on every
  delta is the standard mistake and the standard source of long-session jank.
  Glamour renders completed blocks; in-flight text uses a fast path with minimal
  styling and is re-rendered once on block completion.
- **Virtualized scrollback.** Render the viewport, never the full transcript. This
  is what keeps a 500-turn session responsive.
- **Diffs are rendered once and cached.** Syntax highlighting via chroma, computed
  on diff creation rather than on every repaint.
- **Status line always shows** the active tier, provider/model/effort target,
  session cost to date, and context window utilization. Routing must be visible at
  rest, not on demand.
- **Tier override** by command prefix (`/t3 <prompt>`) and by keybinding to pin a
  tier for the session.
- **Router decisions are shown inline**, collapsed to one line with the tier and a
  short rationale, expandable to the full decision record.

Performance targets: first paint under 50ms, input latency under 16ms, and no
full-transcript reflow on terminal resize.

---

## 15. Cost accounting

Per turn, per tier and route target: input, output, cache-read, and cache-write tokens,
hosted-tool fees where reported, and derived dollar cost using the active catalog
revision. Aggregated per session and per project. Exposed as `sb cost` and in the
status line. It is an estimator and reconciliation aid, not a replacement for the
provider's invoice; taxes, credits, negotiated rates, and unreported modifiers may
differ.

A hard task budget is enforced against a conservative preflight bound: cold-cache
input, configured maximum output/reasoning, router overhead, and known tool fees. If
that bound does not fit, the harness reduces the output cap, selects another
feasible target, or asks the user to revise the budget. Streaming cancellation is a
last guard, not an exact spending brake; providers may bill work already performed.

This is three things at once: unified cross-provider accounting for the user, the
feedback signal the router learns from, and the evidence behind the product's most
compelling claim, "routing saved you X this month." It is therefore not deferred.
Savings are labeled **estimated** and name the counterfactual baseline, eval
version, and uncertainty; observed spend alone cannot prove what an unchosen target
would have cost or whether it would have succeeded.

---

## 16. Telemetry

**Hard boundary, enforced in code rather than policy:** no prompts, no code, no
diffs, no file paths, no repository names, no environment variable values, no
command strings. The telemetry package cannot access those types.

Events carry a random pseudonymous install identifier: session start and end
(duration, turn count), tier decisions (tier, source, confidence bucket), escalation
triggers (type only), tool errors (class only), provider errors (status class),
latency histograms, token counts, version, OS, and architecture.

The event schema is published in-repo at `docs/telemetry.md` and treated as a
contract. Telemetry is off until the user explicitly opts in; declining is a stored
choice and does not reprompt on routine upgrades. `sb telemetry off`,
`SWITCHBOARD_TELEMETRY=0`, and `DO_NOT_TRACK=1` work. The backend and destination
are disclosed in the consent screen; PostHog is the initial implementation choice.

The telemetry package exposes only purpose-built metadata structs and has tests that
attempt to pass prompts, paths, diffs, command strings, repository identifiers, and
environment values across the boundary. The install identifier is random, can be
reset, and is not reused for hosted-account identity.

Update checks are separate product traffic, can be disabled, and are not repurposed
as version-distribution telemetry without telemetry consent. Network-level metadata
such as IP address may still be visible to the service and is disclosed rather than
described as anonymous in an absolute sense.

---

## 17. Accounts and the hosted tier

| | Free | Paid |
|---|---|---|
| Models | Bring your own keys | Hosted, proxied inference |
| Account | Not required | Required |
| Product telemetry | Off until opt-in | Disclosed operational metadata plus optional product telemetry; the §16 content boundary still applies |
| Works offline | Yes | No |

An account additionally unlocks settings and tier-configuration sync across
machines, team-shared configurations, and a cross-provider usage dashboard.

Mandatory accounts are excluded deliberately. On an open-source CLI where the user
supplies their own key and their own compute, a login gate buys nothing and is a
ten-line fork to remove. Gating the things that genuinely require a server produces
more signups than gating the tool.

Auth uses a device-code flow: `sb login` prints a code and opens a browser. Tokens
are stored in the OS keychain.

The hosted tier is not part of v0.1. Before implementation it requires a separate
threat model and design covering tenant isolation, proxy retention, abuse controls,
provider terms, billing and refunds, regional processing, deletion, support access,
incident response, and the distinction between inference content (which must reach
the selected model service) and product telemetry (which does not contain it).

---

## 18. Distribution and updates

Single default binary, cross-compiled for darwin, linux, and windows on supported
amd64 and arm64 combinations. Releases use signed, versioned update metadata plus
published checksums; a checksum served beside a compromised binary is not by itself
authenticity. The updater verifies target platform, size, hash, signature, rollback
rules, and atomic replacement before activation.

Self-update with stable and beta channels. Installs originating from a package
manager (Homebrew, apt, scoop) are detected and defer to it rather than fighting
it. Update checks are operational traffic and remain separate from telemetry as
specified in §16.

**The CGo tension, and its resolution.** A native ONNX runtime conflicts with the
fully-static, small, cross-platform binary goal. The default artifact therefore
contains the heuristic router and any learned model that can run acceptably in pure
Go. It must pass the product gate without ONNX.

If a larger encoder later produces a material measured gain, it ships as an
optional, version-matched helper or separately documented native variant. Dynamic
loading does not "collapse back" to a truly static artifact; it merely moves the
native dependency to runtime. Distribution size, cold start, memory, signature, and
fallback behavior are part of the classifier eval, not deferred packaging details.

---

## 19. Repository layout and build sequence

### 19.1 Layout

```
cmd/sb/                  CLI entry point
internal/
  agent/                 the loop
  tools/                 core tool suite
  permission/            rules, prompts, sandbox
  execution/             argv/shell runner and platform isolation
  session/               event log, resume, fork, checkpoints
  context/               zones, layout, compaction
  cache/                 breakpoint manager, state tracker, cost model
  provider/              canonical types and adapters
    anthropic/
    openai/
    google/
    openaicompat/
  catalog/               versioned targets, capabilities, pricing, provenance
  router/                heuristic, classifier, llm, chain
  orchestrator/          delegation, escalation, reviewer
  mcp/
  tui/
  telemetry/
  update/
  auth/
pkg/switchboard/         public SDK surface
eval/                    router eval harness and task sets
docs/
```

### 19.2 Sequence

Each phase has an explicit exit gate. Ordering is driven by dependency and by the
need to falsify the routing thesis before polishing a generic agent harness.

| Phase | Contents | Rationale |
|---|---|---|
| 0 | Minimal agent loop, streaming, `read`/`write`/`edit`/`exec`, permission model, platform sandbox capability report, crash-safe session log, and one provider. Minimal REPL only. | Exit: a small verified task corpus completes safely and sessions resume after forced interruption. |
| 1 | Canonical provider layer, versioned target catalog, auth, a second meaningfully different route target, manual tier selection, and observed cost accounting. | Exit: identical tasks run on two pinned targets and actual usage reconciles within documented estimator error. |
| 2a | Context zones, target-specific breakpoint manager, cache tracker, cost estimator, heuristic router, sticky-primary policy. Route decisions and cache state surfaced in the phase-0 REPL. | Exit: on eligible requests, observed cache hit rates match the policy's expectations, and estimate-versus-actual cost error is bounded and reported per target. The mechanism is shown to work before it is asked to prove anything. |
| 2b | Eval harness, tier-1 task corpus, verifiers, baseline runs, and the gate measurement. | Exit: pass the §7.1 falsification gate. If it fails, revisit or narrow the product before building the periphery. |
| 3 | TUI with visible route, target, budget, cache state, permission state, and estimate-versus-actual cost. | The validated core now gets its product surface. |
| 4 | MCP tools over stdio and Streamable HTTP, AGENTS.md, slash commands, hooks, workspace-trust flow, and subagent definitions. | Extensibility ships after its execution and cache invalidation boundaries are testable. |
| 5 | Additional first-party adapters, OpenAI-compatible profiles, fallback chains, and broader platform sandbox support. | Breadth follows two high-fidelity vertical slices rather than preceding them. |
| 6 | Orchestration: delegate tool, subagent scoping, conflict control, and risk-based reviewer pass. | Delegation is evaluated against sticky single-primary baselines. |
| 7 | Learned router candidates, local calibration, and tightly sandboxed shadow routing. | Ships only if it beats heuristics after runtime and distribution costs are included. |
| 8 | Separate platform program: accounts, sync, hosted inference, billing, and consented PostHog telemetry. | Requires its own threat model and operating plan; it is not part of the CLI MVP. |
| Deferred | Content-addressed tool cache, summary handoff, speculative warming. | See §6.7. |

Phase 2 is split because it is the longest stretch in the plan with no intermediate
verdict, which is the shape of work that quietly consumes a solo schedule. The 2a
gate is mechanical and cheap to measure: caches either hit or they do not, and the
estimator's error either bounds or it does not. Reaching it wrong is recoverable.
Discovering at the end of an undivided phase 2 that the cache layer never worked, and
that the gate therefore measured nothing, is not.

Note that the TUI is already downstream of the gate. Phase 2 runs against the phase-0
REPL, so neither the falsification measurement nor the decision to continue depends on
the terminal interface existing.

### 19.3 v0.1 boundary

Version 0.1 ends at phase 3 and intentionally includes only:

- macOS and Linux automatic execution where the sandbox capability tests pass;
- Windows in read-only or per-action mode until its native containment meets the
  same automatic-execution gate;
- two high-fidelity route targets, which may initially be two models on one provider
  if that produces the fastest valid cache-routing experiment;
- manual tiers plus the heuristic, cache-aware sticky router;
- the core tools, session resume, cost/budget display, and a focused TUI;
- local configuration and bring-your-own credentials.

Windows automatic execution, broad provider compatibility, MCP, hooks, subagents,
the learned router, media generation, public SDK stability, accounts, sync, hosted
inference, and product telemetry are post-v0.1 unless the phase-2 evidence changes
the priority. This boundary turns the solo schedule from "feature parity" into a
testable product slice.

**The Windows limitation is stated plainly, not discovered.** On Windows, v0.1 is a
plan-and-approve product: the agent reads, proposes, and edits under approval, and
every command execution requires per-action confirmation. That is materially less
capable than the macOS and Linux experience and it is a deliberate consequence of
principle 4, since shipping automatic execution without verified containment would
mean presenting a permission prompt as a sandbox. The release notes, the README, and
the first-run experience on Windows all say this in those terms. A user who
discovers the limitation by hitting it will reasonably read it as a bug.

---

## 20. Risks

**Scope against a solo timeline.** Feature parity with mature agent harnesses plus a
novel routing layer and hosted platform is realistically a multi-year product, not
one implementation milestone. The v0.1 boundary in §19.3 and the phase-2
falsification gate are the mitigation: breadth stops until the narrow thesis works.

**The router may not beat a fixed frontier model.** The entire product thesis is
that allocation beats always-using-the-best. If frontier model prices fall faster
than routing sophistication improves, the thesis weakens. The eval harness in §8.6
is the instrument for finding this out early rather than late, and it should be
treated as a falsification test, not a validation exercise.

**Capability is not a total order.** A tier ladder is simple enough to understand,
but a model can be better at one task class and worse at another. Hard constraints,
task-profile routing, target calibration, and visible low confidence reduce the
risk; they do not eliminate cold-start errors for unknown targets.

**Authentication policy can change.** Even documented OAuth or device flows can
change on a vendor's schedule. Only public, provider-authorized third-party flows
are eligible, and API keys or cloud credential chains remain the portable baseline.
The helper boundary limits the blast radius; it does not create an entitlement.

**Provider API drift.** Caching semantics on both major providers changed materially
within the last year. The target catalog and adapter contract isolate this, but
catalog maintenance, source verification, and regression fixtures are ongoing work
that never finishes. A compromised catalog is also a supply-chain risk, hence
signatures, provenance, rollback, and per-session revision pinning.

**Cache state is only partially observable.** Usage reports arrive after routing,
TTL guarantees may be minimums rather than exact expiry, and cache affinity is
provider-managed. The router must remain conservative under uncertainty and enforce
budgets against worst-reasonable cost, not a presumed hit.

**Evaluation can validate the harness instead of the product.** Self-reported
completion, subjective tier labels, benchmark contamination, and one-run model
variance all bias results. Independent verifiers, multiple runs, pinned versions,
predeclared thresholds, and honest uncertainty are required for the thesis test.

**The execution surface is hostile.** Shell commands, package scripts, compiler
plugins, Git hooks, MCP servers, and repository hooks can all escape simplistic
allowlists. Workspace trust, OS isolation, egress control, minimal credentials, and
fail-closed platform capability checks are release blockers, not polish.

**Local model tool-calling reliability.** Small local models frequently cannot do
reliable structured tool calling. Binding a low tier to one is a real support
burden, and the capability probe in §4 mitigates but does not solve it.

**Hosted operations are a separate company-sized scope.** Proxying user code adds
tenant isolation, billing, abuse, retention, compliance, and incident-response
obligations. Keeping it out of v0.1 avoids hiding that risk inside a late build
phase.

**Name.** "Switchboard" needs a trademark, GitHub, npm, and crates availability
check before commitment. "Patchbay" is the strongest alternative in the same
metaphor family.

---

## 21. Open questions

1. Does the eval set and UI remain comprehensible past roughly six tiers? A soft cap
   with a warning may be better than a hard limit even though the architecture
   permits arbitrary `N`.
2. Which fixed-width task-profile dimensions in §8.2 remain stable across model
   generations, and how much target-specific data is needed before they beat a
   scalar heuristic?
3. Should the phase-1 validation use two models on one provider to reduce adapter
   work, or two providers to exercise the real portability boundary sooner?
4. Is a single canonical message type sufficient, or should it include a typed
   opaque extension for provider reasoning, continuation, and tool-state items from
   the first release? Lossless replay argues for the extension; portability argues
   for keeping its use narrow.
5. How should budget enforcement divide between expected cost and worst-reasonable
   cold-cache cost when server cache state is uncertain?
6. Can a static tool-discovery broker preserve model tool-selection quality well
   enough to avoid putting a large MCP catalog in the cached prefix?
7. What native Windows containment can meet the same automatic-execution release
   bar as the macOS and Linux implementations?
8. Should shadow routing ever be on by default for a future hosted tier where the
   project pays for it? The default remains **no** until a separate consent and
   privacy review answers that question.

When a required capability has no approved feasible target, the default is already
decided: stop with an actionable error and offer configuration choices. Switchboard
does not silently remove the input, send it to an unapproved provider, or choose a
more expensive target outside the user's ceiling.

---

## Appendix A. Primary-source snapshot

These sources were checked on 2026-08-12. The target catalog must retain more
granular per-entry provenance; this list only anchors the design claims.

**These are snapshot references, not permanent links.** Provider documentation is
versioned by editing in place: the URLs below will keep resolving and will stop
describing what was read on the verification date, with no diff and no notice. Any
claim in this document that depends on a number from one of these pages is therefore
only as current as the header date, and a reader arriving later should assume drift
rather than assume agreement. Where a reader needs the state as-checked, an archive
snapshot of each URL taken on the verification date is the artifact to consult.

Reproducibility does not rest on this list. It rests on the target catalog's
per-entry `VerifiedAt`, `CatalogRevision`, and `SourceURLs` fields (§4) and on the
catalog revision pinned into each session record, which together let a historical
cost or route decision be reconstructed against the data that actually produced it.

- [OpenAI GPT-5.6 model guidance](https://developers.openai.com/api/docs/guides/latest-model)
  documents explicit and implicit prompt caching, cache-write billing, model IDs,
  reasoning controls, and the recommendation to monitor cache usage.
- [OpenAI model catalog](https://developers.openai.com/api/docs/models) and
  [API pricing](https://openai.com/api/pricing/) document current model snapshots,
  context limits, features, price bands, and cached-input prices.
- [Claude prompt caching](https://platform.claude.com/docs/en/build-with-claude/prompt-caching)
  documents minimum token thresholds, write/read multipliers, TTLs, usage fields,
  the 20-block lookback, the four-breakpoint limit, invalidators, concurrency, and
  pre-warming behavior.
- [Claude model IDs and versioning](https://platform.claude.com/docs/en/about-claude/models/model-ids-and-versions)
  documents pinned and aliased identifiers across serving surfaces.
- [Claude effort controls](https://platform.claude.com/docs/en/build-with-claude/effort)
  documents per-model effort support and the cache effect of changing effort.
- [Claude Opus 5 changes](https://platform.claude.com/docs/en/about-claude/models/whats-new-claude-4-8)
  documents the current beta for mid-conversation tool changes; it is not assumed
  to exist on other targets.
- [MCP transport specification](https://modelcontextprotocol.io/specification/2025-06-18/basic/transports)
  defines stdio and Streamable HTTP, deprecates HTTP+SSE as the primary transport,
  and specifies origin, loopback, and authorization considerations.
