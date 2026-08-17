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
// is absent; stale:true means the recorded element path no longer holds an
// element of the recorded role, which the tool turns into "call state
// again" rather than acting on whatever sits there now.

// computerPrelude is shared by every script: find the process, or answer
// running:false. The helpers resolve a recorded child-ordinal path from
// the front window and read the front window's title for the answer.
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
function activate(p) { p.frontmost = true; delay(0.3); }
function notRunning() { return JSON.stringify({running: false}); }
function ok(p, extra) {
  delay(0.4);
  const out = Object.assign({running: true, ok: true, window: frontTitle(p)}, extra || {});
  return JSON.stringify(out);
}
function stale(role) { return JSON.stringify({running: true, stale: true, role: role}); }
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
  const out = {running: true, frontmost: p.frontmost(), windows: [], menus: [], els: [], walked: 0, timedOut: false};
  try { out.menus = p.menuBars[0].menuBarItems.name().filter(n => n).slice(0, 24); } catch (_) {}
  const wins = p.windows();
  for (let i = 0; i < Math.min(wins.length, 8); i++) {
    let t = ""; try { t = wins[i].title() || "" } catch (_) {}
    out.windows.push(t);
  }
  if (wins.length === 0) return JSON.stringify(out);
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
      out.els.push({path: path, r: r, t: t, d: d, v: v, p: poss[k] || null, s: sizes[k] || null});
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

// computerClickScript clicks a recorded element. The role check is the
// staleness gate: element .click() lands on whatever the path resolves to,
// so a changed window must answer stale, never click.
const computerClickScript = computerPrelude + `
function run(argv) {
  const got = proc(argv);
  if (!got) return notRunning();
  const p = got.p;
  activate(p);
  let el, role = "";
  try { el = resolvePath(p, argv[1]); role = el.role() || "" } catch (_) { return stale("nothing"); }
  if (role !== argv[2]) return stale(role);
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
  let el, role = "";
  try { el = resolvePath(p, argv[1]); role = el.role() || "" } catch (_) { return stale("nothing"); }
  if (role !== argv[2]) return stale(role);
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
