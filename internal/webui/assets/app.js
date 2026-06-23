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
const rangeState = { apps: "session", domains: "session" };
const sortState = { apps: { key: "total", dir: -1 }, domains: { key: "total", dir: -1 } };
let liveApps = [], liveDomains = [];
const rateHist = []; // {t, rx, tx}
const MAXP = 120;

// ============================================================ tables
// filterState holds the per-panel search query (matches name/domain/app/cat/country).
const filterState = { apps: "", domains: "" };
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

  const head = `<thead><tr>
    <th></th>
    <th data-key="name">${isApps ? "App" : "Domain"}</th>
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
    rows += `<tr${rowAttr}>
      <td class="rank">${i + 1}</td>
      <td><div class="cell-name">
        <span class="swatch" style="background:${swatchColor(name)}"></span>
        <span class="label" title="${esc(isApps ? (it.path || name) : name)}">${isApps ? "" : flagChip(it.country)}${esc(name)}${sub}${cat}</span>
      </div><div class="usebar"><i style="width:${(100 * total / max).toFixed(1)}%"></i></div></td>
      <td class="num rx">${fmtBytes(it.rxBytes).str}</td>
      <td class="num tx">${fmtBytes(it.txBytes).str}</td>
      <td class="num">${fmtBytes(total).str}</td>
    </tr>`;
  });
  return `<table class="tbl">${head}<tbody>${rows}</tbody></table>`;
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
    patchRows(tbody, sorted); // same rows: just update numbers + bar widths
    return;
  }
  el.innerHTML = tableHTML(items, target); // structure changed: rebuild
  wireSort(target);
  liveSig[target] = sig;
}

// patchRows updates the numeric cells and bar widths of existing rows in place.
function patchRows(tbody, sorted) {
  const max = Math.max(1, ...sorted.map((x) => Number(x.rxBytes) + Number(x.txBytes)));
  for (let i = 0; i < sorted.length; i++) {
    const it = sorted[i], tr = tbody.children[i];
    if (!tr) continue;
    const total = Number(it.rxBytes) + Number(it.txBytes);
    const nums = tr.querySelectorAll("td.num");
    if (nums[0]) nums[0].textContent = fmtBytes(it.rxBytes).str;
    if (nums[1]) nums[1].textContent = fmtBytes(it.txBytes).str;
    if (nums[2]) nums[2].textContent = fmtBytes(total).str;
    const bar = tr.querySelector(".usebar i");
    if (bar) bar.style.width = (100 * total / max).toFixed(1) + "%";
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

// range tabs
document.querySelectorAll(".tabs").forEach((tabs) => {
  const target = tabs.dataset.target;
  tabs.querySelectorAll("button").forEach((btn) => {
    btn.onclick = () => {
      tabs.querySelectorAll("button").forEach((b) => b.classList.remove("active"));
      btn.classList.add("active");
      rangeState[target] = btn.dataset.range;
      if (btn.dataset.range === "session") renderPanel(target);
      else loadHistory(target, btn.dataset.range);
    };
  });
});

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
  c.width = Math.max(1, r.width * dpr);
  c.height = Math.max(1, r.height * dpr);
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
  const css = getComputedStyle(document.body);
  const cLine = css.getPropertyValue("--line").trim();
  const cMuted = css.getPropertyValue("--muted").trim();
  const cRx = css.getPropertyValue("--rx").trim();
  const cTx = css.getPropertyValue("--tx").trim();

  const peak = Math.max(1, ...rateHist.map((p) => Math.max(p.rx, p.tx)));
  const top = niceMax(peak);

  // gridlines + y labels
  g.font = "10px " + css.getPropertyValue("--mono");
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
  const css = getComputedStyle(document.body);
  const cLine = css.getPropertyValue("--line").trim();
  const cMuted = css.getPropertyValue("--muted").trim();
  const cRx = css.getPropertyValue("--rx").trim();
  const cTx = css.getPropertyValue("--tx").trim();
  const padL = 56, padB = 18, padT = 8, padR = 6;
  const plotW = w - padL - padR, plotH = h - padT - padB;
  if (!histPoints.length) {
    g.fillStyle = cMuted; g.font = "12px " + css.getPropertyValue("--sans");
    g.textAlign = "center"; g.textBaseline = "middle";
    g.fillText("no traffic in this range", w / 2, h / 2);
    return;
  }
  const peak = Math.max(1, ...histPoints.map((p) => Math.max(Number(p.rxBytes), Number(p.txBytes))));
  const top = niceMax(peak);
  g.font = "10px " + css.getPropertyValue("--mono"); g.textBaseline = "middle";
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
  drawChart();
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
function onSnapshot(s) {
  const since = s.sessionStart ? sessionAge(s.sessionStart) : "";
  setStatus("live", (s.interface || "—") + " · capturing" + (since ? " · session " + since : ""));
  $("rxps").textContent = fmtRate(s.rxPerSec);
  $("txps").textContent = fmtRate(s.txPerSec);
  liveApps = s.apps || [];
  liveDomains = s.domains || [];
  $("c-active").textContent = (s.activeApps != null ? s.activeApps : liveApps.length);
  renderPanel("apps"); renderPanel("domains");

  rateHist.push({ t: new Date(s.time).getTime() || Date.now(), rx: Number(s.rxPerSec) || 0, tx: Number(s.txPerSec) || 0 });
  while (rateHist.length > MAXP) rateHist.shift();
  if (chartMode === "live") drawChart(); // don't clobber a history view
  drawSpark("spark-total", getComputedStyle(document.body).getPropertyValue("--accent").trim());
}

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

function setStatus(kind, text) {
  const dot = $("dot");
  dot.className = "dot " + (kind === "live" ? "live" : kind === "warn" ? "warn" : "");
  $("status-text").textContent = text;
}

async function fetchJSON(url) {
  const r = await fetch(url, { cache: "no-store" });
  if (!r.ok) throw new Error(r.status);
  return r.json();
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
  const css = getComputedStyle(document.body);
  const cLine = css.getPropertyValue("--line").trim();
  const cMuted = css.getPropertyValue("--muted").trim();
  const cRx = css.getPropertyValue("--rx").trim();
  const cTx = css.getPropertyValue("--tx").trim();
  if (!points.length) {
    g.fillStyle = cMuted; g.font = "12px " + css.getPropertyValue("--sans");
    g.textAlign = "center"; g.textBaseline = "middle";
    g.fillText("no traffic in this range", w / 2, h / 2);
    return;
  }
  const padL = 52, padB = 4, padT = 8, padR = 6;
  const plotW = w - padL - padR, plotH = h - padT - padB;
  const peak = Math.max(1, ...points.map((p) => Math.max(Number(p.rxBytes), Number(p.txBytes))));
  const top = niceMax(peak);
  g.font = "10px " + css.getPropertyValue("--mono"); g.textBaseline = "middle";
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
