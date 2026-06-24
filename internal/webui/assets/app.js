/* netscope dashboard logic — vanilla JS, no build step.
   Live data arrives via SSE (/api/live); history via REST (/api/apps|domains|summary). */
"use strict";

const $ = (id) => document.getElementById(id);
const API = ""; // same-origin (daemon embed or Wails asset-server proxy)

// ---------- formatting ----------
function fmtBytes(n) {
  n = Number(n) || 0;
  const u = ["B", "KB", "MB", "GB", "TB", "PB"];
  let i = 0;
  while (n >= 1024 && i < u.length - 1) { n /= 1024; i++; }
  const s = i === 0 ? String(n) : n.toFixed(n < 10 ? 2 : n < 100 ? 1 : 0);
  return { num: s, unit: u[i], str: s + " " + u[i] };
}
const fmtRate = (n) => { const b = fmtBytes(n); return b.str + "/s"; };
const esc = (s) => String(s).replace(/[&<>"]/g, (c) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;" }[c]));
// setText assigns textContent only when it changed, avoiding a needless repaint
// of cells that re-render the same value each snapshot.
const setText = (el, s) => { if (el && el.textContent !== s) el.textContent = s; };

// stable color per name
function hueOf(s) {
  let h = 0;
  for (let i = 0; i < s.length; i++) h = (h * 31 + s.charCodeAt(i)) % 360;
  return h;
}
const swatchColor = (name) => `hsl(${hueOf(name)} 55% 58%)`;

// ISO alpha-2 country code -> flag emoji (regional indicator letters).
function flagEmoji(cc) {
  if (!cc || cc.length !== 2) return "";
  return cc.toUpperCase().replace(/./g, (c) => String.fromCodePoint(0x1f1e6 + c.charCodeAt(0) - 65));
}
// A small flag chip for a domain row (empty when country is unknown).
function flagChip(cc) {
  return cc ? `<span class="flag" title="${esc(cc)}">${flagEmoji(cc)}</span> ` : "";
}

// ---------- state ----------
// "session" = cumulative since the daemon started (stable, never resets);
// "today"/"week" = historical totals from storage.
const rangeState = { apps: "session", domains: "session", countries: "session" };
const sortState = { apps: { key: "total", dir: -1 }, domains: { key: "total", dir: -1 } };
let liveApps = [], liveDomains = [];
const rateHist = []; // {t, rx, tx}
const MAXP = 120;

// ============================================================ tables
// filterState holds the per-panel search query (matches name/domain/app/cat/country).
const filterState = { apps: "", domains: "", conns: "" };
function matchFilter(it, target, q) {
  if (target === "apps") {
    return (it.name || "").toLowerCase().includes(q) || (it.path || "").toLowerCase().includes(q);
  }
  return (it.domain || "").toLowerCase().includes(q) || (it.appName || "").toLowerCase().includes(q) ||
    (it.category || "").toLowerCase().includes(q) || (it.country || "").toLowerCase().includes(q);
}

function tableHTML(items, target) {
  const q = filterState[target].toLowerCase(); // case-insensitive (macOS may auto-capitalize input)
  let list = items || [];
  if (q) list = list.filter((it) => matchFilter(it, target, q));
  if (!list.length) {
    if (q) return `<div class="state">no matches for “${esc(q)}”</div>`;
    const session = rangeState[target] === "session";
    return `<div class="state">${session ? "waiting for traffic…" : "no traffic in this range"}</div>`;
  }
  const isApps = target === "apps";
  const s = sortState[target];
  const sorted = [...list].sort((a, b) => {
    const av = sortVal(a, s.key), bv = sortVal(b, s.key);
    return (av < bv ? -1 : av > bv ? 1 : 0) * s.dir;
  });
  const max = Math.max(1, ...sorted.map((x) => Number(x.rxBytes) + Number(x.txBytes)));

  const spark = isApps && rangeState[target] === "session"; // live per-app trend
  const head = `<thead><tr>
    <th></th>
    <th data-key="name">${isApps ? "App" : "Domain"}</th>
    ${spark ? `<th class="spark-col">Trend</th>` : ""}
    <th class="num ${th("down", target)}" data-key="down">↓ Down<span class="caret">▼</span></th>
    <th class="num ${th("up", target)}" data-key="up">↑ Up<span class="caret">▼</span></th>
    <th class="num ${th("total", target)}" data-key="total">Total<span class="caret">▼</span></th>
  </tr></thead>`;

  let rows = "";
  sorted.slice(0, 50).forEach((it, i) => {
    const total = Number(it.rxBytes) + Number(it.txBytes);
    const name = isApps ? (it.name || "unknown") : it.domain;
    const sub = isApps ? "" : (it.appName && it.appName !== "unknown" ? ` <small>· ${esc(it.appName)}</small>` : "");
    const cat = (!isApps && it.category) ? ` <span class="chip">${esc(it.category)}</span>` : "";
    const rowAttr = isApps ? ` data-app="${esc(name)}" class="clickable"` : "";
    // Apps show their real macOS icon (served by /appicon, resolved via
    // NSWorkspace); the colored dot stays underneath as the fallback when no
    // icon resolves (the <img> removes itself on error).
    const ico = isApps
      ? `<span class="cell-ico"><span class="swatch" style="background:${swatchColor(name)}"></span>` +
        `<img class="app-ico" alt="" loading="lazy" onerror="this.remove()" ` +
        `src="/appicon?path=${encodeURIComponent(it.path || "")}&name=${encodeURIComponent(name)}"></span>`
      : `<span class="swatch" style="background:${swatchColor(name)}"></span>`;
    rows += `<tr${rowAttr}>
      <td class="rank">${i + 1}</td>
      <td><div class="cell-name">
        ${ico}
        <span class="label" title="${esc(isApps ? (it.path || name) : name)}">${isApps ? "" : flagChip(it.country)}${esc(name)}${sub}${cat}</span>
      </div><div class="usebar"><i style="width:${(100 * total / max).toFixed(1)}%"></i></div></td>
      ${spark ? `<td class="spark-col">${sparkSVG((appHist.get(name) || {}).pts)}</td>` : ""}
      <td class="num rx">${fmtBytes(it.rxBytes).str}</td>
      <td class="num tx">${fmtBytes(it.txBytes).str}</td>
      <td class="num">${fmtBytes(total).str}</td>
    </tr>`;
  });
  return `<table class="tbl">${head}<tbody>${rows}</tbody></table>`;
}
// Per-app live throughput history (client-side): each snapshot we diff an app's
// cumulative session bytes to get a per-tick delta, building a small ring buffer
// for the row's mini sparkline. No backend change needed.
const appHist = new Map(); // name -> { prev: number, pts: number[] }
const SPARK_PTS = 24;
function updateAppHist(apps) {
  const seen = new Set();
  for (const a of apps || []) {
    const name = a.name || "unknown";
    seen.add(name);
    const total = Number(a.rxBytes) + Number(a.txBytes);
    let h = appHist.get(name);
    if (!h) { h = { prev: total, pts: [] }; appHist.set(name, h); }
    const delta = Math.max(0, total - h.prev);
    h.prev = total;
    h.pts.push(delta);
    if (h.pts.length > SPARK_PTS) h.pts.shift();
  }
  for (const k of appHist.keys()) if (!seen.has(k)) appHist.delete(k); // drop gone apps
}

// sparkSVG renders a tiny throughput trend as an inline SVG polyline (cheap to
// update every second, unlike a per-row canvas).
// sparkPoints returns just the polyline "points" string (or "" if too few
// points), so the live patch path can update an existing polyline's attribute
// instead of re-parsing a whole SVG subtree every second.
function sparkPoints(pts) {
  if (!pts || pts.length < 2) return "";
  const w = 60, h = 16, max = Math.max(1, ...pts), step = w / (pts.length - 1);
  return pts.map((v, i) => `${(i * step).toFixed(1)},${(h - 1 - (v / max) * (h - 2)).toFixed(1)}`).join(" ");
}
function sparkSVG(pts) {
  const d = sparkPoints(pts);
  if (!d) return "";
  return `<svg class="spark-svg" viewBox="0 0 60 16" preserveAspectRatio="none" aria-hidden="true">` +
    `<polyline points="${d}" fill="none" stroke="var(--accent)" stroke-width="1.5" stroke-linejoin="round" stroke-linecap="round"/></svg>`;
}
// setSparkCell updates a row's trend cell in place: patch the existing
// polyline's points (cheap) and only rebuild the SVG when it first appears or
// disappears.
function setSparkCell(cell, pts) {
  const d = sparkPoints(pts);
  const line = cell.querySelector("polyline");
  if (d && line) { if (line.getAttribute("points") !== d) line.setAttribute("points", d); return; }
  const html = sparkSVG(pts);
  if (cell.innerHTML !== html) cell.innerHTML = html;
}

const th = (key, target) => sortState[target].key === key ? "sorted" : "";
function sortVal(x, key) {
  if (key === "down") return Number(x.rxBytes);
  if (key === "up") return Number(x.txBytes);
  if (key === "name") return (x.name || x.domain || "").toLowerCase();
  return Number(x.rxBytes) + Number(x.txBytes);
}

// liveSig remembers the row identity+order so we can patch values in place when
// nothing structural changed — avoiding a full innerHTML rebuild every second
// (which thrashed layout, dropped hover, and made the live view feel choppy).
const liveSig = { apps: "", domains: "" };
function renderPanel(target) {
  if (rangeState[target] !== "session") return; // history handled by loadHistory
  const isApps = target === "apps";
  const items = isApps ? liveApps : liveDomains;
  if (filterState[target]) { // filtering: full rebuild (skip in-place patch)
    $(target).innerHTML = tableHTML(items, target);
    wireSort(target);
    liveSig[target] = "";
    return;
  }
  const s = sortState[target];
  const sorted = [...items].sort((a, b) => {
    const av = sortVal(a, s.key), bv = sortVal(b, s.key);
    return (av < bv ? -1 : av > bv ? 1 : 0) * s.dir;
  }).slice(0, 50);
  const sig = sorted.map((x) => (isApps ? (x.name || "unknown") : x.domain)).join("|") + "#" + s.key + s.dir;
  const el = $(target);
  const tbody = el.querySelector("tbody");
  if (tbody && liveSig[target] === sig && tbody.children.length === sorted.length) {
    patchRows(tbody, sorted, target); // same rows: just update numbers + bar widths
    return;
  }
  el.innerHTML = tableHTML(items, target); // structure changed: rebuild
  wireSort(target);
  liveSig[target] = sig;
}

// patchRows updates the numeric cells and bar widths of existing rows in place.
function patchRows(tbody, sorted, target) {
  const max = Math.max(1, ...sorted.map((x) => Number(x.rxBytes) + Number(x.txBytes)));
  const spark = target === "apps" && rangeState.apps === "session";
  for (let i = 0; i < sorted.length; i++) {
    const it = sorted[i], tr = tbody.children[i];
    if (!tr) continue;
    const total = Number(it.rxBytes) + Number(it.txBytes);
    const nums = tr.querySelectorAll("td.num");
    // Only write when the formatted value actually changed: assigning identical
    // text still triggers a repaint of the tabular-nums cell every tick.
    setText(nums[0], fmtBytes(it.rxBytes).str);
    setText(nums[1], fmtBytes(it.txBytes).str);
    setText(nums[2], fmtBytes(total).str);
    const bar = tr.querySelector(".usebar i");
    if (bar) {
      const wpct = (100 * total / max).toFixed(1) + "%";
      // Skip identical width writes: with the .45s width transition, rewriting
      // the same value every second kept the bars perpetually mid-animation.
      if (bar.style.width !== wpct) bar.style.width = wpct;
    }
    if (spark) {
      const cell = tr.querySelector(".spark-col");
      if (cell) setSparkCell(cell, (appHist.get(it.name || "unknown") || {}).pts);
    }
  }
}

// skeletonTable returns shimmer placeholder rows mirroring the table layout,
// shown while a range loads so the panel doesn't jump from spinner to data.
function skeletonTable(n) {
  let r = "";
  for (let i = 0; i < (n || 7); i++) {
    const nameW = 38 + ((i * 53) % 46); // varied widths read as real labels
    r += `<tr>
      <td class="rank"><span class="skel" style="width:13px;height:9px"></span></td>
      <td><div class="cell-name"><span class="skel" style="width:9px;height:9px;border-radius:3px"></span>
        <span class="skel skel-line" style="width:${nameW}%"></span></div></td>
      <td class="num"><span class="skel skel-line" style="width:46px"></span></td>
      <td class="num"><span class="skel skel-line" style="width:42px"></span></td>
      <td class="num"><span class="skel skel-line" style="width:52px"></span></td>
    </tr>`;
  }
  return `<table class="tbl skel-tbl"><tbody>${r}</tbody></table>`;
}

async function loadHistory(target, range) {
  const el = $(target);
  liveSig[target] = ""; // force a clean rebuild when returning to the live tab
  // Stale-while-revalidate: keep the current table visible during the (near
  // instant) fetch so the panel height doesn't collapse to a short skeleton and
  // bounce back. Only show a skeleton when there's nothing to keep.
  if (!el.querySelector(".tbl")) el.innerHTML = skeletonTable();
  try {
    const data = await fetchJSON(`${API}/api/${target}?range=${range}`);
    el.dataset.cache = JSON.stringify(data || []);
    el.innerHTML = tableHTML(data || [], target);
    el.firstElementChild && el.firstElementChild.classList.add("fade-in");
    wireSort(target);
  } catch (e) {
    el.innerHTML = `<div class="state">failed to load — is netscoped running?</div>`;
  }
}

function wireSort(target) {
  $(target).querySelectorAll("th[data-key]").forEach((thEl) => {
    thEl.onclick = () => {
      const key = thEl.dataset.key;
      const s = sortState[target];
      if (s.key === key) s.dir *= -1; else { s.key = key; s.dir = key === "name" ? 1 : -1; }
      if (rangeState[target] === "session") renderPanel(target);
      else $(target).innerHTML = tableHTML(JSON.parse($(target).dataset.cache || "[]"), target), wireSort(target);
    };
  });
}

// ============================================================ countries
// Aggregate the domains list by country (client-side; country comes from GeoIP).
function countriesHTML(domains) {
  const by = new Map();
  (domains || []).forEach((d) => {
    const cc = d.country;
    if (!cc) return;
    const e = by.get(cc) || { cc, rx: 0, tx: 0, domains: 0 };
    e.rx += Number(d.rxBytes) || 0; e.tx += Number(d.txBytes) || 0; e.domains++;
    by.set(cc, e);
  });
  const list = [...by.values()].sort((a, b) => (b.rx + b.tx) - (a.rx + a.tx));
  if (!list.length) return `<div class="state">no geo-located traffic yet</div>`;
  const max = Math.max(1, ...list.map((c) => c.rx + c.tx));
  const rows = list.slice(0, 50).map((c, i) => {
    const total = c.rx + c.tx;
    return `<tr>
      <td class="rank">${i + 1}</td>
      <td><div class="cell-name"><span class="flag">${flagEmoji(c.cc)}</span>
        <span class="label">${esc(c.cc)} <small>· ${c.domains} domain${c.domains > 1 ? "s" : ""}</small></span>
      </div><div class="usebar"><i style="width:${(100 * total / max).toFixed(1)}%"></i></div></td>
      <td class="num rx">${fmtBytes(c.rx).str}</td>
      <td class="num tx">${fmtBytes(c.tx).str}</td>
      <td class="num">${fmtBytes(total).str}</td>
    </tr>`;
  }).join("");
  return `<table class="tbl"><thead><tr><th></th><th>Country</th>
    <th class="num">↓ Down</th><th class="num">↑ Up</th><th class="num">Total</th></tr></thead>
    <tbody>${rows}</tbody></table>`;
}
let countriesSig = "";
function renderCountries() {
  const el = $("countries"); // tolerate a stale cached HTML without this panel
  if (!el || rangeState.countries !== "session") return;
  const html = countriesHTML(liveDomains);
  // Skip rebuild when unchanged: countries re-derived the same table every
  // second, restarting bar transitions and dropping hover.
  if (html !== countriesSig) { el.innerHTML = html; countriesSig = html; }
}
async function loadCountries(range) {
  const el = $("countries");
  countriesSig = ""; // history view: force a clean rebuild when back to live
  if (!el.querySelector(".tbl")) el.innerHTML = skeletonTable();
  try {
    const data = await fetchJSON(`${API}/api/domains?range=${range}`);
    el.innerHTML = countriesHTML(data || []);
    el.firstElementChild && el.firstElementChild.classList.add("fade-in");
  } catch (e) {
    el.innerHTML = `<div class="state">failed to load</div>`;
  }
}

// ============================================================ live connections
let connsList = []; // last fetched connections
let connsBusy = false;
let connsLastFetch = 0;

function connMatch(c, q) {
  return (c.host || "").toLowerCase().includes(q) || (c.app || "").toLowerCase().includes(q) ||
    (c.country || "").toLowerCase().includes(q) || (c.category || "").toLowerCase().includes(q) ||
    (c.remoteIP || "").toLowerCase().includes(q) || String(c.remotePort).includes(q);
}

function connsHTML(list) {
  let items = list || [];
  const q = filterState.conns.toLowerCase();
  if (q) items = items.filter((c) => connMatch(c, q));
  if (!items.length) {
    return `<div class="state">${q ? `no matches for “${esc(q)}”` : "no active connections"}</div>`;
  }
  const max = Math.max(1, ...items.map((c) => Number(c.rxBytes) + Number(c.txBytes)));
  const rows = items.slice(0, 100).map((c) => {
    const total = Number(c.rxBytes) + Number(c.txBytes);
    const host = c.host || c.remoteIP;
    const ico = `<span class="cell-ico"><span class="swatch" style="background:${swatchColor(c.app)}"></span>` +
      `<img class="app-ico" alt="" loading="lazy" onerror="this.remove()" ` +
      `src="/appicon?path=${encodeURIComponent(c.path || "")}&name=${encodeURIComponent(c.app || "")}"></span>`;
    return `<tr>
      <td><div class="cell-name">${ico}<span class="label" title="${esc(c.app)}">${esc(c.app || "unknown")}</span></div></td>
      <td><div class="cell-name"><span class="flag">${flagEmoji(c.country)}</span>
        <span class="label" title="${esc(host)}:${c.remotePort}">${esc(host)}<small>:${c.remotePort}</small></span></div></td>
      <td><span class="chip">${esc(c.proto)}</span></td>
      <td><div class="usebar"><i style="width:${(100 * total / max).toFixed(1)}%"></i></div></td>
      <td class="num rx">${fmtBytes(c.rxBytes).str}</td>
      <td class="num tx">${fmtBytes(c.txBytes).str}</td>
      <td class="num">${fmtBytes(total).str}</td>
    </tr>`;
  }).join("");
  return `<table class="tbl"><thead><tr>
    <th>App</th><th>Remote</th><th>Proto</th><th></th>
    <th class="num">↓ Down</th><th class="num">↑ Up</th><th class="num">Total</th>
  </tr></thead><tbody>${rows}</tbody></table>`;
}

let connsSig = "";
function renderConns() {
  const el = $("conns");
  if (!el) return;
  const html = connsHTML(connsList);
  // Skip the rebuild when the rendered markup is identical — re-`innerHTML`ing
  // 100 rows of <img> icons every refresh dropped hover and re-ran lazy-load.
  if (html !== connsSig) { el.innerHTML = html; connsSig = html; }
}

async function refreshConnections() {
  if (connsBusy) return;
  connsBusy = true;
  try {
    connsList = (await fetchJSON(`${API}/api/connections?window=15`)) || [];
    renderConns();
  } catch (_) { /* daemon not ready */ }
  finally { connsBusy = false; }
}

// range tabs
document.querySelectorAll(".tabs").forEach((tabs) => {
  const target = tabs.dataset.target;
  tabs.querySelectorAll("button").forEach((btn) => {
    btn.onclick = () => {
      tabs.querySelectorAll("button").forEach((b) => b.classList.remove("active"));
      btn.classList.add("active");
      rangeState[target] = btn.dataset.range;
      const live = btn.dataset.range === "session";
      if (target === "countries") { live ? renderCountries() : loadCountries(btn.dataset.range); }
      else if (live) renderPanel(target);
      else loadHistory(target, btn.dataset.range);
    };
  });
});

// ============================================================ metered / tethering
// Tracks data usage on interfaces the user marks as "metered" (a tethered phone),
// against an optional monthly budget. The daemon sums per-interface daily bytes
// since the billing-cycle start; this just renders and edits the config.
let meteredData = { plans: [], interfaces: [] };
let meteredEditing = null; // iface being edited, "" for the add form, or null

function gib(bytes) { return (Number(bytes) / (1024 ** 3)); }
function cycleDateStr(unix) {
  const d = new Date(unix * 1000);
  return d.toLocaleDateString(undefined, { month: "short", day: "numeric" });
}

async function loadMetered() {
  try {
    meteredData = (await fetchJSON(`${API}/api/metered`)) || { plans: [], interfaces: [] };
  } catch (_) { return; } // endpoint unavailable (older daemon)
  renderMetered();
}

function meteredFormHTML(plan) {
  // plan: existing meteredPlan (edit) or null (add). Interface is a <select> when
  // adding, fixed when editing.
  const ifaces = meteredData.interfaces || [];
  const cur = plan ? plan.iface : (ifaces.find((i) => i.active) || {}).name || (ifaces[0] || {}).name || "";
  const opts = ifaces.map((i) => `<option value="${esc(i.name)}"${i.name === cur ? " selected" : ""}>${esc(i.display || i.name)}${i.active ? " · active" : ""}</option>`).join("");
  const ifaceField = plan
    ? `<span class="m-iface">${esc(plan.iface)}</span><input type="hidden" id="m-iface" value="${esc(plan.iface)}">`
    : `<select id="m-iface">${opts || `<option value="">no interfaces</option>`}</select>`;
  const label = plan ? esc(plan.label) : "";
  const budget = plan && plan.budgetBytes ? gib(plan.budgetBytes).toFixed(1) : "";
  const day = plan ? plan.cycleStartDay || 1 : 1;
  return `<div class="m-form">
    <div class="m-row">${ifaceField}
      <input id="m-label" type="text" placeholder="label (e.g. SKT)" value="${label}" maxlength="24">
    </div>
    <div class="m-row">
      <label>budget <input id="m-budget" type="number" min="0" step="0.5" placeholder="GB" value="${budget}"> GB</label>
      <label>resets day <input id="m-day" type="number" min="1" max="28" value="${day}"></label>
    </div>
    <div class="m-row m-actions">
      <button class="mini" id="m-save">Save</button>
      <button class="mini ghost" id="m-cancel">Cancel</button>
    </div>
  </div>`;
}

function meteredCardHTML(p) {
  const used = Number(p.usedBytes);
  const has = p.budgetBytes > 0;
  const pct = has ? Math.min(100, 100 * used / p.budgetBytes) : 0;
  const usedStr = fmtBytes(used).str;
  const budStr = has ? fmtBytes(p.budgetBytes).str : "no budget";
  const cls = p.overBudget ? " over" : (has && pct >= 80 ? " warn" : "");
  return `<div class="m-card${cls}">
    <div class="m-head">
      <span class="m-name">${esc(p.label || p.iface)}</span>
      <span class="m-sub">${esc(p.iface)} · cycle from ${cycleDateStr(p.cycleStart)}</span>
      <span class="m-edit" data-edit="${esc(p.iface)}" title="Edit">✎</span>
      <span class="m-edit" data-remove="${esc(p.iface)}" title="Stop tracking">✕</span>
    </div>
    <div class="m-usage">${usedStr} <small>/ ${budStr}</small>${p.overBudget ? ` <span class="chip over">over budget</span>` : ""}</div>
    ${has ? `<div class="usebar m-bar"><i style="width:${pct.toFixed(1)}%"></i></div>` : ""}
  </div>`;
}

function renderMetered() {
  const el = $("metered");
  if (!el) return;
  const plans = meteredData.plans || [];
  let html = plans.map(meteredCardHTML).join("");
  if (meteredEditing !== null) {
    const plan = meteredEditing ? plans.find((p) => p.iface === meteredEditing) : null;
    html += meteredFormHTML(plan);
  } else if (!plans.length) {
    html += `<div class="state">No metered interfaces. Tether your phone, then “＋ metered”.</div>`;
  }
  el.innerHTML = html;
  wireMetered();
}

function wireMetered() {
  const el = $("metered");
  el.querySelectorAll("[data-edit]").forEach((b) => b.onclick = () => { meteredEditing = b.dataset.edit; renderMetered(); });
  el.querySelectorAll("[data-remove]").forEach((b) => b.onclick = async () => {
    await postJSON(`${API}/api/metered`, { iface: b.dataset.remove, metered: false });
    await loadMetered();
  });
  const save = $("m-save");
  if (save) save.onclick = async () => {
    const iface = ($("m-iface").value || "").trim();
    if (!iface) { meteredEditing = null; return renderMetered(); }
    const gbVal = parseFloat($("m-budget").value) || 0;
    const day = Math.max(1, Math.min(28, parseInt($("m-day").value, 10) || 1));
    await postJSON(`${API}/api/metered`, {
      iface, metered: true, label: ($("m-label").value || "").trim(),
      budgetBytes: Math.round(gbVal * (1024 ** 3)), cycleStartDay: day,
    });
    meteredEditing = null;
    await loadMetered();
  };
  const cancel = $("m-cancel");
  if (cancel) cancel.onclick = () => { meteredEditing = null; renderMetered(); };
}

(() => {
  const add = $("metered-add");
  if (add) add.onclick = () => { meteredEditing = ""; renderMetered(); };
})();
loadMetered();
// Refresh usage on its own timer (independent of the live snapshot path), every
// 5s, but never while editing (that would wipe the open form) or while hidden.
setInterval(() => {
  if (meteredEditing === null && !document.hidden) loadMetered();
}, 5000);

// ============================================================ theme
// "auto" follows the OS (prefers-color-scheme); light/dark force it via
// <html data-theme>. Chosen in the popover's settings and persisted server-side
// (/theme); we poll so a change made in the popover reflects here while open.
let themeMode = "auto";
// Theme colors are read from CSS custom properties, but getComputedStyle is a
// forced style resolution — doing it on every snapshot (for the chart/sparkline)
// was needless main-thread work since the values only change on a theme switch.
// Cache them and refresh only when the theme actually changes.
const THEME_VARS = ["--line", "--muted", "--rx", "--tx", "--accent", "--mono", "--sans"];
let themeCache = null;
function refreshThemeCache() {
  const css = getComputedStyle(document.body);
  themeCache = {};
  for (const v of THEME_VARS) themeCache[v] = css.getPropertyValue(v).trim();
}
function tvar(name) {
  if (!themeCache) refreshThemeCache();
  return themeCache[name] !== undefined ? themeCache[name] : getComputedStyle(document.body).getPropertyValue(name).trim();
}
function applyTheme(mode) {
  const next = ["auto", "light", "dark"].includes(mode) ? mode : "auto";
  if (next === themeMode) return;
  themeMode = next;
  if (themeMode === "auto") document.documentElement.removeAttribute("data-theme");
  else document.documentElement.setAttribute("data-theme", themeMode);
  refreshThemeCache(); // resolved colors changed with the theme
  if (chartMode === "live") drawChart(); else drawHistChart(); // recolor canvas
}
async function loadTheme() {
  try {
    const d = await fetchJSON(`${API}/theme`);
    applyTheme((d && d.theme) || "auto");
  } catch (_) { /* keep current */ }
}
// The GUI pushes theme changes instantly via dashEvalJS(); this is the hook.
window.nsApplyTheme = applyTheme;
loadTheme(); // initial (covers a theme set before this window opened)
// In "auto" mode the OS can flip light/dark at runtime; refresh the cached
// colors and recolor the canvas when it does.
window.matchMedia("(prefers-color-scheme: dark)").addEventListener("change", () => {
  if (themeMode !== "auto") return;
  refreshThemeCache();
  if (chartMode === "live") drawChart(); else drawHistChart();
});

// live-connections search box (separate: not a tableHTML target)
(() => {
  const box = $("conns-search");
  if (box) box.oninput = () => { filterState.conns = box.value.trim(); renderConns(); };
})();

// search/filter boxes — re-render the current range (live or cached history)
["apps", "domains"].forEach((target) => {
  const box = $(target + "-search");
  if (!box) return;
  box.oninput = () => {
    filterState[target] = box.value.trim();
    if (rangeState[target] === "session") {
      renderPanel(target);
    } else {
      $(target).innerHTML = tableHTML(JSON.parse($(target).dataset.cache || "[]"), target);
      wireSort(target);
    }
  };
});

// ============================================================ summary cards
async function loadSummary() {
  try {
    const s = await fetchJSON(`${API}/api/summary?range=today`);
    const total = Number(s.totalRx) + Number(s.totalTx);
    $("c-total").textContent = fmtBytes(total).str;
    $("c-total-sub").innerHTML = `<span style="color:var(--rx)">↓ ${fmtBytes(s.totalRx).str}</span> &nbsp; <span style="color:var(--tx)">↑ ${fmtBytes(s.totalTx).str}</span>`;
    if (s.topApp && s.topApp.name) {
      $("c-top").textContent = s.topApp.name;
      $("c-top-sub").textContent = fmtBytes(s.topApp.bytes).str + " today";
    }
    if (s.topDomain && s.topDomain.name) {
      $("c-topdomain").textContent = s.topDomain.name;
      $("c-topdomain-sub").textContent = fmtBytes(s.topDomain.bytes).str + " today";
    }
  } catch (e) { /* daemon not ready; leave placeholders */ }
}

// ============================================================ throughput chart
const chart = $("chart"), cx = chart.getContext("2d");
const tip = $("chart-tip");
let hoverIdx = -1;

function sizeCanvas(c) {
  const r = c.getBoundingClientRect();
  const dpr = window.devicePixelRatio || 1;
  const W = Math.max(1, Math.round(r.width * dpr)), H = Math.max(1, Math.round(r.height * dpr));
  // Only reassign width/height when it actually changed: setting either clears
  // and reallocates the backing bitmap, so doing it every frame is wasteful.
  if (c.width !== W || c.height !== H) { c.width = W; c.height = H; }
  const g = c.getContext("2d");
  g.setTransform(dpr, 0, 0, dpr, 0, 0);
  return { w: r.width, h: r.height, g };
}

function niceMax(v) {
  if (v <= 0) return 1024;
  const pow = Math.pow(1024, Math.floor(Math.log(v) / Math.log(1024)));
  const unit = v / pow;
  const step = unit <= 1 ? 1 : unit <= 2 ? 2 : unit <= 5 ? 5 : 10;
  return step * pow;
}

function drawChart() {
  const { w, h, g } = sizeCanvas(chart);
  g.clearRect(0, 0, w, h);
  const padL = 52, padB = 18, padT = 8, padR = 6;
  const plotW = w - padL - padR, plotH = h - padT - padB;
  const cLine = tvar("--line");
  const cMuted = tvar("--muted");
  const cRx = tvar("--rx");
  const cTx = tvar("--tx");

  const peak = Math.max(1, ...rateHist.map((p) => Math.max(p.rx, p.tx)));
  const top = niceMax(peak);

  // gridlines + y labels
  g.font = "10px " + tvar("--mono");
  g.textBaseline = "middle";
  for (let i = 0; i <= 4; i++) {
    const y = padT + (plotH * i) / 4;
    g.strokeStyle = cLine; g.globalAlpha = 0.5; g.beginPath();
    g.moveTo(padL, y); g.lineTo(w - padR, y); g.stroke(); g.globalAlpha = 1;
    g.fillStyle = cMuted; g.textAlign = "right";
    g.fillText(fmtRate(top * (1 - i / 4)), padL - 8, y);
  }
  if (rateHist.length < 2) { $("chart-peak").textContent = ""; return; }

  const x = (i) => padL + (plotW * i) / (MAXP - 1);
  const y = (v) => padT + plotH * (1 - v / top);

  const series = (key, color, soft) => {
    g.beginPath();
    rateHist.forEach((p, i) => { const px = x(i + (MAXP - rateHist.length)), py = y(p[key]); i ? g.lineTo(px, py) : g.moveTo(px, py); });
    const lastX = x(MAXP - 1), firstX = x(MAXP - rateHist.length);
    g.lineTo(lastX, y(0)); g.lineTo(firstX, y(0)); g.closePath();
    const grad = g.createLinearGradient(0, padT, 0, padT + plotH);
    grad.addColorStop(0, soft); grad.addColorStop(1, "transparent");
    g.fillStyle = grad; g.fill();
    g.beginPath();
    rateHist.forEach((p, i) => { const px = x(i + (MAXP - rateHist.length)), py = y(p[key]); i ? g.lineTo(px, py) : g.moveTo(px, py); });
    g.strokeStyle = color; g.lineWidth = 1.6; g.lineJoin = "round"; g.stroke();
  };
  series("rx", cRx, "rgba(63,185,80,.28)");
  series("tx", cTx, "rgba(240,136,62,.28)");

  // x time ticks (every ~30s)
  g.fillStyle = cMuted; g.textAlign = "center"; g.textBaseline = "top";
  const n = rateHist.length;
  for (let k = 0; k <= 4; k++) {
    const idx = Math.round(((n - 1) * k) / 4);
    const p = rateHist[idx]; if (!p) continue;
    const px = x(idx + (MAXP - n));
    const secsAgo = Math.round((rateHist[n - 1].t - p.t) / 1000);
    g.fillText(secsAgo === 0 ? "now" : `-${secsAgo}s`, px, padT + plotH + 5);
  }

  // hover crosshair
  if (hoverIdx >= 0 && hoverIdx < n) {
    const px = x(hoverIdx + (MAXP - n));
    g.strokeStyle = cMuted; g.globalAlpha = 0.5; g.setLineDash([3, 3]);
    g.beginPath(); g.moveTo(px, padT); g.lineTo(px, padT + plotH); g.stroke();
    g.setLineDash([]); g.globalAlpha = 1;
    const p = rateHist[hoverIdx];
    [["rx", cRx], ["tx", cTx]].forEach(([key, col]) => {
      g.fillStyle = col; g.beginPath(); g.arc(px, y(p[key]), 3, 0, 7); g.fill();
    });
  }
  $("chart-peak").textContent = "peak " + fmtRate(peak);
}

// ---- throughput range (live vs day/week/month history) ----
let chartMode = "live", histPoints = [];
const RANGE_LABEL = { day: "last 24 hours", week: "last 7 days", month: "last 30 days" };

document.querySelectorAll("#chart-tabs button").forEach((btn) => {
  btn.onclick = () => {
    document.querySelectorAll("#chart-tabs button").forEach((b) => b.classList.remove("active"));
    btn.classList.add("active");
    chartMode = btn.dataset.mode;
    if (chartMode === "live") {
      $("chart-hint").textContent = "live · last 2 min";
      seedLive();
    } else {
      loadHistChart(chartMode);
    }
  };
});

// seedLive prefills the live chart with the daemon's recent per-second history,
// so it shows the last ~2 min immediately instead of starting blank on open.
async function seedLive() {
  try {
    const pts = await fetchJSON(`${API}/api/ratehist`);
    if (chartMode !== "live") return;
    rateHist.length = 0;
    (pts || []).forEach((p) => rateHist.push({
      t: new Date(p.time).getTime() || Date.now(),
      rx: Number(p.rxPerSec) || 0, tx: Number(p.txPerSec) || 0,
    }));
  } catch (_) { /* daemon not ready */ }
  drawChart();
}

async function loadHistChart(range) {
  $("chart-hint").textContent = "loading " + (RANGE_LABEL[range] || range) + "…";
  try {
    const pts = await fetchJSON(`${API}/api/timeseries?range=${range}`);
    if (chartMode !== range) return; // switched away
    histPoints = pts || [];
    let rx = 0, tx = 0;
    histPoints.forEach((p) => { rx += Number(p.rxBytes) || 0; tx += Number(p.txBytes) || 0; });
    $("chart-hint").textContent =
      `${RANGE_LABEL[range] || range} · ${fmtBytes(rx + tx).str} total`;
    drawHistChart();
  } catch (e) {
    $("chart-hint").textContent = "failed to load history";
  }
}

// drawHistChart renders per-bucket download/upload bytes over time as stacked
// area, with date/time ticks suited to the range.
function drawHistChart() {
  const { w, h, g } = sizeCanvas(chart);
  g.clearRect(0, 0, w, h);
  const cLine = tvar("--line");
  const cMuted = tvar("--muted");
  const cRx = tvar("--rx");
  const cTx = tvar("--tx");
  const padL = 56, padB = 18, padT = 8, padR = 6;
  const plotW = w - padL - padR, plotH = h - padT - padB;
  if (!histPoints.length) {
    g.fillStyle = cMuted; g.font = "12px " + tvar("--sans");
    g.textAlign = "center"; g.textBaseline = "middle";
    g.fillText("no traffic in this range", w / 2, h / 2);
    return;
  }
  const peak = Math.max(1, ...histPoints.map((p) => Math.max(Number(p.rxBytes), Number(p.txBytes))));
  const top = niceMax(peak);
  g.font = "10px " + tvar("--mono"); g.textBaseline = "middle";
  for (let i = 0; i <= 4; i++) {
    const y = padT + (plotH * i) / 4;
    g.strokeStyle = cLine; g.globalAlpha = 0.5; g.beginPath();
    g.moveTo(padL, y); g.lineTo(w - padR, y); g.stroke(); g.globalAlpha = 1;
    g.fillStyle = cMuted; g.textAlign = "right";
    g.fillText(fmtBytes(top * (1 - i / 4)).str, padL - 8, y);
  }
  const n = histPoints.length;
  const x = (i) => padL + (n === 1 ? plotW / 2 : (plotW * i) / (n - 1));
  const y = (v) => padT + plotH * (1 - v / top);
  const series = (key, color, soft) => {
    g.beginPath();
    histPoints.forEach((p, i) => { const px = x(i), py = y(Number(p[key])); i ? g.lineTo(px, py) : g.moveTo(px, py); });
    g.lineTo(x(n - 1), y(0)); g.lineTo(x(0), y(0)); g.closePath();
    const grad = g.createLinearGradient(0, padT, 0, padT + plotH);
    grad.addColorStop(0, soft); grad.addColorStop(1, "transparent");
    g.fillStyle = grad; g.fill();
    g.beginPath();
    histPoints.forEach((p, i) => { const px = x(i), py = y(Number(p[key])); i ? g.lineTo(px, py) : g.moveTo(px, py); });
    g.strokeStyle = color; g.lineWidth = 1.6; g.lineJoin = "round"; g.stroke();
  };
  series("rxBytes", cRx, "rgba(63,185,80,.28)");
  series("txBytes", cTx, "rgba(240,136,62,.28)");
  // x time/date ticks
  g.fillStyle = cMuted; g.textAlign = "center"; g.textBaseline = "top";
  const day = chartMode === "day";
  for (let k = 0; k <= 4; k++) {
    const idx = Math.round(((n - 1) * k) / 4);
    const p = histPoints[idx]; if (!p) continue;
    const d = new Date(p.time);
    const label = day
      ? d.toLocaleTimeString([], { hour: "2-digit", minute: "2-digit" })
      : `${d.getMonth() + 1}/${d.getDate()}`;
    g.fillText(label, x(idx), padT + plotH + 5);
  }
  $("chart-peak").textContent = "peak " + fmtBytes(peak).str + "/bucket";
}

// Coalesce hover redraws to one per animation frame: mousemove fires far faster
// than the display refresh, and each drawChart is a full clear + gradient fills.
let chartRaf = 0;
function scheduleChart() {
  if (chartRaf) return;
  chartRaf = requestAnimationFrame(() => { chartRaf = 0; drawChart(); });
}

chart.addEventListener("mousemove", (e) => {
  if (chartMode !== "live") return; // hover crosshair is for the live view only
  const r = chart.getBoundingClientRect();
  const padL = 52, padR = 6, plotW = r.width - padL - padR;
  const n = rateHist.length;
  const rel = (e.clientX - r.left - padL) / plotW;
  const idx = Math.round(rel * (MAXP - 1)) - (MAXP - n);
  hoverIdx = Math.max(0, Math.min(n - 1, idx));
  const p = rateHist[hoverIdx];
  if (p) {
    tip.style.opacity = 1;
    tip.style.left = (e.clientX - r.left) + "px";
    tip.style.top = "8px";
    const ago = Math.round((rateHist[n - 1].t - p.t) / 1000);
    tip.innerHTML = `<div class="t-time">${ago === 0 ? "now" : ago + "s ago"}</div>
      <div style="color:var(--rx)">▼ <b>${fmtRate(p.rx)}</b></div>
      <div style="color:var(--tx)">▲ <b>${fmtRate(p.tx)}</b></div>`;
  }
  scheduleChart();
});
chart.addEventListener("mouseleave", () => { hoverIdx = -1; tip.style.opacity = 0; if (chartMode === "live") drawChart(); });
window.addEventListener("resize", () => { chartMode === "live" ? drawChart() : drawHistChart(); });

// mini sparklines on cards
function drawSpark(id, color) {
  const c = $(id); if (!c) return;
  const { w, h, g } = sizeCanvas(c);
  g.clearRect(0, 0, w, h);
  if (rateHist.length < 2) return;
  const data = rateHist.map((p) => p.rx + p.tx);
  const max = Math.max(1, ...data);
  g.beginPath();
  data.forEach((v, i) => { const px = (w * i) / (data.length - 1), py = h - (v / max) * (h - 3) - 2; i ? g.lineTo(px, py) : g.moveTo(px, py); });
  g.lineTo(w, h); g.lineTo(0, h); g.closePath();
  const grad = g.createLinearGradient(0, 0, 0, h);
  grad.addColorStop(0, color + "44"); grad.addColorStop(1, "transparent");
  g.fillStyle = grad; g.fill();
}

// ============================================================ live wiring
let lastSnap = null;
function onSnapshot(s) {
  lastSnap = s;
  // Always keep lightweight state current so the view is correct the instant
  // the window is shown again — but skip the expensive DOM work while hidden.
  liveApps = s.apps || [];
  updateAppHist(liveApps);
  liveDomains = s.domains || [];
  rateHist.push({ t: new Date(s.time).getTime() || Date.now(), rx: Number(s.rxPerSec) || 0, tx: Number(s.txPerSec) || 0 });
  while (rateHist.length > MAXP) rateHist.shift();
  if (document.hidden) return; // dashboard occluded/minimized: don't render
  renderSnapshot(s);
}

// renderSnapshot does the per-tick DOM updates and chart draws. Split out from
// onSnapshot so it can be skipped while hidden and replayed on re-show.
function renderSnapshot(s) {
  applyPausedFromSnapshot(!!s.paused);
  const since = s.sessionStart ? sessionAge(s.sessionStart) : "";
  const state = capPaused
    ? (s.interface || "—") + " · paused"
    : (s.interface || "—") + " · capturing" + (since ? " · session " + since : "");
  setStatus(capPaused ? "paused" : "live", state);
  setText($("rxps"), fmtRate(s.rxPerSec));
  setText($("txps"), fmtRate(s.txPerSec));
  setText($("c-active"), String(s.activeApps != null ? s.activeApps : liveApps.length));
  renderPanel("apps"); renderPanel("domains"); renderCountries();

  // Refresh live connections at most ~every 2s (snapshots arrive ~1s).
  const tnow = Date.now();
  if (tnow - connsLastFetch > 2000) { connsLastFetch = tnow; refreshConnections(); }

  if (chartMode === "live") drawChart(); // don't clobber a history view
  drawSpark("spark-total", tvar("--accent"));
}

// When the dashboard becomes visible again, immediately repaint from the latest
// snapshot so it doesn't show a frame of stale data until the next tick.
document.addEventListener("visibilitychange", () => {
  if (!document.hidden && lastSnap) renderSnapshot(lastSnap);
});

function sessionAge(iso) {
  const start = new Date(iso).getTime();
  if (!start) return "";
  let s = Math.max(0, Math.floor((Date.now() - start) / 1000));
  const h = Math.floor(s / 3600); s -= h * 3600;
  const m = Math.floor(s / 60); s -= m * 60;
  if (h) return `${h}h ${m}m`;
  if (m) return `${m}m ${s}s`;
  return `${s}s`;
}

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
  if (b) { b.textContent = p ? "▶ Resume" : "⏸ Pause"; b.title = p ? "Resume capture" : "Pause capture"; b.classList.toggle("on", p); }
}
async function togglePause() {
  const next = !capPaused;
  pausePendingUntil = Date.now() + 3000; // hold our choice until snapshots catch up
  reflectPaused(next); // optimistic; snapshots reconcile
  try {
    await fetch(`${API}/api/capture`, {
      method: "POST", headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ paused: next }),
    });
  } catch (_) { /* ignore; next snapshot reconciles */ }
}
$("pause-btn").onclick = togglePause;

function setStatus(kind, text) {
  const dot = $("dot");
  dot.className = "dot " + (kind === "live" ? "live" : kind === "warn" || kind === "paused" ? "warn" : "");
  $("status-text").textContent = text;
}

async function fetchJSON(url) {
  const r = await fetch(url, { cache: "no-store" });
  if (!r.ok) throw new Error(r.status);
  return r.json();
}

async function postJSON(url, body) {
  const r = await fetch(url, { method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify(body) });
  if (!r.ok) throw new Error(r.status);
  return r.text();
}

let es = null;
function connect() {
  setStatus("warn", "connecting…");
  es = new EventSource(`${API}/api/live`);
  es.onmessage = (e) => { try { onSnapshot(JSON.parse(e.data)); } catch (_) {} };
  es.onerror = () => { setStatus("warn", "reconnecting…"); es.close(); setTimeout(connect, 2000); };
}

// keyboard shortcuts
window.addEventListener("keydown", (e) => {
  if (e.key === "Escape" && $("drill").classList.contains("show")) { closeDrill(); return; }
  if (e.target.tagName === "INPUT") return;
  if (e.key === "l" || e.key === "L") {
    document.querySelectorAll('.tabs').forEach((t) => t.querySelector('[data-range="session"]').click());
  } else if (e.key === "r" || e.key === "R") {
    ["apps", "domains"].forEach((t) => { if (rangeState[t] !== "session") loadHistory(t, rangeState[t]); });
    loadSummary();
  }
});

// version / update banner in the footer
async function loadVersion() {
  try {
    const v = await fetchJSON(`${API}/api/version`); // daemon build + update status
    const app = await fetchJSON(`${API}/appinfo`).catch(() => null); // GUI build
    const el = $("version");
    if (!el) return;
    const daemon = v.current || "—";
    const appVer = (app && app.version) || daemon;
    // Show the app version, and the daemon version too when they differ.
    let base = "netscope " + appVer;
    if (daemon !== appVer) base += ` · daemon ${daemon}`;
    if (v.updateAvailable) {
      el.innerHTML = `${esc(base)} · <b style="color:var(--accent)">⬆ ${esc(v.latest)} available — see the menu bar</b>`;
    } else {
      el.textContent = base;
    }
  } catch (e) { /* daemon not ready */ }
}


// ============================================================ per-app drill-down
const drillState = { app: null, range: "today" };
let drillCache = [], drillDomains = [];

function openDrill(app) {
  drillState.app = app;
  drillState.range = "today";
  $("drill-name").textContent = app;
  $("drill-name").title = app;
  $("drill-swatch").style.background = swatchColor(app);
  document.querySelectorAll("#drill-tabs button").forEach((b) =>
    b.classList.toggle("active", b.dataset.range === "today"));
  // Fresh open: clear prior content so skeletons show (range switches keep it).
  $("drill-totals").textContent = "";
  $("drill-domains").innerHTML = "";
  $("drill").classList.add("show");
  loadDrill();
}
function closeDrill() { $("drill").classList.remove("show"); drillState.app = null; }

async function loadDrill() {
  const app = drillState.app, range = drillState.range;
  if (!app) return;
  const enc = encodeURIComponent(app);
  const dd0 = $("drill-domains");
  if (!dd0.querySelector(".tbl")) dd0.innerHTML = skeletonTable(8); // keep prior on range switch
  if (!$("drill-totals").textContent.trim())
    $("drill-totals").innerHTML = `<span class="skel skel-line" style="width:120px;height:18px"></span>`;
  try {
    const [ts, doms] = await Promise.all([
      fetchJSON(`${API}/api/timeseries?range=${range}&app=${enc}`),
      fetchJSON(`${API}/api/domains?range=${range}&app=${enc}`),
    ]);
    if (drillState.app !== app) return; // switched away
    drillCache = ts || [];
    let rx = 0, tx = 0;
    (ts || []).forEach((p) => { rx += Number(p.rxBytes) || 0; tx += Number(p.txBytes) || 0; });
    $("drill-totals").innerHTML =
      `<span class="dt-total">${fmtBytes(rx + tx).str}</span>` +
      `<span style="color:var(--rx)">↓ ${fmtBytes(rx).str}</span>` +
      `<span style="color:var(--tx)">↑ ${fmtBytes(tx).str}</span>` +
      `<span class="dt-cap">${range}</span>`;
    drawDrillChart(ts || []);
    drillDomains = doms || []; // remembered for the drill export button
    const dd = $("drill-domains");
    dd.innerHTML = drillDomainsHTML(doms || []);
    dd.firstElementChild && dd.firstElementChild.classList.add("fade-in");
  } catch (e) {
    $("drill-domains").innerHTML = `<div class="state">failed to load — is netscoped running?</div>`;
  }
}

function drillDomainsHTML(items) {
  if (!items.length) return `<div class="state">no domains in this range</div>`;
  const max = Math.max(1, ...items.map((x) => Number(x.rxBytes) + Number(x.txBytes)));
  const rows = items.slice(0, 40).map((d, i) => {
    const total = Number(d.rxBytes) + Number(d.txBytes);
    const cat = d.category ? ` <span class="chip">${esc(d.category)}</span>` : "";
    return `<tr>
      <td class="rank">${i + 1}</td>
      <td><div class="cell-name">
        <span class="swatch" style="background:${swatchColor(d.domain)}"></span>
        <span class="label" title="${esc(d.domain)}">${flagChip(d.country)}${esc(d.domain)}${cat}</span>
      </div><div class="usebar"><i style="width:${(100 * total / max).toFixed(1)}%"></i></div></td>
      <td class="num rx">${fmtBytes(d.rxBytes).str}</td>
      <td class="num tx">${fmtBytes(d.txBytes).str}</td>
      <td class="num">${fmtBytes(total).str}</td>
    </tr>`;
  }).join("");
  return `<table class="tbl"><thead><tr><th></th><th>Domain</th>
    <th class="num">↓ Down</th><th class="num">↑ Up</th><th class="num">Total</th></tr></thead>
    <tbody>${rows}</tbody></table>`;
}

// drawDrillChart plots per-bucket download/upload bytes for the selected app.
function drawDrillChart(points) {
  const c = $("drill-chart");
  const { w, h, g } = sizeCanvas(c);
  g.clearRect(0, 0, w, h);
  const cLine = tvar("--line");
  const cMuted = tvar("--muted");
  const cRx = tvar("--rx");
  const cTx = tvar("--tx");
  if (!points.length) {
    g.fillStyle = cMuted; g.font = "12px " + tvar("--sans");
    g.textAlign = "center"; g.textBaseline = "middle";
    g.fillText("no traffic in this range", w / 2, h / 2);
    return;
  }
  const padL = 52, padB = 4, padT = 8, padR = 6;
  const plotW = w - padL - padR, plotH = h - padT - padB;
  const peak = Math.max(1, ...points.map((p) => Math.max(Number(p.rxBytes), Number(p.txBytes))));
  const top = niceMax(peak);
  g.font = "10px " + tvar("--mono"); g.textBaseline = "middle";
  for (let i = 0; i <= 4; i++) {
    const y = padT + (plotH * i) / 4;
    g.strokeStyle = cLine; g.globalAlpha = 0.5; g.beginPath();
    g.moveTo(padL, y); g.lineTo(w - padR, y); g.stroke(); g.globalAlpha = 1;
    g.fillStyle = cMuted; g.textAlign = "right";
    g.fillText(fmtBytes(top * (1 - i / 4)).str, padL - 8, y);
  }
  const n = points.length;
  const x = (i) => padL + (n === 1 ? plotW / 2 : (plotW * i) / (n - 1));
  const y = (v) => padT + plotH * (1 - v / top);
  const series = (key, color, soft) => {
    g.beginPath();
    points.forEach((p, i) => { const px = x(i), py = y(Number(p[key])); i ? g.lineTo(px, py) : g.moveTo(px, py); });
    g.lineTo(x(n - 1), y(0)); g.lineTo(x(0), y(0)); g.closePath();
    const grad = g.createLinearGradient(0, padT, 0, padT + plotH);
    grad.addColorStop(0, soft); grad.addColorStop(1, "transparent");
    g.fillStyle = grad; g.fill();
    g.beginPath();
    points.forEach((p, i) => { const px = x(i), py = y(Number(p[key])); i ? g.lineTo(px, py) : g.moveTo(px, py); });
    g.strokeStyle = color; g.lineWidth = 1.6; g.lineJoin = "round"; g.stroke();
  };
  series("rxBytes", cRx, "rgba(63,185,80,.28)");
  series("txBytes", cTx, "rgba(240,136,62,.28)");
}

// wiring: click an app row to drill in; tabs/close/Esc/backdrop to navigate out
$("apps").addEventListener("click", (e) => {
  const tr = e.target.closest("tr[data-app]");
  if (tr) openDrill(tr.dataset.app);
});
document.querySelectorAll("#drill-tabs button").forEach((btn) => {
  btn.onclick = () => {
    document.querySelectorAll("#drill-tabs button").forEach((b) => b.classList.remove("active"));
    btn.classList.add("active");
    drillState.range = btn.dataset.range;
    loadDrill();
  };
});
$("drill-close").onclick = closeDrill;
$("drill").addEventListener("click", (e) => { if (e.target === $("drill")) closeDrill(); });
window.addEventListener("resize", () => { if ($("drill").classList.contains("show")) drawDrillChart(drillCache); });

// ============================================================ export (CSV)
const csvCell = (v) => {
  const s = String(v == null ? "" : v);
  return /[",\n]/.test(s) ? '"' + s.replace(/"/g, '""') + '"' : s;
};
const toCSV = (rows) => rows.map((r) => r.map(csvCell).join(",")).join("\n") + "\n";

// exportAll saves apps + domains together (the current range) as one JSON file.
async function exportAll() {
  const range = rangeState.apps;
  let apps, domains;
  if (range === "session") {
    apps = liveApps || []; domains = liveDomains || [];
  } else {
    [apps, domains] = await Promise.all([
      fetchJSON(`${API}/api/apps?range=${range}`).catch(() => []),
      fetchJSON(`${API}/api/domains?range=${range}`).catch(() => []),
    ]);
  }
  const bundle = { generatedAt: new Date().toISOString(), range, apps: apps || [], domains: domains || [] };
  const date = new Date().toISOString().slice(0, 10);
  saveFile(`netscope-${range}-${date}.json`, JSON.stringify(bundle, null, 2));
}

// exportDrillDomains saves the focused app's per-domain breakdown as CSV.
function exportDrillDomains() {
  const app = drillState.app;
  if (!app) return;
  const rows = [["domain", "country", "category", "rx_bytes", "tx_bytes", "total_bytes"]];
  (drillDomains || []).forEach((d) => rows.push([d.domain, d.country || "", d.category || "",
    d.rxBytes || 0, d.txBytes || 0, (Number(d.rxBytes) + Number(d.txBytes)) || 0]));
  const safe = app.replace(/[^\w.-]+/g, "_");
  saveFile(`netscope-${safe}-domains-${drillState.range}.csv`, toCSV(rows));
}

// saveFile hands the text to native (Save panel) in the app, or falls back to a
// blob download in a plain browser / dev.
function saveFile(filename, text) {
  const bridge = window.webkit && window.webkit.messageHandlers && window.webkit.messageHandlers.netscopeExport;
  if (bridge) { bridge.postMessage({ filename, text }); return; }
  const a = document.createElement("a");
  a.href = URL.createObjectURL(new Blob([text], { type: "text/csv" }));
  a.download = filename;
  document.body.appendChild(a); a.click(); a.remove();
  setTimeout(() => URL.revokeObjectURL(a.href), 1000);
}

$("export-all").onclick = exportAll;
$("drill-export").onclick = exportDrillDomains;

// boot
seedLive(); // prefill the live chart from the daemon's recent history
connect();
loadSummary();
loadVersion();
setInterval(loadSummary, 10000);
setInterval(loadVersion, 3600000);
