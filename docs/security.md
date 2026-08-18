# Security

Switchboard separates permission, containment, workspace trust, and credential
handling. A permission answer does not claim that a process is sandboxed.

## Permission modes

Tools declare an effect: read, write, execute, or external. The permission
engine applies rules and the active mode to that effect.

| Mode | Reads | File writes | Commands | External tools |
| --- | --- | --- | --- | --- |
| `plan` | Allowed | Denied | Denied | Denied |
| `default` | Allowed | Ask | Ask | Ask |
| `acceptEdits` | Allowed | Allowed | Ask | Ask |
| `auto` | Allowed | Allowed | Eligible non-Windows direct commands receive bounded model review; opaque interpreter, sensitive, and shared-host-loopback commands ask | Ask |
| `yolo` | Allowed | Allowed | Allowed with full, unconfined host reach; sensitive commands ask | Ask |
| `bypass` | Allowed | Allowed | Allowed only when verified confinement isolates host network and IPC; current production profiles ask | Ask |

External tools act outside the workspace and outside Switchboard's command
sandbox. MCP and computer-use calls therefore require an explicit rule or user
approval even in bypass or yolo mode. A remembered approval lasts for the
current session and is scoped to the permission identity, not to arbitrary new
tools that happen to sanitize to the same display name.

Auto mode sends eligible direct-command metadata to the `[slots] approver` when
set; otherwise it tries reachable ladder tiers from the bottom. The packet
contains the tool, path, argv, network request, effective reach, and retained
host IPC or loopback authority. It
excludes file contents, environment values, and external tool arguments. The
reviewer can allow, deny, or escalate. Missing reviewers, errors, invalid
decisions, and explicit escalation go to the user. A context with no listener
denies an unresolved command. The review result is recorded with the
permission event, and its model call is budgeted and tagged as approval work.
Shell form and inline interpreter code such as `sh -c`, `python -c`, or
`node -e` skip model review, as do sensitive commands and ordinary commands in
a macOS Seatbelt sandbox. An explicit full-network request does not carry the
shared-host-loopback posture and remains reviewable, with retained host IPC
authority disclosed. Linux direct argv remains reviewable with its retained
host IPC authority disclosed. Windows execution remains human-gated because
descendant cleanup is not guaranteed.

Human approval views escape terminal controls. A very large command is visibly
shortened while retaining its executable and early flags, its tail, and an
omitted-character count; the shortening is display-only.

`yolo` is deliberately unconfined. It does not override explicit deny rules,
the outbound-secret gate, or external-tool approval. The current sandbox
selection is retained while `yolo` forces full host reach, so leaving `yolo` can
restore the selected posture.

## Command containment

The sandbox is off by default. Approved commands then have the user's normal
filesystem and network reach. `sb -sandbox` requires verified confinement at
startup. `/sandbox on|off|auto|status` changes or reports the posture for the
running process. `on` fails if the host cannot apply its verified profile;
`auto` uses the profile when available and otherwise stays visibly off. A
successful interactive change is also saved as the next launch default.

Switchboard uses Seatbelt on macOS and a provenance-checked system bubblewrap
on Linux. A confined command can write inside the workspace and approved build
caches, cannot read
credential stores or their service sockets, and cannot open direct
non-loopback connections unless its permission decision authorizes network
access. On macOS, an existing host-loopback service can act with its own
authority. Both production profiles expose some host-local IPC; see the
platform details before treating confinement as isolation from local services.

Windows has no verified containment profile. `/sandbox on` fails there and
`auto` remains off. Permission mode still decides whether a host-direct command
asks, is denied, or receives the explicit yolo grant. The full filesystem and
network contract is in [Confining commands](sandbox.md). In yolo mode,
descendant processes may survive cancellation; the approval reason states this
limit.

## Workspace trust

A checkout can declare MCP servers and hooks under `.switchboard/`, and its
build graph can control a language server. None of those processes start until
the workspace is trusted.

`/trust` previews the exact declared MCP servers, hook commands and tool
filters, and language-server candidate without starting them. `/trust grant`
records permission for the resolved checkout. `/trust revoke` removes it.

User files under `~/.switchboard` are treated as the user's own configuration.
Native Codex and Claude project MCP entries still require Switchboard trust;
another client's remembered trust does not cross the boundary. Plugin
executable trust is separate and digest-bound. See
[Native extension compatibility](extensions.md).

## Credential lookup

Local targets need no credential. Other providers resolve credentials in this
order:

1. `SB_<PROVIDER>_API_KEY`, or the vendor's standard environment variable;
2. a configured credential helper;
3. an OAuth login;
4. the OS credential service, using Keychain on macOS or Secret Service on
   Linux.

`/login`, `sb auth`, and first-run setup read secrets from a masked prompt or
stdin. Secrets are not accepted on the command line and are not written to
configuration, session logs, or errors. Status output uses placeholders.

There is no encrypted-file fallback. A mode-0600 file controls access but does
not encrypt its contents. On a machine without a credential service, use an
environment variable or helper:

```toml
[auth.anthropic]
helper = ["op", "read", "op://vault/anthropic/credential"]
```

The helper is provider-wide. A helper that returns a Codex CLI token follows
Codex's expiration schedule; running `codex` refreshes that login. `/setup` can
configure this helper when it finds an existing Codex login.

## Outbound secret checks

Before a prompt leaves the machine, Switchboard scans it for known credential
prefixes. The scan includes file attachments and output injected by `!cmd`.
It uses known issuer formats rather than entropy guesses so a match has a clear
meaning.

In the TUI, a match offers three choices: redact it, send the original text, or
drop the prompt. Redaction tells the model where a credential was removed. A
headless `sb -p` run cannot ask, so it refuses the send unless
`-allow-secrets` is present. Diagnostics name the credential type and prefix
without repeating the value.

Web queries and URLs are scanned before egress. The first request to a host
asks for approval, and that approval covers only that host for the session. A
redirect to a different host is refused. No permission mode skips this check.

Computer-use text is scanned before it is typed into another application, and
text read from applications is redacted before it enters the session. See
[Computer use](computer.md).

## Configured processes

Legacy Switchboard MCP child processes inherit the ordinary environment after
SSH-agent sockets and names associated with secrets, tokens, keys, passwords,
credentials, authentication, sessions, cookies, service URLs, and DSNs are
removed case-insensitively. Restricted and native declarations start from a
small baseline, add only named forwarded variables, then apply explicit
environment values.

Hooks are the user's standing commands. They run without a per-call prompt and
outside the command sandbox, which is why repository hooks require workspace
trust. An armed `/watch` verifier is also unconfined, but only the user can arm
one; repositories cannot declare it.

## OpenAI subscription OAuth

`sb auth oauth login openai/subscription` uses the OAuth client registered by
OpenAI's Codex CLI. Switchboard is not affiliated with or endorsed by OpenAI,
and OpenAI does not publish this flow for third-party clients. Accounts have
been actioned for using it. OpenAI's terms govern the account.

A client you register under `[auth.openai.oauth]` takes precedence over the
bundled client.
