# Computer use

On macOS, the optional `computer` tool reads and operates application controls
through the Accessibility API. It can inspect windows, click controls, type,
press key combinations, set field values, and choose menu items. Use it for UI
work that has no CLI or direct API.

The tool is assembled only where macOS and `osascript` are available. Its
behavior was verified against live applications before the schema was fixed.

## Actions

| Action | Behavior |
| --- | --- |
| `apps` | List running applications, window counts, and the frontmost app |
| `state` | List one app's windows, menus, and indexed elements with role, label, value, position, and size; launch the app in the background if needed |
| `click` | Click an indexed element from the latest state, or a screen point for a control the tree cannot name |
| `type` | Send keystrokes; a newline presses Return |
| `key` | Send a key or combination such as `return`, `esc`, `cmd+s`, or `cmd+shift+t` |
| `set` | Write an accessibility value directly and read it back |
| `menu` | Choose a path such as `File > Save`; on a miss, report the entries at the failed level |

An element index is valid only for the latest `state` of that application. The
state record stores the accessibility path plus opaque fingerprints for the
front window and element. Before an indexed `click` or `set`, Switchboard
activates the app, resolves the path again, and checks both fingerprints.

The element fingerprint binds its role, title, description, and value.
Anonymous controls also bind their position and size. A replaced window,
modal, or same-role control returns “call state again” instead of acting on the
new element. Raw identity inputs remain inside the accessibility script and do
not appear in mismatch logs.

## Accessibility grant

All operations go through System Events and `open(1)`. Switchboard does not
script target applications by name. Direct application scripting needs a
separate Automation consent for each app, and an unanswered dialog can block
until the AppleEvent timeout.

System Events uses one Accessibility grant for the terminal under System
Settings > Privacy & Security. Switchboard does not probe the grant during
session assembly because the probe can open a consent dialog. The first tool
call reports the missing grant with a remedy. `sb doctor` performs the live
probe explicitly.

## Permission and secrets

Every computer action has the external permission effect. It occurs outside
the workspace and outside command confinement, so no bounded permission mode
auto-approves it, including bypass; yolo, the everything-grant, does. Plan
mode denies it. An approval
is scoped to the application for the current session.

The approval detail uses non-sensitive metadata, for example
`click [7] AXButton in TextEdit`. Accessibility titles, descriptions, values,
and identity fingerprints do not enter approval details or action errors.

Text sent through `type` or `set` passes the credential scanner first. A known
credential form is refused before it reaches the application. Text read from
an application is redacted before it enters the session because a UI can
contain secrets, including password-manager data.

## Live verification notes

Live tests established these operating constraints:

- Per-element attribute reads cost about 25 ms each. `state` therefore uses
  bulk per-container reads, breadth-first traversal, an element cap, and a time
  budget. Application chrome arrives first. Deep web content may be partial,
  and the result says so.
- Window-chrome buttons can ignore synthetic AXPress actions. Menu commands or
  keyboard shortcuts such as `cmd+w` are more reliable for window management.
- Modern SwiftUI applications may expose anonymous controls. Calculator digit
  buttons are one example. Positions and coordinate clicks provide the tested
  fallback.
- Direct target-app scripting can block on Automation consent. This is why the
  implementation uses System Events only.
- The scratch-document cleanup sequence used by the live test, including its
  save sheet, was verified manually before automation.

Offline parser fixtures in `internal/tools/testdata/computer_*.json` came from
real `osascript` runs. The `SB_LIVE=1` macOS test drives the tool end to end
against a scratch TextEdit document and restores the application state when it
finishes.

## Limits

- No screenshots. `screencapture` can return wallpaper-only frames when Screen
  Recording permission is absent, and the current implementation cannot
  verify the frame without adding a native dependency.
- No scroll-wheel action. Page and arrow keys cover common scrolling through
  the `key` action.
- `type`, `key`, and `click` activate the target because keystrokes go to the
  frontmost application. Reading `state` does not change focus.
- Actions are visible on the user's screen while they run.
