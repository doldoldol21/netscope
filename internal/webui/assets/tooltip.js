/* Lightweight tooltip portal, shared by the dashboard and the popover.
   A single #ns-tip element is appended to <body> and positioned with
   position:fixed, so it escapes every overflow:hidden panel (which clipped a
   CSS ::after approach) and is clamped inside the viewport. Trigger on any
   element with a data-tip attribute; data-tip-down prefers placing it below. */
(function () {
  "use strict";
  let tip = null;
  const PAD = 6;

  function el() {
    if (!tip) {
      tip = document.createElement("div");
      tip.id = "ns-tip";
      document.body.appendChild(tip);
    }
    return tip;
  }

  function show(target) {
    const text = target.getAttribute("data-tip");
    if (!text) return;
    const t = el();
    t.textContent = text;
    t.style.display = "block";
    const r = target.getBoundingClientRect();
    const tr = t.getBoundingClientRect();
    const preferBelow = target.hasAttribute("data-tip-down");

    let top = preferBelow ? r.bottom + 7 : r.top - tr.height - 7;
    // Flip if it would leave the viewport.
    if (!preferBelow && top < PAD) top = r.bottom + 7;
    if (preferBelow && top + tr.height > window.innerHeight - PAD) top = r.top - tr.height - 7;

    let left = r.left + r.width / 2 - tr.width / 2;
    left = Math.max(PAD, Math.min(left, window.innerWidth - tr.width - PAD));

    t.style.top = Math.round(top) + "px";
    t.style.left = Math.round(left) + "px";
  }

  function hide() {
    if (tip) tip.style.display = "none";
  }

  document.addEventListener("mouseover", (e) => {
    const target = e.target.closest && e.target.closest("[data-tip]");
    if (target) show(target);
  });
  document.addEventListener("mouseout", (e) => {
    const target = e.target.closest && e.target.closest("[data-tip]");
    if (target) hide();
  });
  // Hide on any click or scroll so a stale tip never lingers.
  document.addEventListener("click", hide, true);
  window.addEventListener("scroll", hide, true);

  // ---- truncation peek ------------------------------------------------------
  // Hovering an ellipsized name (.label in the tables, .nm in the popover's app
  // list) shows a selectable popover with the full text — the native title
  // tooltip can't be copied from. Click copies it; when the element's title
  // carries extra info (an app's bundle path), it shows as a second line.
  let peek = null, peekHideTimer = 0, peekTarget = null;

  function hidePeek() {
    clearTimeout(peekHideTimer);
    if (peek) peek.style.display = "none";
    peekTarget = null;
  }
  // A grace period so the pointer can travel from the name into the peek to
  // select text without it vanishing.
  function schedulePeekHide() {
    clearTimeout(peekHideTimer);
    peekHideTimer = setTimeout(hidePeek, 250);
  }

  function peekEl() {
    if (peek) return peek;
    peek = document.createElement("div");
    peek.id = "ns-peek";
    peek.addEventListener("mouseenter", () => clearTimeout(peekHideTimer));
    peek.addEventListener("mouseleave", schedulePeekHide);
    peek.addEventListener("click", (e) => {
      e.stopPropagation();
      if (window.getSelection && String(window.getSelection())) return; // selecting, not copying-all
      const text = peek.dataset.text || "";
      const done = () => {
        const m = peek.querySelector(".pk-main");
        if (m) m.textContent = typeof t === "function" ? t("peek.copied") : "Copied";
        peekHideTimer = setTimeout(hidePeek, 700);
      };
      if (navigator.clipboard && navigator.clipboard.writeText) {
        navigator.clipboard.writeText(text).then(done, done);
      } else {
        const ta = document.createElement("textarea");
        ta.value = text;
        document.body.appendChild(ta);
        ta.select();
        try { document.execCommand("copy"); } catch (_) { /* best effort */ }
        ta.remove();
        done();
      }
    });
    document.body.appendChild(peek);
    return peek;
  }

  function showPeek(target) {
    const text = target.textContent.trim();
    if (!text) return;
    // The peek replaces the native tooltip; keep the title's info as a sub line.
    const title = target.getAttribute("title");
    if (title) { target.dataset.peekTitle = title; target.removeAttribute("title"); }
    const extra = target.dataset.peekTitle;

    const p = peekEl();
    p.dataset.text = text;
    p.innerHTML = "";
    const main = document.createElement("div");
    main.className = "pk-main";
    main.textContent = text;
    p.appendChild(main);
    if (extra && extra !== text) {
      const sub = document.createElement("div");
      sub.className = "pk-sub";
      sub.textContent = extra;
      p.appendChild(sub);
    }
    p.style.display = "block";
    const r = target.getBoundingClientRect(), pr = p.getBoundingClientRect();
    let top = r.bottom + 6;
    if (top + pr.height > window.innerHeight - PAD) top = r.top - pr.height - 6;
    const left = Math.max(PAD, Math.min(r.left - 10, window.innerWidth - pr.width - PAD));
    p.style.top = Math.round(top) + "px";
    p.style.left = Math.round(left) + "px";
    peekTarget = target;
  }

  document.addEventListener("mouseover", (e) => {
    const target = e.target.closest && e.target.closest(".label, .nm");
    if (!target) return;
    clearTimeout(peekHideTimer);
    if (target === peekTarget) return;
    if (target.scrollWidth > target.clientWidth + 1) showPeek(target);
    else hidePeek();
  });
  document.addEventListener("mouseout", (e) => {
    if (e.target.closest && e.target.closest(".label, .nm")) schedulePeekHide();
  });
  // Clicks outside the peek (it stops propagation) and scrolls dismiss it.
  document.addEventListener("click", (e) => {
    if (!peek || peek.style.display === "none") return;
    if (!peek.contains(e.target)) hidePeek();
  }, true);
  window.addEventListener("scroll", (e) => {
    if (peek && peek.contains(e.target)) return; // scrolling inside the peek itself
    hidePeek();
  }, true);
})();
