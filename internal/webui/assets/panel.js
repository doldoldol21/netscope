/* netscope menu-bar popover panel logic. Same-origin /api is reverse-proxied to
   the daemon by the Wails asset server. */
"use strict";
const $ = (id) => document.getElementById(id);

function fmtParts(n) {
  n = Number(n) || 0;
  const u = ["B", "KB", "MB", "GB", "TB"]; let i = 0;
  while (n >= 1024 && i < u.length - 1) { n /= 1024; i++; }
  return { num: String(i === 0 ? n : n.toFixed(n < 10 ? 1 : 0)), unit: u[i] };
}
function fmtBytes(n) { const p = fmtParts(n); return p.num + " " + p.unit; }
// numUnitHTML renders a figure with its unit de-emphasized (Field Notebook).
const numUnitHTML = (n, rate) => {
  const p = fmtParts(n);
  return `<span class="n">${p.num}</span><span class="u">${p.unit}${rate ? "/s" : ""}</span>`;
};
const esc = (s) => String(s).replace(/[&<>"]/g, (c) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;" }[c]));

// Theme colors for the canvas, cached — getComputedStyle is a forced style
// resolution and drawSpark runs every second. Invalidated on theme change
// (applyTheme) and on OS light/dark flips.
let sparkColorCache = null;
function sparkColors() {
  if (sparkColorCache) return sparkColorCache;
  const css = getComputedStyle(document.body);
  sparkColorCache = {
    rx: css.getPropertyValue("--rx").trim(),
    tx: css.getPropertyValue("--tx").trim(),
    rxFill: css.getPropertyValue("--rx-fill").trim(),
    txFill: css.getPropertyValue("--tx-fill").trim(),
  };
  return sparkColorCache;
}
window.matchMedia("(prefers-color-scheme: dark)").addEventListener("change", () => { sparkColorCache = null; });

// ---- sparkline ----
const spark = $("spark"), sx = spark.getContext("2d"), hist = [], MAXP = 80;
// The popover is fixed-size, so cache the canvas rect instead of forcing
// layout with getBoundingClientRect on every 1 Hz draw (right after the DOM
// writes in render(), which made it a guaranteed reflow).
let sparkRect = null;
window.addEventListener("resize", () => { sparkRect = null; });
function drawSpark() {
  const dpr = window.devicePixelRatio || 1;
  const r = sparkRect && sparkRect.width >= 1 ? sparkRect : (sparkRect = spark.getBoundingClientRect());
  // While the popover is hidden the canvas has no layout size; skip so we don't
  // blank it to nothing (which made the graph "disappear" until the next tick).
  if (r.width < 1 || r.height < 1) return;
  // Assigning width/height clears and reallocates the backing bitmap, so only
  // do it when the layout size actually changed (this redraws every second).
  const W = Math.round(r.width * dpr), H = Math.round(r.height * dpr);
  if (spark.width !== W || spark.height !== H) { spark.width = W; spark.height = H; }
  sx.setTransform(dpr, 0, 0, dpr, 0, 0);
  const w = r.width, h = r.height; sx.clearRect(0, 0, w, h);
  if (hist.length < 2) return;
  const c = sparkColors();
  const max = Math.max(1, ...hist.map((p) => Math.max(p.rx, p.tx)));
  // Step traces with a flat soft fill (Field Notebook instrument-chart look).
  const line = (key, color, fill) => {
    const px = (i) => w * (i + (MAXP - hist.length)) / (MAXP - 1);
    const py = (p) => h - (p[key] / max) * (h - 6) - 3;
    const trace = () => {
      sx.beginPath();
      hist.forEach((p, i) => {
        if (!i) { sx.moveTo(px(i), py(p)); return; }
        sx.lineTo(px(i), py(hist[i - 1]));
        sx.lineTo(px(i), py(p));
      });
    };
    trace();
    sx.lineTo(w, h); sx.lineTo(px(0), h); sx.closePath();
    sx.fillStyle = fill; sx.fill();
    trace();
    sx.strokeStyle = color; sx.lineWidth = 1.2; sx.lineJoin = "round"; sx.stroke();
  };
  line("rx", c.rx, c.rxFill);
  line("tx", c.tx, c.txFill);
}

const setText = (el, s) => { if (el && el.textContent !== s) el.textContent = s; };
// Guarded attribute/HTML writes: rewriting an identical value still replaces
// nodes, which reads as a flicker on the popover's 1 Hz refresh.
const setAttr = (el, name, v) => { if (el && el.getAttribute(name) !== v) el.setAttribute(name, v); };
const setHTML = (el, html) => { if (el && el.innerHTML !== html) el.innerHTML = html; };
let appsSig = "";
function render(s) {
  applyPausedFromSnapshot(!!s.paused);
  if (!capPaused) $("dot").classList.add("live");
  if (s.interface) {
    const changed = s.interface !== ifaceCur;
    ifaceCur = s.interface;
    // Pull the friendly-name list if we don't know this interface yet (boot or a
    // just-switched interface), so the meta line shows "Wi-Fi" not "en0".
    if (changed && !ifaceOpts.some((o) => o.name === ifaceCur)) refreshIface();
    updateMetaText();
  }
  const rxEl = $("rx"), txEl = $("tx");
  const rxHTML = numUnitHTML(s.rxPerSec, true), txHTML = numUnitHTML(s.txPerSec, true);
  if (rxEl.innerHTML !== rxHTML) rxEl.innerHTML = rxHTML;
  if (txEl.innerHTML !== txHTML) txEl.innerHTML = txHTML;
  if (s.activeApps != null) setText($("active"), t("pop.active", { n: s.activeApps }));

  hist.push({ rx: Number(s.rxPerSec) || 0, tx: Number(s.txPerSec) || 0 });
  while (hist.length > MAXP) hist.shift();
  drawSpark();

  const apps = (s.apps || []).slice(0, 6);
  const el = $("apps");
  if (!apps.length) { if (appsSig !== "empty") { el.innerHTML = `<li class="empty">${t("state.waiting")}</li>`; appsSig = "empty"; } return; }
  const max = Math.max(1, ...apps.map((a) => Number(a.rxBytes) + Number(a.txBytes)));
  // Signature on which apps are listed, order-independent: a re-rank moves the
  // existing <li>s instead of rebuilding the list (rebuilding blinked the rows
  // and dropped hover every time two apps swapped places).
  const names = apps.map((a) => a.name || "unknown");
  const sig = [...names].sort().join("|");
  if (sig === appsSig && el.children.length === apps.length) {
    let prev = null;
    const byKey = new Map();
    for (const li of el.children) byKey.set(li.dataset.k, li);
    for (let i = 0; i < apps.length; i++) {
      const li = byKey.get(names[i]);
      if (!li) { appsSig = ""; break; } // unexpected: fall through to rebuild
      const target = prev ? prev.nextSibling : el.firstChild;
      if (li !== target) el.insertBefore(li, target);
      prev = li;
      const a = apps[i];
      setText(li.querySelector(".rk"), String(i + 1));
      setText(li.querySelector(".by"), fmtBytes(Number(a.rxBytes) + Number(a.txBytes)));
      const seg = li.querySelector(".ub").children;
      const rxW = (100 * Number(a.rxBytes) / max).toFixed(1) + "%";
      const txW = (100 * Number(a.txBytes) / max).toFixed(1) + "%";
      if (seg[0].style.width !== rxW) seg[0].style.width = rxW;
      if (seg[1].style.width !== txW) seg[1].style.width = txW;
    }
    if (appsSig) return;
  }
  const html = apps.map((a, i) => {
    const name = a.name || "unknown";
    const total = Number(a.rxBytes) + Number(a.txBytes);
    const rxW = (100 * Number(a.rxBytes) / max).toFixed(1);
    const txW = (100 * Number(a.txBytes) / max).toFixed(1);
    return `<li data-k="${esc(name)}"><span class="rk">${i + 1}</span>` +
      `<span class="mid"><span class="nm" title="${esc(a.path || name)}">${esc(name)}</span>` +
      `<span class="ub"><i class="rx" style="width:${rxW}%"></i><i class="tx" style="width:${txW}%"></i></span></span>` +
      `<span class="by">${fmtBytes(total)}</span></li>`;
  }).join("");
  el.innerHTML = html;
  appsSig = sig;
}

function setDisconnected() { $("dot").classList.remove("live"); setText($("meta"), t("status.reconnecting")); }

// ---- today's total (polled; the live snapshot only carries per-second rates) ----
async function loadToday() {
  try {
    const r = await fetch("/api/summary?range=today");
    if (!r.ok) return;
    const s = await r.json();
    const rx = Number(s.totalRx) || 0, tx = Number(s.totalTx) || 0;
    setHTML($("t-total"), numUnitHTML(rx + tx));
    setHTML($("t-split"),
      `<span class="rx">↓ ${fmtBytes(rx)}</span>` +
      `<span class="tx">↑ ${fmtBytes(tx)}</span>`);
  } catch (_) { /* daemon not ready */ }
}

// Live updates run only while the popover is visible. Go calls window.nsLive
// on show/hide so a hidden popover doesn't keep the daemon streaming a snapshot
// every second (wasted CPU/battery). wantLive gates reconnects so an error
// while paused doesn't silently re-open the stream.
let es = null, todayTimer = null, wantLive = false;
function connect() {
  if (es) return;
  es = new EventSource("/api/live");
  es.onmessage = (e) => { try { render(JSON.parse(e.data)); } catch (_) {} };
  es.onerror = () => {
    setDisconnected();
    if (es) { es.close(); es = null; }
    if (wantLive) setTimeout(() => { if (wantLive) connect(); }, 2000);
  };
}
function disconnect() { if (es) { es.close(); es = null; } }

// Seed the sparkline from the daemon's recent per-second history so the graph is
// continuous on (re)open instead of starting blank — the popover only collects
// live points while visible, so without this it has gaps for the time it was
// closed. Live messages append after.
async function seedSpark() {
  try {
    const r = await fetch("/api/ratehist");
    if (!r.ok) return;
    const pts = await r.json();
    if (!Array.isArray(pts) || !pts.length) return;
    hist.length = 0;
    for (const p of pts.slice(-MAXP)) {
      hist.push({ rx: Number(p.rxPerSec) || 0, tx: Number(p.txPerSec) || 0 });
    }
    drawSpark();
  } catch (_) { /* daemon not ready */ }
}
// nsLive(true|false): start/stop the live stream + today's-total polling.
window.nsLive = (on) => {
  wantLive = !!on;
  if (on) {
    seedSpark(); // continuous history on (re)open, then live appends
    connect();
    loadToday();
    if (!todayTimer) todayTimer = setInterval(loadToday, 15000);
  } else {
    disconnect();
    if (todayTimer) { clearInterval(todayTimer); todayTimer = null; }
  }
};

// ---- actions (Wails runtime) ----
const rt = () => window.runtime || {};
$("dash").onclick = () => {
  // Go promotes the window into a standalone dashboard window and navigates.
  const r = rt();
  if (r.EventsEmit) r.EventsEmit("netscope:opendash");
};
$("quit").onclick = () => { const r = rt(); if (r.Quit) r.Quit(); };

// ---- usage-alerts settings ----
const GB = 1024 * 1024 * 1024;
const gbToBytes = (v) => { const n = parseFloat(v); return n > 0 ? Math.round(n * GB) : 0; };
const bytesToGb = (n) => { n = Number(n) || 0; return n > 0 ? +(n / GB).toFixed(n >= GB ? 1 : 2) : 0; };

function openSettings() {
  const r = rt();
  if (r.EventsEmit) {
    r.EventsEmit("netscope:getalerts");  // Go replies on "netscope:alerts"
    r.EventsEmit("netscope:getupdate");  // Go replies on "netscope:update"
    r.EventsEmit("netscope:getmenubar"); // Go replies on "netscope:menubar"
    r.EventsEmit("netscope:gettheme");   // Go replies on "netscope:theme"
  }
  $("settings").classList.add("show");
}

// ---- theme (shared with the dashboard; persisted server-side) ----
function applyTheme(mode) {
  const m = ["auto", "light", "dark"].includes(mode) ? mode : "auto";
  if (m === "auto") document.documentElement.removeAttribute("data-theme");
  else document.documentElement.setAttribute("data-theme", m);
  sparkColorCache = null; // canvas colors changed with the theme
  const sel = $("set-theme");
  if (sel) sel.value = m;
}
$("set-theme").onchange = (e) => {
  const v = e.currentTarget.value;
  applyTheme(v);
  const r = rt();
  if (r.EventsEmit) r.EventsEmit("netscope:settheme", v);
};

// ---- pause/resume capture (daemon closes the pcap handle while paused) ----
let capPaused = false;
let pausePendingUntil = 0; // ignore stale snapshots right after a manual toggle
// A snapshot generated just before our POST landed still reports the old state;
// during the pending window keep our optimistic value until snapshots agree.
function applyPausedFromSnapshot(p) {
  if (Date.now() < pausePendingUntil && p !== capPaused) return;
  pausePendingUntil = 0;
  reflectPaused(p);
}
function reflectPaused(p) {
  capPaused = p;
  const b = $("pause-btn");
  if (b) {
    setText(b, p ? "▶" : "⏸");
    setAttr(b, "data-tip", p ? t("tip.resume") : t("tip.pause"));
    // aria-label overrides content — keep it in sync or AT hears the old action.
    setAttr(b, "aria-label", p ? t("tip.resume") : t("tip.pause"));
  }
  $("dot").classList.toggle("paused", p);
  if (p) $("dot").classList.remove("live");
  updateMetaText();
}
async function togglePause() {
  const next = !capPaused;
  pausePendingUntil = Date.now() + 3000; // hold our choice until snapshots catch up
  reflectPaused(next); // optimistic; snapshots confirm
  try {
    await fetch("/api/capture", {
      method: "POST", headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ paused: next }),
    });
  } catch (_) { /* ignore; next snapshot reconciles */ }
}
$("pause-btn").onclick = togglePause;

// ---- capture interface picker (top-right chip → dropdown, daemon API) ----
let ifaceCur = "", ifaceSel = "", ifaceOpts = [];
// Map a raw BSD name (en0) to its macOS-friendly name (Wi-Fi) using the
// interface list we already fetch for the picker; falls back to the raw name.
function friendlyIface(name) {
  const o = ifaceOpts.find((x) => x.name === name);
  return o && o.friendly ? o.friendly : name;
}
// Runs on every snapshot — guarded so the interface chip isn't rewritten (and
// repainted) once a second with the same text.
function updateMetaText() {
  if (capPaused) { setText($("meta"), t("status.paused")); return; }
  const cur = ifaceCur ? friendlyIface(ifaceCur) : t("meta.live");
  setText($("meta"), ifaceSel ? cur : t("meta.auto", { name: cur }));
}
async function refreshIface() {
  try {
    const r = await fetch("/api/interfaces");
    if (!r.ok) return;
    const d = await r.json();
    ifaceSel = d.selected || "";
    ifaceOpts = d.options || [];
    if (d.current) ifaceCur = d.current;
    updateMetaText();
    buildIfaceMenu();
  } catch (_) { /* daemon not ready */ }
}
function buildIfaceMenu() {
  const item = (name, label, on) =>
    `<button class="ifm-item${on ? " on" : ""}" data-iface="${esc(name)}"><span class="ifm-lbl">${esc(label)}</span>${on ? `<span class="ifm-mark">✓</span>` : ""}</button>`;
  let html = `<div class="ifm-head">${t("tip.captureIface")}</div>`;
  html += item("", t("iface.auto"), ifaceSel === "");
  ifaceOpts.forEach((o) => { html += item(o.name, o.display, ifaceSel === o.name); });
  const m = $("iface-menu");
  m.innerHTML = html;
  m.querySelectorAll("[data-iface]").forEach((b) => {
    b.onclick = async () => {
      m.hidden = true;
      try {
        await fetch("/api/interfaces", {
          method: "POST", headers: { "Content-Type": "application/json" },
          body: JSON.stringify({ name: b.dataset.iface }),
        });
      } catch (_) { /* ignore */ }
      setTimeout(refreshIface, 800); // let capture re-open, then refresh
    };
  });
}
$("meta").onclick = (e) => {
  e.stopPropagation();
  const m = $("iface-menu");
  m.hidden = !m.hidden;
  if (!m.hidden) refreshIface();
};
document.addEventListener("click", (e) => {
  const m = $("iface-menu");
  if (!m.hidden && !m.contains(e.target) && e.target !== $("meta")) m.hidden = true;
});
document.addEventListener("keydown", (e) => {
  if (e.key !== "Escape") return;
  const m = $("iface-menu");
  if (!m.hidden) { m.hidden = true; return; }
  $("settings").classList.remove("show");
});

// ---- menu-bar readout style ----
function fillMenuBar(cfg) {
  cfg = cfg || {};
  const sel = $("set-menubar");
  sel.innerHTML = (cfg.options || [])
    .map((o) => `<option value="${esc(o.id)}">${esc(o.label)}</option>`).join("");
  if (cfg.current) sel.value = cfg.current;
  $("set-menubar-color").checked = !!cfg.color;
  $("set-menubar-anim").checked = cfg.animate !== false; // default on
}
$("set-menubar").onchange = (e) => {
  const r = rt();
  if (r.EventsEmit) r.EventsEmit("netscope:setmenubar", e.currentTarget.value);
};
$("set-menubar-color").onchange = (e) => {
  const r = rt();
  if (r.EventsEmit) r.EventsEmit("netscope:setmenubarcolor", e.currentTarget.checked);
};
$("set-menubar-anim").onchange = (e) => {
  const r = rt();
  if (r.EventsEmit) r.EventsEmit("netscope:setmenubaranim", e.currentTarget.checked);
};
function fillSettings(cfg) {
  cfg = cfg || {};
  $("set-daily").value = bytesToGb(cfg.dailyTotalBytes) || "";
  $("set-app").value = bytesToGb(cfg.perAppBytes) || "";
  $("set-up-daily").value = bytesToGb(cfg.dailyUploadBytes) || "";
  $("set-up-app").value = bytesToGb(cfg.perAppUploadBytes) || "";
}
// Alerts save instantly on edit (on blur / Enter) — no separate Save button, so
// every setting in this panel applies the moment it changes.
function saveAlerts() {
  const r = rt();
  if (r.EventsEmit) r.EventsEmit("netscope:setalerts", {
    dailyTotalBytes: gbToBytes($("set-daily").value),
    perAppBytes: gbToBytes($("set-app").value),
    dailyUploadBytes: gbToBytes($("set-up-daily").value),
    perAppUploadBytes: gbToBytes($("set-up-app").value),
  });
}
["set-daily", "set-app", "set-up-daily", "set-up-app"].forEach((id) => { $(id).onchange = saveAlerts; });
$("alerts-btn").onclick = openSettings;
$("set-close").onclick = () => { $("settings").classList.remove("show"); };

// ---- software updates ----
function renderUpdate(st) {
  st = st || {};
  $("set-autocheck").checked = st.autoCheck !== false;
  const banner = $("updbanner"), now = $("upd-now"), status = $("upd-status");
  now.textContent = t("upd.now"); now.disabled = false;
  if (st.updateAvailable && st.latest) {
    status.textContent = t("upd.availableV", { v: st.latest });
    status.classList.add("avail");
    banner.querySelector(".ub-txt").textContent = t("pop.updateV", { v: st.latest });
    banner.hidden = false; now.hidden = false;
  } else {
    status.textContent = st.current ? t("upd.uptodateV", { v: st.current }) : t("upd.uptodate");
    status.classList.remove("avail");
    banner.hidden = true; now.hidden = true;
  }
}
function startUpdate(btn) {
  const r = rt();
  if (!r.EventsEmit) return;
  if (btn) { btn.textContent = t("upd.downloading"); btn.disabled = true; }
  r.EventsEmit("netscope:doupdate"); // app downloads, swaps the bundle, relaunches
}
$("upd-check").onclick = () => {
  const r = rt();
  $("upd-status").textContent = t("upd.checking");
  $("upd-status").classList.remove("avail");
  if (r.EventsEmit) r.EventsEmit("netscope:checkupdate"); // Go replies on "netscope:update"
};
$("upd-now").onclick = (e) => startUpdate(e.currentTarget);
$("updbanner").onclick = () => { openSettings(); startUpdate($("upd-now")); };
$("set-autocheck").onchange = (e) => {
  const r = rt();
  if (r.EventsEmit) r.EventsEmit("netscope:setautocheck", e.currentTarget.checked);
};

window.addEventListener("DOMContentLoaded", () => {
  if (window.runtime && window.runtime.EventsOn) {
    window.runtime.EventsOn("netscope:show", () => { /* already on panel */ });
    window.runtime.EventsOn("netscope:alerts", (cfg) => fillSettings(cfg));
    window.runtime.EventsOn("netscope:menubar", (cfg) => fillMenuBar(cfg));
    window.runtime.EventsOn("netscope:theme", (t) => applyTheme(t));
    window.runtime.EventsOn("netscope:update", (st) => renderUpdate(st));
    window.runtime.EventsOn("netscope:updateerror", () => {
      $("upd-status").textContent = t("upd.failed");
      $("upd-status").classList.remove("avail");
      const now = $("upd-now");
      now.textContent = t("upd.now"); now.disabled = false;
    });
    // Ask for cached update status + theme so the popover styles itself on launch.
    if (window.runtime.EventsEmit) {
      window.runtime.EventsEmit("netscope:getupdate");
      window.runtime.EventsEmit("netscope:gettheme");
    }
  }
});

// Start live by default; Go pauses it via nsLive(false) once the hidden
// popover's DOM is ready, and toggles it on show/hide thereafter.
window.nsLive(true);
refreshIface(); // populate the capture-interface chip + menu
drawSpark();
window.addEventListener("resize", drawSpark);
