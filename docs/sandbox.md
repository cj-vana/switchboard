# Confining commands

How Switchboard confines what the agent runs, how each platform earns the right
to execute without asking, and what the confinement does not protect against.

| Platform | Mechanism | Status |
|---|---|---|
| macOS | Seatbelt via `sandbox-exec` | Verified; automatic execution available |
| Linux | bubblewrap | Verified; automatic execution available |
| Windows | none | Per-action approval only (§21.7) |

Every rule on both platforms was arrived at by running it. Where one looks
redundant it is usually load-bearing in a way that is invisible until it is
removed, and the sections below say which.

## The contract

Both platforms implement the same promise. A confined command may:

- read almost anything on the machine, except the paths listed as hidden;
- write inside the workspace, the temp directory, and a fixed set of build
  caches;
- execute other programs, allocate terminals, and fork;
- bind and connect to loopback addresses.

It may not:

- write anywhere else, including the home directory;
- read the enumerated credential stores, or reach the daemon that hands out
  credentials;
- use the ssh agent;
- reach the network off the machine unless egress was granted for that command.

## Verification

Whether confinement is available is not a constant, a build tag, or a
configuration flag. `Capability` carries a `*Confinement`, which is produced
only by a self-test that passes on this machine, and that same value is what
wraps the command. The two cannot disagree, which is the point: a harness that
believes it is contained while running commands unconfined is exactly the
failure design principle 4 exists to prevent. `Run` fails closed when it has a
confinement it cannot apply.

The self-test checks the security-critical direction, that things which must be
refused are refused, plus enough of the allowed direction to catch a profile
that simply breaks everything. Reads of a hidden file are checked by content
rather than exit status, using a canary written into Switchboard's own state
directory, because a hidden file can surface as an empty successful read rather
than an error.

Results cache in `~/.switchboard/sandbox-check.json`, keyed by a hash of the
profile and by the host's OS build or kernel. Editing the confinement or
updating the system invalidates the cache, because neither should inherit an old
pass. Any failed assertion refuses the whole profile rather than trusting the
part that still works.

Whether real toolchains still function is checked by the platform-tagged tests
rather than at startup, since it needs compilers the user may not have.

## Network

Two levels, because §11 requires egress to be granted separately from filesystem
access.

`NetworkLoopback` is the default: fixture servers on ephemeral loopback ports
work, and nothing reaches off the machine. This is not a detail. A test suite
standing up a local server is the single most common thing an agent runs, and a
sandbox that breaks it is a sandbox nobody keeps on.

`NetworkFull` is requested per command through the `network` field on the `exec`
tool and always requires the user's approval, even where containment is
verified. The sandbox governs what a command reads and writes; it cannot judge
whether sending this workspace to the internet is what the user meant.

Neither platform can filter by hostname, so there is no middle ground between
loopback and open egress. Do not add a per-domain option; it would not be
enforceable.

## macOS

The policy is `internal/execution/seatbelt.sb`, rendered with parameters passed
as separate argv elements so a workspace path containing a quote cannot rewrite
it.

**Paths must be pre-resolved.** Seatbelt fully resolves a path before matching,
so a rule naming `/tmp`, `/var`, or `/etc` never fires. `$TMPDIR` is the trap:
on macOS it is `/var/folders/...`, and a profile granting `/tmp` fails every Go
build with `operation not permitted`. A rule written against a symlink is worse
than a missing rule, because the profile still reads as strict.

**Later rules win, so denies go last.** Moving a deny above the grant it is
meant to override silently disables it.

**`mach-lookup` is granted nowhere.** `securityd` answers keychain queries over
mach IPC, so denying the keychain files accomplishes nothing on its own:
`security list-keychains` returns real output through a broad `(allow
mach-lookup)` while the profile looks strict. Denying it entirely closes that,
and no toolchain tested needs it. If one ever does, grant the specific service
and re-run the keychain assertion.

**Binding a port needs `network-inbound` as well as `network-bind`.** With
`network-bind` alone `net.Listen` fails as though the kernel refused.

`sandbox-exec` has carried a deprecation warning in the headers since 10.8. It
emits nothing at runtime and remains what Apple's own software and Chromium use.
The posture is to depend on it and keep per-action approval as the
always-available path, which is already how every unverified host behaves. If
Apple removes it, macOS degrades to the Windows plan-and-approve model with no
code path rewritten.

## Linux

Confinement is a namespace construction rather than a policy language.
bubblewrap builds a filesystem view: the whole tree read-only, writable binds on
top, and empty mounts over what must not be readable.

**Order is the policy.** Mounts apply in sequence, so writable binds must come
after the read-only root and the deny mounts must come after those. A deny
placed before a bind covering the same path silently does nothing.

**A deny mount needs an existing mountpoint.** `--tmpfs` on a path that does not
exist fails the entire invocation, because bubblewrap cannot create the
directory under a parent that is already read-only. Absent paths are filtered
out, which is safe: there is nothing there to hide.

**A private network namespace comes with working loopback.** Unlike macOS, no
extra grant is needed; `--unshare-net` gives fixture servers a usable `lo` and
no route off the machine.

**The session bus is the keychain equivalent.** gnome-keyring, kwallet, and
anything else implementing the Secret Service API hand out credentials over
D-Bus, so hiding `~/.ssh` while leaving `$DBUS_SESSION_BUS_ADDRESS` reachable
repeats the mistake macOS made with securityd. The bus socket and
`$XDG_RUNTIME_DIR/keyring` are covered.

**`/bin/sh` is dash on Debian and Ubuntu.** The egress check cannot use
bash's `/dev/tcp`, because on those systems it fails identically whether or not
the network is reachable, which would make the assertion pass while measuring
nothing. The self-test uses `curl`, `wget`, or `nc`, and says so in its detail
string when none is available. The policy mapping itself is covered by a
deterministic test that needs no network at all.

**Unprivileged user namespaces are a kernel setting.** Some distributions and
hardening profiles disable them, and then bubblewrap cannot build a namespace at
all. That is a host property, not a defect, and it correctly results in
per-action approval.

**Killing the wrapper tears down the PID namespace,** so a timed-out build does
not leave its compiler running. Verified by liveness rather than by pid, since a
pid inside the namespace means nothing outside it.

## What this does not protect against

**Reads leak by default.** Both platforms allow broad reads and subtract an
enumerated list. Any secret whose location is not on that list is readable. A
read allowlist is the safer shape and it breaks toolchains continuously, which
is a maintenance cost this project does not currently absorb. This is the open
question the design carries: it should be settled before v0.1, and the hidden
list should grow whenever a new credential location becomes common.

**Cache directories are a persistence vector.** Granting writes to `~/.cargo`,
`~/.npm`, and the rest is what makes a second build fast. It also lets a command
plant a config or a compiled artifact that a later, separately approved command
executes. Confinement is per command; it is not a durable boundary between
commands.

**The temp directory is writable,** because build tools are unusable without it.
A confined command can write anywhere in it, including files another process may
later read.

**Nothing here confines the model.** The sandbox constrains commands. It does
not constrain what the agent reads into context and sends to a provider, which
is the destination policy's job (§16, principle 6).

## Adding a toolchain

When a tool fails confined:

1. Run it under the confinement directly to see the refusal.
2. If it needs to write outside the workspace, add its cache with a comment
   naming the tool, and weigh the persistence cost above.
3. If it needs a service the confinement blocks, grant that specific one, then
   re-run the credential assertions. A broad grant reopens the credential store
   on both platforms.
4. Add it to `TestInstalledToolchainsWorkConfined` so the next change does not
   silently break it.

Editing either confinement changes its key, which invalidates cached
verifications automatically.

## Testing Linux from macOS

The Linux path was developed and verified in a container:

```
docker build -f Dockerfile.linuxdev -t sb-linuxdev .
docker run --rm --privileged -v "$PWD:/src" -w /src sb-linuxdev go test ./...
```

`--privileged` is required because Docker Desktop's kernel does not allow
unprivileged user namespaces inside a container, which bubblewrap needs. Real
Linux desktops generally do allow them. A privileged container is a good proxy
for the construction but not proof about a specific host, which is why the
self-test still has to pass where the binary actually runs.
