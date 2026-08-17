# Computer use

On macOS, a `computer` tool joins the suite at session assembly: the model
can read an application's windows and controls through the system
accessibility tree, click them, type, press key combos, and pick menu
items. It exists because a coding session regularly dead-ends at an app no
CLI reaches — the simulator that needs one button pressed, the browser
showing the rendered page, the dialog holding a build hostage — and the
choice at that point is this tool or your hands.

This document records how the tool holds its permissions, what was
verified against live applications before the schema froze, and what it
deliberately does not do.

## The surface

One tool, seven actions:

- `apps` — the running applications, window counts, which is frontmost.
- `state` — one app's windows, menu names, and an indexed element list:
  role, label, value, position, size. Launches the app (in the
  background, focus untouched) when it is not running.
- `click` — an element by index from the latest `state`, or a screen
  point by `x,y` for the elements the tree cannot name.
- `type` — real keystrokes into the app. A newline presses Return.
- `key` — a key or combo: `return`, `esc`, `cmd+s`, `cmd+shift+t`.
- `set` — write a field's accessibility value directly, and read back
  what it now holds.
- `menu` — a menu path, `File > Save` or `Format > Font > Bold`. A miss
  at any level answers with what that level actually holds.

Element indexes are valid only against the latest `state` of that app.
Each entry remembers its accessibility path — the chain of child ordinals
from the front window — and an action re-resolves that path and checks
the role found there. A mismatch answers "call state again", never a
click on whatever sits there now.

## One grant, probed honestly

Everything runs through System Events and `open(1)`; no target
application is ever scripted directly. This is a hard rule with a
measured reason: scripting an app by name needs a separate Automation
consent per app, and an unanswered consent dialog hangs the call for two
minutes before failing. System Events needs one grant — Accessibility,
to your terminal, under System Settings > Privacy & Security — and that
one grant covers every app the tool will ever touch.

The grant is deliberately not probed at session assembly, because the
probe itself can pop a consent dialog and startup is no moment to
interrupt the screen. The tool registers wherever the platform can serve
it (macOS with `osascript`, which every macOS has); the first call
surfaces the grant state with its remedy, and `sb doctor` probes it live,
where a dialog is the point rather than an interruption.

## The permission posture

Every action carries the external effect, the MCP posture, because the
action is the same kind: it happens outside the workspace and outside any
sandbox this host verified, on your own screen. No mode auto-allows it —
bypass included, since bypass suppresses prompts inside a granted sandbox
and a click on your screen was never inside one. Plan mode refuses it
outright, which also keeps `/race` arms read-only without a special case.

An approval covers the app for the session, not one byte-exact call —
the grain web approvals use for a host. The request names the app and
describes the act: `click [7] AXButton (Save) in TextEdit`.

Secrets are scanned in both directions. What `type` and `set` would put
into another app passes the credential scan first and a key-shaped string
is refused before it leaves — typing a key into a form is the
exfiltration the webfetch URL scan exists to stop. What comes back is
redacted unconditionally before it reaches the record, because another
app's UI can hold anything — a password manager included — and mid-turn
there is no one to ask.

## Verified against live applications

The capability rule (tested against the target, not its docs) produced
most of this design. Each of these contradicted a reasonable guess:

- **Per-element attribute reads cost ~25ms each** — one Apple event per
  read — so a naive walk of a 3000-element Safari window takes minutes.
  Bulk per-container reads answer a whole sibling list in one event. The
  state walk is breadth-first with bulk reads under an element cap and a
  stated time budget: app chrome arrives first and cheap, deep web
  content is read partially and the output says so. (A web page is
  better read with `webfetch` anyway; the tool description says that
  too.)
- **Window-chrome buttons swallow synthetic clicks.** A close button
  that lists an AXPress action takes the press and does nothing, while
  menu items, sheet buttons, and ordinary app buttons click fine. The
  reliable route to closing things is the menu or the keyboard
  (`cmd+w`), and the tool description steers there.
- **Modern SwiftUI apps expose anonymous elements.** Calculator's digit
  buttons carry no title, no description beyond "button", no value. This
  is why `state` reports positions and `click` takes coordinates: the
  named path is preferred, the coordinate path is the verified fallback.
- **Direct app scripting hangs on consent.** `Application("TextEdit")`
  from a terminal without that specific Automation consent blocks until
  an AppleEvent timeout. Hence the System-Events-only rule above.
- The key sequence the live test uses to discard its scratch document —
  `cmd+w`, the save sheet, `cmd+delete`, `cmd+q` — was verified by hand
  first, sheet and all.

The wire fixtures the offline tests parse were captured from real
osascript runs (`internal/tools/testdata/computer_*.json`), and the live
test that captured them (`SB_LIVE=1`, macOS) drives the real tool end to
end against a scratch TextEdit document and leaves the machine as it
found it.

## Stated limits

- **No screenshots, on purpose.** A tool result is text in this
  harness, and `screencapture` without the separate Screen Recording
  permission silently returns wallpaper-only frames — there is no
  cgo-free way to detect that in advance, and a capture that might be
  lying is worse than a stated absence. The accessibility tree is the
  interface; it is also what a model can act on precisely.
- **No scrolling action.** Scroll-wheel synthesis is not available
  through System Events; page and arrow keys through `key` cover most of
  what scrolling is for.
- **Keystrokes go to the frontmost app**, so `type`, `key`, and `click`
  activate the target first. Reading `state` never steals focus.
- Everything is visible on your screen as it happens. That is a feature:
  the same visibility every routing decision gets, applied to the
  model's hands.
