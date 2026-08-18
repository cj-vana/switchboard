package tools

// The fixed JXA programs the computer tool hands to osascript. Every one
// was run against live applications before it was frozen here — the walk
// against a 3000-element Safari window and a TextEdit document, the click
// forms against Calculator's unlabeled buttons, the key and menu forms
// against TextEdit's save sheet — because System Events contradicts
// reasonable guesses: bulk attribute reads are ~200x cheaper than
// per-element ones, scripting a target app directly hangs on a consent
// dialog System Events never needs, and window-chrome buttons swallow
// synthetic clicks that land fine everywhere else. Arguments arrive as
// argv, never spliced into source, so an app named `"); doShellScript(...`
// is just a process that does not exist.
//
// Each script answers with one JSON line. running:false means the process
// is absent; stale:true means the recorded front window or element identity
// changed, which the tool turns into "call state again" rather than acting
// on whatever sits at the old ordinal path now.

// computerPrelude is shared by every script: find the process, or answer
// running:false. The helpers resolve a recorded child-ordinal path from
// the front window, derive opaque identity fingerprints, and read the front
// window's title for the answer. Fingerprints never expose AX values.
const computerPrelude = `
function proc(argv) {
  const se = Application("System Events");
  const app = argv[0];
  if (!se.applicationProcesses.name().includes(app)) return null;
  return {se: se, p: se.applicationProcesses.byName(app)};
}
function resolvePath(p, pathStr) {
  let cur = p.windows[0];
  if (pathStr !== "") {
    for (const part of pathStr.split(",")) cur = cur.uiElements[parseInt(part, 10)];
  }
  return cur;
}
function frontTitle(p) {
  try { const w = p.windows(); return w.length ? (w[0].title() || "") : "" } catch (_) { return "" }
}
function identityText(value) {
  if (value == null) return "";
  let text = "";
  try { text = typeof value === "string" ? value : JSON.stringify(value); }
  catch (_) { try { text = String(value); } catch (_) { return ""; } }
  return text;
}
function opaqueIdentity(parts) {
  const seeds = [0x811c9dc5, 0x9e3779b9, 0x85ebca6b, 0xc2b2ae35];
  const primes = [0x01000193, 0x27d4eb2d, 0x165667b1, 0x9e3779b1];
  const hs = seeds.slice();
  for (let p = 0; p < parts.length; p++) {
    const text = identityText(parts[p]);
    const framed = text.length + ":" + text + ";";
    for (let i = 0; i < framed.length; i++) {
      const code = framed.charCodeAt(i);
      for (let h = 0; h < hs.length; h++) {
        hs[h] ^= code & 0xff;
        hs[h] = Math.imul(hs[h], primes[h]);
        hs[h] ^= code >>> 8;
        hs[h] = Math.imul(hs[h], primes[h]);
      }
    }
  }
  return hs.map(h => ("00000000" + (h >>> 0).toString(16)).slice(-8)).join("");
}
function safeValue(spec, property) {
  try { const value = spec[property](); return value == null ? "" : value; }
  catch (_) { return ""; }
}
function safeAttribute(spec, name) {
  try { const value = spec.attributes.byName(name).value(); return value == null ? "" : value; }
  catch (_) { return ""; }
}
function genericDescription(value) {
  const text = identityText(value).toLowerCase();
  return text === "" || text === "button" || text === "checkbox" || text === "radio button" ||
    text === "text field" || text === "text area" || text === "group" || text === "image" || text === "text";
}
function elementIdentity(role, title, description, value, position, size) {
  const anonymous = identityText(title) === "" && identityText(value) === "" && genericDescription(description);
  return opaqueIdentity(["element-v1", role, title, description, value,
    anonymous ? position : "", anonymous ? size : ""]);
}
function liveElementIdentity(el) {
  return elementIdentity(safeValue(el, "role"), safeValue(el, "title"), safeValue(el, "description"),
    safeValue(el, "value"), safeValue(el, "position"), safeValue(el, "size"));
}
function windowIdentity(p) {
  let w;
  try { const windows = p.windows(); if (windows.length === 0) return ""; w = windows[0]; }
  catch (_) { return ""; }
  return opaqueIdentity(["window-v1", safeValue(w, "role"), safeAttribute(w, "AXSubrole"),
    safeValue(w, "title"), safeValue(w, "description"), safeValue(w, "position"), safeValue(w, "size"),
    safeAttribute(w, "AXIdentifier"), safeAttribute(w, "AXWindowNumber")]);
}
function activate(p) { p.frontmost = true; delay(0.3); }
function notRunning() { return JSON.stringify({running: false}); }
function ok(p, extra) {
  delay(0.4);
  const out = Object.assign({running: true, ok: true, window: frontTitle(p)}, extra || {});
  return JSON.stringify(out);
}
function stale(role, reason) { return JSON.stringify({running: true, stale: true, role: role, reason: reason || "element"}); }
function fail(msg, menus) {
  const out = {running: true, error: msg};
  if (menus) out.menus = menus;
  return JSON.stringify(out);
}
`

// computerStateScript walks the front window breadth-first with bulk
// per-container reads, under an element cap and a time budget, and keeps
// only elements worth acting on: actionable roles always, anything with a
// title or value, and descriptions beyond the generic furniture words.
// Hidden elements still count in walked, so the truncation lines are
// honest about what was read versus what was shown.
const computerStateScript = computerPrelude + `
function run(argv) {
  const got = proc(argv);
  if (!got) return notRunning();
  const p = got.p;
  const maxWalk = parseInt(argv[1], 10);
  const maxShow = parseInt(argv[2], 10);
  const budgetMs = parseInt(argv[3], 10);
  const out = {running: true, frontmost: p.frontmost(), windows: [], window_id: "", menus: [], els: [], walked: 0, timedOut: false};
  try { out.menus = p.menuBars[0].menuBarItems.name().filter(n => n).slice(0, 24); } catch (_) {}
  const wins = p.windows();
  for (let i = 0; i < Math.min(wins.length, 8); i++) {
    let t = ""; try { t = wins[i].title() || "" } catch (_) {}
    out.windows.push(t);
  }
  if (wins.length === 0) return JSON.stringify(out);
  out.window_id = windowIdentity(p);
  const always = {AXButton:1, AXTextField:1, AXTextArea:1, AXCheckBox:1, AXRadioButton:1,
    AXPopUpButton:1, AXComboBox:1, AXMenuButton:1, AXLink:1, AXSlider:1, AXIncrementor:1,
    AXMenuItem:1, AXSegmentedControl:1, AXDisclosureTriangle:1, AXSheet:1, AXColorWell:1};
  const generic = {"group":1, "split group":1, "scroll bar":1, "ruler":1, "ruler marker":1,
    "scroll area":1, "toolbar":1, "drawer":1, "splitter":1, "grow area":1,
    "image":1, "text":1, "web area":1, "tab group":1, "opaque group":1};
  const noise = {AXRulerMarker:1, AXScrollBar:1, AXValueIndicator:1, AXGrowArea:1, AXSplitter:1};
  const cap = s => { s = String(s); return s.length > 80 ? s.slice(0, 80) + "…" : s };
  const start = Date.now();
  const queue = [{spec: wins[0], path: []}];
  while (queue.length > 0 && out.walked < maxWalk && out.els.length < maxShow) {
    if (Date.now() - start > budgetMs) { out.timedOut = true; break }
    const c = queue.shift();
    let kids, roles, titles, descs, vals, poss, sizes;
    try {
      kids = c.spec.uiElements();
      if (kids.length === 0) continue;
      roles = c.spec.uiElements.role();
      titles = c.spec.uiElements.title();
      descs = c.spec.uiElements.description();
      vals = c.spec.uiElements.value();
      poss = c.spec.uiElements.position();
      sizes = c.spec.uiElements.size();
    } catch (_) { continue }
    for (let k = 0; k < kids.length; k++) {
      if (out.walked >= maxWalk || out.els.length >= maxShow) break;
      out.walked++;
      const path = c.path.concat([k]);
      queue.push({spec: kids[k], path: path});
      const r = roles[k] || "";
      const t = titles[k] == null ? "" : cap(titles[k]);
      const d = descs[k] == null ? "" : cap(descs[k]);
      const v = vals[k] == null ? "" : cap(vals[k]);
      if (noise[r]) continue;
      if (!(always[r] || t !== "" || v !== "" || (d !== "" && !generic[d]))) continue;
      out.els.push({path: path, r: r, t: t, d: d, v: v, p: poss[k] || null, s: sizes[k] || null,
        f: elementIdentity(r, titles[k], descs[k], vals[k], poss[k] || null, sizes[k] || null)});
    }
  }
  return JSON.stringify(out);
}
`

// computerAppsScript lists processes with a user interface. One bulk read
// for names and frontmost; window counts are one event per process, which
// a screenful of apps affords.
const computerAppsScript = `
function run() {
  const se = Application("System Events");
  const procs = se.applicationProcesses.whose({backgroundOnly: false});
  const names = procs.name();
  const fronts = procs.frontmost();
  const out = [];
  for (let i = 0; i < names.length; i++) {
    let wins = 0;
    try { wins = procs[i].windows().length } catch (_) {}
    out.push({name: names[i], frontmost: fronts[i] === true, windows: wins});
  }
  return JSON.stringify(out);
}
`

// computerClickScript clicks a recorded element only when both the recorded
// front window and opaque element fingerprint still match. Role alone is not
// enough: Save and Delete can occupy the same ordinal path.
const computerClickScript = computerPrelude + `
function run(argv) {
  const got = proc(argv);
  if (!got) return notRunning();
  const p = got.p;
  activate(p);
  if (windowIdentity(p) !== argv[3]) return stale("window", "window");
  let el, role = "";
  try { el = resolvePath(p, argv[1]); role = el.role() || "" } catch (_) { return stale("nothing"); }
  if (role !== argv[2]) return stale(role);
  if (liveElementIdentity(el) !== argv[4]) return stale(role, "element");
  if (windowIdentity(p) !== argv[3]) return stale("window", "window");
  try { el.click(); } catch (e) { return fail("the click failed: " + e.message); }
  return ok(p);
}
`

// computerClickAtScript posts a click at screen coordinates through the
// process. It exists for the elements the tree cannot name — verified
// against Calculator's unlabeled SwiftUI buttons.
const computerClickAtScript = computerPrelude + `
function run(argv) {
  const got = proc(argv);
  if (!got) return notRunning();
  const p = got.p;
  activate(p);
  try { p.click({at: [parseInt(argv[1], 10), parseInt(argv[2], 10)]}); }
  catch (e) { return fail("the click failed: " + e.message); }
  return ok(p);
}
`

// computerTypeScript sends real keystrokes, which go to the frontmost
// process — activation is not a courtesy here, it is what aims the keys.
const computerTypeScript = computerPrelude + `
function run(argv) {
  const got = proc(argv);
  if (!got) return notRunning();
  const p = got.p;
  activate(p);
  try { got.se.keystroke(argv[1]); } catch (e) { return fail("typing failed: " + e.message); }
  return ok(p);
}
`

// computerKeyScript presses one key or combo: a character through
// keystroke, a named key through its hardware code, either with modifier
// phrases. The spec arrives as JSON composed by the Go side.
const computerKeyScript = computerPrelude + `
function run(argv) {
  const got = proc(argv);
  if (!got) return notRunning();
  const p = got.p;
  activate(p);
  const spec = JSON.parse(argv[1]);
  try {
    if (spec.char) {
      if (spec.mods.length > 0) got.se.keystroke(spec.char, {using: spec.mods});
      else got.se.keystroke(spec.char);
    } else {
      if (spec.mods.length > 0) got.se.keyCode(spec.code, {using: spec.mods});
      else got.se.keyCode(spec.code);
    }
  } catch (e) { return fail("the key press failed: " + e.message); }
  return ok(p);
}
`

// computerSetScript writes an element's accessibility value directly and
// reads it back, so the answer reports what the field now holds rather
// than what was asked for.
const computerSetScript = computerPrelude + `
function run(argv) {
  const got = proc(argv);
  if (!got) return notRunning();
  const p = got.p;
  activate(p);
  if (windowIdentity(p) !== argv[4]) return stale("window", "window");
  let el, role = "";
  try { el = resolvePath(p, argv[1]); role = el.role() || "" } catch (_) { return stale("nothing"); }
  if (role !== argv[2]) return stale(role);
  if (liveElementIdentity(el) !== argv[5]) return stale(role, "element");
  if (windowIdentity(p) !== argv[4]) return stale("window", "window");
  try { el.value = argv[3]; } catch (e) { return fail("setting the value failed: " + e.message); }
  let now = "";
  try { now = String(el.value()).slice(0, 120) } catch (_) {}
  return ok(p, {value: now});
}
`

// computerMenuScript walks a menu path and clicks the last item. A miss at
// any level answers with what that level actually holds, because the
// difference between "no such item" and "here is what exists" is the
// difference between abandoning the menu and correcting the name.
const computerMenuScript = computerPrelude + `
function run(argv) {
  const got = proc(argv);
  if (!got) return notRunning();
  const p = got.p;
  activate(p);
  const items = JSON.parse(argv[1]);
  const names = coll => { try { return coll.name().filter(n => n).slice(0, 30).join(", ") } catch (_) { return "" } };
  const bar = p.menuBars[0];
  let menu;
  try { menu = bar.menuBarItems.byName(items[0]).menus[0]; menu.menuItems.name(); }
  catch (_) { return fail("no menu named " + items[0], names(bar.menuBarItems)); }
  for (let i = 1; i < items.length; i++) {
    let item;
    try { item = menu.menuItems.byName(items[i]); item.name(); }
    catch (_) { return fail("no item named " + items[i] + " under " + items[i-1], names(menu.menuItems)); }
    if (i === items.length - 1) {
      try { item.click(); } catch (e) { return fail("the menu click failed: " + e.message); }
    } else {
      try { menu = item.menus[0]; menu.menuItems.name(); }
      catch (_) { return fail(items[i] + " has no submenu", names(menu.menuItems)); }
    }
  }
  return ok(p);
}
`
