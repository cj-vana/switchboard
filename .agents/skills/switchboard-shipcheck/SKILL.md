---
name: switchboard-shipcheck
description: Perform final offline verification of a Switchboard branch or working tree. Use before declaring a router, provider, TUI, extension, security, portability, or release change complete.
---

# Switchboard Shipcheck

Preserve unrelated working-tree changes. Inspect `git status`, the complete diff, and `AGENTS.md` before verification. Do not enable live tests or use provider credentials unless the user explicitly asks.

## Verify in order

1. Run `gofmt` only on changed Go files, then `git diff --check`.
2. Run focused tests for every changed package and the nearest integration surface. Include `-race` for concurrency, session, router, MCP, and TUI lifecycle changes.
3. Run `go test ./...` and `go vet ./...` offline.
4. Compile or vet both `GOOS=linux GOARCH=amd64` and `GOOS=windows GOARCH=amd64`. Run Unix-tagged behavior on a Unix host.
5. Run repository-specific fuzzers or deterministic stress loops for parsers, cancellation, races, ordering, and persistence touched by the diff.
6. Exercise user-visible CLI/TUI commands through their real dispatch and frozen-session boundaries.
7. Review generated schemas, help, completions, README, and `AGENTS.md` for drift. Claims must distinguish implemented compatibility from discovered-but-disabled or unsupported semantics.

If a sandbox blocks loopback listeners, subprocess controls, or cross-build caches, rerun the same check with the narrow permission it needs; do not reinterpret an infrastructure refusal as a product failure or skip the check silently.

## Review independently

Use separate correctness, concurrency, security, compatibility, and completeness passes when agents are available. Give reviewers the raw diff and repository constraints, not the intended conclusion. Fix findings, add regressions, and rerun the affected matrix. Repeat until the final review is clean.

Report exactly which checks passed, which were intentionally not run, and any remaining unsupported behavior. A green focused test is not a release result while repository-wide compilation or a P1 review finding remains.
