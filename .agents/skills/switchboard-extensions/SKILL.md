---
name: switchboard-extensions
description: Add or review native Codex and Claude skills, plugins, MCP servers, hooks, LSP components, manifests, marketplaces, and activation flows in Switchboard. Use for extension discovery, compatibility, installation, trust, permissions, or protocol work.
---

# Switchboard Extensions

Treat extension compatibility as an adapter problem with three separate states: discovered inventory, explicit Switchboard activation, and executable trust.

## Discovery

1. Read only exact documented roots and explicit caller-supplied plugin paths. Discovery must not execute, dial, expand secrets, scrape caches, or inherit another client's trust.
2. Preserve dialect, scope, source file, real path, native ID, and whole-entry precedence. Retain conflicts visibly; never flatten two authorities into one short name.
3. Bound files, tree depth, entries, and aggregate bytes. Revalidate opened roots and reject traversal, symlink/junction escape, replacement races, `.git` executable paths, duplicate keys, ambiguous identity, and unsupported behavior-bearing fields.
4. Keep marketplace inventory distinct from installed bytes. Native enabled state is provenance only.

## Skills and plugins

- Honor model- and user-invocation controls before advertising a skill. Unsupported tool/model/context/shell controls must block invocation with a diagnostic rather than disappear.
- Resolve supporting files inside the anchored skill or plugin root.
- Namespace plugin components by canonical plugin identity.
- Enabling prompt-only components must not grant executable trust. Bind executable trust to the exact root and content digest, and invalidate it when bytes change.
- Install through a bounded staging copy, rediscover and match identity/digest, then atomically promote. Never run lifecycle scripts during install.

## MCP

- Keep native values redacted through discovery and diagnostics. Release them only after exact definition activation, project trust where required, and an explicit runtime-feature check.
- Preserve transport, cwd, restricted environment forwarding, headers, bearer references, timeouts, required state, tool filters, and approval behavior. Refuse semantics the runtime cannot implement.
- Treat tool schemas as frozen for the session. Relist only at a new assembly boundary.
- Generate permission rules from the exact raw server/tool identity that successfully registered; sanitized-name collisions fail closed.
- Test modern and initialization-era protocol negotiation, cancellation, auth/error non-downgrade, and secret isolation.

Use offline golden fixtures for Codex and Claude user/project/local layers, manifestless and manifest plugins, malformed higher-precedence files, duplicate/case collisions, traversal, symlinks, CRLF/BOM, and Windows path/environment behavior. Run focused race tests, vet, and Linux/Windows compile checks.
