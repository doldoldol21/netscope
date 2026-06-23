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
})();
