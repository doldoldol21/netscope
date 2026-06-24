/* netscope menu-bar popover panel logic. Same-origin /api is reverse-proxied to
   the daemon by the Wails asset server. */
"use strict";
const $ = (id) => document.getElementById(id);

function fmtRate(n) {
  n = Number(n) || 0;
  const u = ["B", "KB", "MB", "GB", "TB"]; let i = 0;
  while (n >= 1024 && i < u.length - 1) { n /= 1024; i++; }
  return (i === 0 ? n : n.toFixed(n < 10 ? 1 : 0)) + " " + u[i] + "/s";
}
function fmtBytes(n) {
  n = Number(n) || 0;
  const u = ["B", "KB", "MB", "GB", "TB"]; let i = 0;
  while (n >= 1024 && i < u.length - 1) { n /= 1024; i++; }
  return (i === 0 ? n : n.toFixed(n < 10 ? 1 : 0)) + " " + u[i];
}
const esc = (s) => String(s).replace(/[&<>"]/g, (c) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;" }[c]));
function hue(s) { let h = 0; for (let i = 0; i < s.length; i++) h = (h * 31 + s.charCodeAt(i)) % 360; return h; }

// ---- sparkline ----
const spark = $("spark"), sx = spark.getContext("2d"), hist = [], MAXP = 80;
function drawSpark() {
  const dpr = window.devicePixelRatio || 1, r = spark.getBoundingClientRect();
  // While the popover is hidden the canvas has no layout size; skip so we don't
  // blank it to nothing (which made the graph "disappear" until the next tick).
  if (r.width < 1 || r.height < 1) return;
  spark.width = r.width * dpr; spark.height = r.height * dpr; sx.setTransform(dpr, 0, 0, dpr, 0, 0);
  const w = r.width, h = r.height; sx.clearRect(0, 0, w, h);
  if (hist.length < 2) return;
  const max = Math.max(1, ...hist.map((p) => Math.max(p.rx, p.tx)));
  const line = (key, color) => {
    sx.beginPath();
    hist.forEach((p, i) => { const x = w * (i + (MAXP - hist.length)) / (MAXP - 1), y = h - (p[key] / max) * (h - 3) - 2; i ? sx.lineTo(x, y) : sx.moveTo(x, y); });
    sx.lineTo(w, h); sx.lineTo(w * (MAXP - hist.length) / (MAXP - 1), h); sx.closePath();
    const g = sx.createLinearGradient(0, 0, 0, h); g.addColorStop(0, color + "44"); g.addColorStop(1, "transparent");
    sx.fillStyle = g; sx.fill();
    sx.beginPath();
    hist.forEach((p, i) => { const x = w * (i + (MAXP - hist.length)) / (MAXP - 1), y = h - (p[key] / max) * (h - 3) - 2; i ? sx.lineTo(x, y) : sx.moveTo(x, y); });
    sx.strokeStyle = color; sx.lineWidth = 1.5; sx.lineJoin = "round"; sx.stroke();
  };
  line("rx", "#3fb950"); line("tx", "#f0883e");
}

const setText = (el, s) => { if (el && el.textContent !== s) el.textContent = s; };
let appsSig = "";
function render(s) {
  applyPausedFromSnapshot(!!s.paused);
  if (!capPaused) $("dot").classList.add("live");
  if (s.interface) { ifaceCur = s.interface; updateMetaText(); }
  setText($("rx"), fmtRate(s.rxPerSec));
  setText($("tx"), fmtRate(s.txPerSec));
  if (s.activeApps != null) setText($("active"), s.activeApps + " active");

  hist.push({ rx: Number(s.rxPerSec) || 0, tx: Number(s.txPerSec) || 0 });
  while (hist.length > MAXP) hist.shift();
  drawSpark();

  const apps = (s.apps || []).slice(0, 6);
  const el = $("apps");
  if (!apps.length) { if (appsSig !== "empty") { el.innerHTML = '<li class="empty">waiting for traffic…</li>'; appsSig = "empty"; } return; }
  const html = apps.map((a) => {
    const name = a.name || "unknown";
    const total = Number(a.rxBytes) + Number(a.txBytes);
    return `<li><span class="sw" style="background:hsl(${hue(name)} 55% 58%)"></span>` +
      `<span class="nm" title="${esc(a.path || name)}">${esc(name)}</span>` +
      `<span class="by">${fmtBytes(total)}</span></li>`;
  }).join("");
  // Skip the rebuild when nothing changed — the popover list re-`innerHTML`d
  // every second, flickering the rows.
  if (html !== appsSig) { el.innerHTML = html; appsSig = html; }
}

function setDisconnected() { $("dot").classList.remove("live"); $("meta").textContent = "reconnecting…"; }

// ---- today's total (polled; the live snapshot only carries per-second rates) ----
async function loadToday() {
  try {
    const r = await fetch("/api/summary?range=today");
    if (!r.ok) return;
    const s = await r.json();
    const rx = Number(s.totalRx) || 0, tx = Number(s.totalTx) || 0;
    $("t-total").textContent = fmtBytes(rx + tx);
    $("t-split").innerHTML =
      `<span style="color:var(--rx)">▼ ${fmtBytes(rx)}</span> ` +
      `<span style="color:var(--tx)">▲ ${fmtBytes(tx)}</span>`;
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
  if (b) { b.textContent = p ? "▶" : "⏸"; b.title = p ? "Resume capture" : "Pause capture"; }
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
function updateMetaText() {
  if (capPaused) { $("meta").textContent = "paused"; return; }
  const cur = ifaceCur || "live";
  $("meta").textContent = ifaceSel ? cur : ("auto · " + cur);
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
    `<button class="ifm-item${on ? " on" : ""}" data-iface="${esc(name)}">${esc(label)}${on ? " ✓" : ""}</button>`;
  let html = item("", "Automatic", ifaceSel === "");
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
  now.textContent = "Update & Restart"; now.disabled = false;
  if (st.updateAvailable && st.latest) {
    status.textContent = `${st.latest} available`;
    status.classList.add("avail");
    banner.querySelector(".ub-txt").textContent = `Update ${st.latest} available`;
    banner.hidden = false; now.hidden = false;
  } else {
    status.textContent = st.current ? `Up to date · ${st.current}` : "Up to date";
    status.classList.remove("avail");
    banner.hidden = true; now.hidden = true;
  }
}
function startUpdate(btn) {
  const r = rt();
  if (!r.EventsEmit) return;
  if (btn) { btn.textContent = "Downloading…"; btn.disabled = true; }
  r.EventsEmit("netscope:doupdate"); // app downloads, swaps the bundle, relaunches
}
$("upd-check").onclick = () => {
  const r = rt();
  $("upd-status").textContent = "Checking…";
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
      $("upd-status").textContent = "Update failed — try again";
      $("upd-status").classList.remove("avail");
      const now = $("upd-now");
      now.textContent = "Update & Restart"; now.disabled = false;
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
