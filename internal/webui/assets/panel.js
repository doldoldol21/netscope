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

function render(s) {
  $("dot").classList.add("live");
  $("meta").textContent = s.interface || "live";
  $("rx").textContent = fmtRate(s.rxPerSec);
  $("tx").textContent = fmtRate(s.txPerSec);
  if (s.activeApps != null) $("active").textContent = s.activeApps + " active";

  hist.push({ rx: Number(s.rxPerSec) || 0, tx: Number(s.txPerSec) || 0 });
  while (hist.length > MAXP) hist.shift();
  drawSpark();

  const apps = (s.apps || []).slice(0, 6);
  const el = $("apps");
  if (!apps.length) { el.innerHTML = '<li class="empty">waiting for traffic…</li>'; return; }
  el.innerHTML = apps.map((a) => {
    const name = a.name || "unknown";
    const total = Number(a.rxBytes) + Number(a.txBytes);
    return `<li><span class="sw" style="background:hsl(${hue(name)} 55% 58%)"></span>` +
      `<span class="nm" title="${esc(a.path || name)}">${esc(name)}</span>` +
      `<span class="by">${fmtBytes(total)}</span></li>`;
  }).join("");
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

let es = null;
function connect() {
  es = new EventSource("/api/live");
  es.onmessage = (e) => { try { render(JSON.parse(e.data)); } catch (_) {} };
  es.onerror = () => { setDisconnected(); es.close(); setTimeout(connect, 2000); };
}

// ---- actions (Wails runtime) ----
const rt = () => window.runtime || {};
$("dash").onclick = () => {
  // Go promotes the window into a standalone dashboard window and navigates.
  const r = rt();
  if (r.EventsEmit) r.EventsEmit("netscope:opendash");
};
$("quit").onclick = () => { const r = rt(); if (r.Quit) r.Quit(); };

window.addEventListener("DOMContentLoaded", () => {
  if (window.runtime && window.runtime.EventsOn) {
    window.runtime.EventsOn("netscope:show", () => { /* already on panel */ });
  }
});

connect();
loadToday();
setInterval(loadToday, 15000);
drawSpark();
window.addEventListener("resize", drawSpark);
