# Security

## Reporting

Report vulnerabilities through GitHub's private vulnerability reporting on
this repository (Security tab, "Report a vulnerability"). Please do not open
a public issue for anything exploitable. You will get an acknowledgment
within a few days, and credit in the fix's release notes unless you prefer
otherwise.

The latest release is the supported version. Fixes ship as a new release,
and the auto-updater moves installs forward by default.

## What this program promises

**Credentials.** A credential enters through stdin or a masked prompt, never
the command line. It is stored in the OS credential service (Keychain on
macOS, Secret Service on Linux) or read from the environment or a helper.
It is never written to the config file, the session log, or any error, and
the secret type has no rendering that shows its value. Commands the agent
runs get the harness's provider credentials stripped from their environment.

**Sandboxing.** Automatic command execution is granted only where
confinement has been demonstrated by a self-test on that host: Seatbelt on
macOS, bubblewrap on Linux. A confined command writes only inside the
workspace, temp, and build caches; cannot read the enumerated credential
stores or reach the daemons that serve them; and cannot reach the network
beyond loopback. Network access off the machine is a separate grant that
always prompts. [docs/sandbox.md](docs/sandbox.md) is the full profile.

**Updates.** Releases publish per-platform archives beside a sha256
`checksums.txt`. The installer and the self-updater verify the checksum
before anything replaces the running binary, and the replacement is atomic.

## What it deliberately does not promise

These are documented limits, not oversights. Reads leak by default outside
an enumerated deny list; the sandbox governs writes and credential access
more tightly than reads. Writable build caches persist between commands and
are a persistence vector. Windows has no verified containment, so every
command there needs individual approval in every mode, and that is the
protection. Release checksums prove integrity, not authenticity: a checksum
served beside a compromised binary proves nothing, and signed update
metadata is designed (§18 of the design document) but not yet shipped.
Custom-command files from a cloned repository can shape prompts but cannot
execute shell at expansion time; only files under `~/.switchboard/commands`
can, because they are yours.

If you find a gap between this document and the code's behavior, that gap
is a bug worth reporting even when the behavior itself is not exploitable.
