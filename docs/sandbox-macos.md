# macOS confinement

How Switchboard confines commands on macOS, why each rule exists, and what the
profile does not protect against.

The policy lives in `internal/execution/seatbelt.sb` and is applied by
`internal/execution/capability_darwin.go`. Every rule here was arrived at by
running it against real toolchains on a real machine. Where a rule looks
redundant, it is usually load-bearing in a way that is invisible until it is
removed.

## The contract

A confined command may:

- read almost anything on the machine, except the paths in the deny block;
- write inside the workspace, `$TMPDIR`, and a fixed set of build caches;
- execute other programs, allocate terminals, and fork;
- bind and connect to loopback addresses.

It may not:

- write anywhere else, including `$HOME` and `/private/tmp`;
- read the credential stores in the deny block, or reach the Keychain API;
- use the ssh agent;
- reach the network off this machine unless egress was granted for that command.

## Four things that are easy to get wrong

**Paths must be pre-resolved.** Seatbelt fully resolves a path before matching
it. A rule naming `/tmp`, `/var`, or `/etc` never fires, because the kernel is
matching `/private/tmp` and friends. The renderer runs every parameter through
`filepath.EvalSymlinks` first. A rule written against a symlink is worse than a
missing rule: the profile still reads as strict.

`$TMPDIR` is the specific trap. On macOS it is `/var/folders/...`, not `/tmp`,
and a profile that grants `/tmp` will fail every Go build with
`mkdir /var/folders/.../go-build...: operation not permitted`.

**Later rules win, so denies go last.** The deny block sits at the bottom of the
profile and overrides the broad `(allow file-read*)` above it. Moving a deny
above the grant it is meant to override silently disables it.

**`mach-lookup` is granted nowhere.** `securityd` answers keychain queries over
mach IPC, so denying the keychain *files* accomplishes nothing on its own:
`security list-keychains` returns real output through a broad `(allow
mach-lookup)` while the profile looks strict. Denying mach-lookup entirely
closes it, and no toolchain tested here needs it. If a future toolchain does,
grant the specific service and re-check the keychain assertion.

**Binding a port needs `network-inbound` as well as `network-bind`.** With
`network-bind` alone, `net.Listen` fails with `operation not permitted`, which
reads like a kernel refusal rather than a missing rule. This matters more than
it sounds: `httptest.NewServer` and every equivalent fixture server in other
ecosystems binds an ephemeral loopback port, so without it the sandbox breaks
the most common thing a coding agent runs.

## Network

Two levels, because §11 requires egress to be granted separately from
filesystem access.

`NetworkLoopback` is the default. Loopback bind and connect are allowed;
everything else is denied. Egress is refused by rule, not as a side effect of
DNS failing, which was confirmed by dialing a raw address with no name lookup
involved.

`NetworkFull` appends `(allow network*)` and is requested per command through
the `network` field on the `exec` tool. It always requires the user's approval,
even on a host with verified containment: the sandbox governs what a command
can read and write, and it cannot judge whether sending this workspace to the
internet is what the user meant.

Seatbelt cannot filter by hostname, so there is no middle ground between
loopback and open egress. Do not add a `blockedHosts`-shaped option; it would
not be enforceable.

## Verification

`PolicyVerified` is not a constant and not a configuration flag. It is the
existence of a `*Confinement`, which is produced only by a self-test that passes
on this machine, and that same value is what wraps the command. The two cannot
disagree, which is the point: a harness that believes it is contained while
running commands unconfined is the exact failure design principle 4 exists to
prevent.

The self-test checks the security-critical direction, that things which must be
refused are refused, plus enough of the allowed direction to catch a profile
that simply breaks everything. It runs at startup and is cached in
`~/.switchboard/sandbox-check.json`, keyed by the profile hash and the macOS
build. Editing the profile or updating the OS invalidates the cache, because
neither should inherit an old pass.

If any assertion fails, the whole profile is refused rather than partially
trusted, and macOS falls back to per-action approval.

Whether real toolchains still work is checked by the darwin-tagged tests rather
than at startup, since it needs compilers the user may not have installed.

## What this does not protect against

**Reads leak by default.** The profile allows broad reads and subtracts a deny
list. Any secret whose location is not enumerated there is readable. A read
allowlist is the safer shape and it breaks toolchains continuously, which is a
maintenance cost this project does not currently absorb. This is the open
question the profile carries: it should be revisited before v0.1 ships, and the
deny list should grow whenever a new credential location becomes common.

**Cache directories are a persistence vector.** Granting writes to `~/.cargo`,
`~/.npm`, and the rest is what makes a second build fast. It also lets a command
plant a config file or a compiled artifact that a later, separately approved
command will execute. Confinement is per command; it is not a durable boundary
between commands.

**`$TMPDIR` is writable.** Build tools are unusable without it. A confined
command can therefore write anywhere in the per-user temp directory, including
files another process may later read.

**Nothing here confines the model.** The sandbox constrains commands. It does
not constrain what the agent reads into context and sends to a provider, which
is the destination policy's job (§16, principle 6).

## Depending on `sandbox-exec`

The front end at `/usr/bin/sandbox-exec` has carried a deprecation warning in
the headers since 10.8. It emits no warning at runtime, it remains what Apple's
own software and Chromium use for sandboxing, and it is not plausibly going away
soon.

The posture is to depend on it and keep per-action approval as the
always-available path, which is already how every unverified host behaves. If
Apple removes it, macOS degrades to the same plan-and-approve product Windows
ships today, and no code path has to be rewritten to make that safe. The
alternatives, should it become necessary, are the Endpoint Security framework,
which needs an entitlement and notarization and is heavyweight for an
open-source CLI, or a helper linking the private `sandbox_init`.

## Adding a toolchain

When a tool fails under the profile:

1. Run it under the profile directly to see the refusal.
2. If it needs to write outside the workspace, add its cache to the write list
   with a comment naming the tool, and weigh the persistence cost above.
3. If it needs a mach service, add that specific `global-name`, then re-run the
   keychain assertion in the self-test. A broad `(allow mach-lookup)` reopens
   the credential store.
4. Add it to `TestInstalledToolchainsWorkConfined` so the next profile change
   does not silently break it.

Editing the profile changes its hash, which invalidates every cached
verification automatically.
