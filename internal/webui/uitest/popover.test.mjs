// Smoke tests for the popover page (index.html + panel.js + tooltip.js) in
// WebKit, the engine the app's WKWebView uses. They load the embedded assets
// straight from disk, with a stand-in for the Wails runtime that records what
// the page emits, so no daemon or app build is needed.
//
// Each test pins a behaviour that once shipped broken and was only caught by
// hand: the peek's timing, its size, and the settings covering the main view.
import { test, before, after } from "node:test";
import assert from "node:assert/strict";
import { fileURLToPath, pathToFileURL } from "node:url";
import path from "node:path";
import { webkit } from "playwright";

const assets = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "../assets");
const popoverURL = pathToFileURL(path.join(assets, "index.html")).href;

let browser;
before(async () => {
  browser = await webkit.launch();
  // The first page in a fresh browser is slow to come up; take that here
  // rather than inside whichever test happens to run first.
  const warm = await browser.newPage();
  await warm.goto(popoverURL);
  await warm.close();
});
after(async () => { await browser?.close(); });

// newPopover opens index.html at the popover's size, with the Wails runtime
// stubbed (every EventsEmit lands in window.__emits) and three app rows whose
// names fit, so the peek takes its delayed path.
async function newPopover() {
  const page = await browser.newPage({ viewport: { width: 320, height: 270 } });
  await page.addInitScript(() => {
    // Timings are taken in the page, not by the test: a round trip to the
    // browser can itself outlast the peek's delay on a cold CI runner.
    window.__overs = []; // [performance.now(), name] per pointer entry
    window.__shown = []; // [performance.now(), name] per peek change
    document.addEventListener("mouseover", (e) => {
      const n = e.target.closest && e.target.closest(".nm");
      if (n) window.__overs.push([performance.now(), n.textContent]);
    }, true);
    let last = null;
    new MutationObserver(() => {
      const p = document.getElementById("ns-peek");
      const now = p && p.style.display === "block" ? p.querySelector(".pk-main")?.textContent : null;
      if (now !== last) { last = now; if (now) window.__shown.push([performance.now(), now]); }
    }).observe(document, { subtree: true, childList: true, attributes: true, characterData: true });
    window.__emits = [];
    window.runtime = {
      EventsEmit: (name, ...args) => window.__emits.push([name, ...args]),
      EventsOn: () => {},
    };
  });
  await page.goto(popoverURL);
  await page.evaluate(() => {
    window.__rows = (names) => {
      document.getElementById("apps").innerHTML = names.map((n) =>
        `<li data-k="${n}"><span class="nm" title="/Applications/${n}.app/Contents/MacOS/${n}">${n}</span>` +
        `<span class="by">1 MB</span><span class="ub"><i class="rx"></i><i class="tx"></i></span></li>`).join("");
    };
    window.__rows(["Safari", "Slack", "Mail"]);
  });
  return page;
}

const peekText = (page) => page.evaluate(() => {
  const p = document.getElementById("ns-peek");
  return p && p.style.display === "block" ? p.querySelector(".pk-main").textContent : null;
});

async function hover(page, i) {
  const r = await page.locator(".nm").nth(i).boundingBox();
  await page.mouse.move(r.x + 8, r.y + r.height / 2);
}

async function awayFromNames(page) {
  await page.mouse.move(300, 20); // the header, clear of every name
}

// lag is how long, in the page's own clock, the peek took to show name after
// the pointer last entered it.
const lag = (page, name) => page.evaluate((n) => {
  const over = window.__overs.filter(([, m]) => m === n).pop();
  const shown = window.__shown.find(([t, m]) => m === n && over && t >= over[0]);
  return over && shown ? shown[0] - over[0] : null;
}, name);

// waitPeek polls until the peek shows `name` or the time runs out.
async function waitPeek(page, name, ms) {
  const end = Date.now() + ms;
  while (Date.now() < end) {
    if ((await peekText(page)) === name) return true;
    await page.waitForTimeout(10);
  }
  return false;
}

test("a name that fits peeks after a beat, not at once", async () => {
  const page = await newPopover();
  await awayFromNames(page);
  await hover(page, 0);
  assert.ok(await waitPeek(page, "Safari", 2000), "never peeked");
  const ms = await lag(page, "Safari");
  assert.ok(ms >= 120, `peeked ${ms}ms after the pointer arrived; a name that fits waits a beat`);
  await page.close();
});

test("moving to the next name while one is up switches at once", async () => {
  const page = await newPopover();
  await hover(page, 0);
  assert.ok(await waitPeek(page, "Safari", 1000));
  await hover(page, 1);
  assert.ok(await waitPeek(page, "Slack", 2000), "kept showing the old name");
  const ms = await lag(page, "Slack");
  // Well inside the first-hover delay: only the warm path is this quick.
  assert.ok(ms < 100, `took ${ms}ms to switch; it should not wait the first-hover delay again`);
  await page.close();
});

test("a list rebuilt mid-wait still peeks the row under the pointer", async () => {
  const page = await newPopover();
  await awayFromNames(page);
  await page.waitForTimeout(800); // let any warm window lapse
  await hover(page, 1);
  await page.waitForTimeout(40);
  await page.evaluate(() => window.__rows(["Zoom", "Figma", "Notion"]));
  assert.ok(await waitPeek(page, "Figma", 1000), "the timer held on to the detached row");
  await page.close();
});

test("the peek is two lines with a copy icon, and a click copies", async () => {
  const page = await newPopover();
  await hover(page, 0);
  assert.ok(await waitPeek(page, "Safari", 1000));
  const shape = await page.evaluate(() => {
    const p = document.getElementById("ns-peek");
    return {
      icon: !!p.querySelector(".pk-row .pk-ico svg"),
      hint: !!p.querySelector(".pk-hint"),
      sub: p.querySelector(".pk-sub")?.textContent,
      height: p.getBoundingClientRect().height,
    };
  });
  assert.equal(shape.icon, true, "no copy icon beside the name");
  assert.equal(shape.hint, false, "the click-to-copy line is back");
  assert.equal(shape.sub, "/Applications");
  assert.ok(shape.height < 56, `peek is ${shape.height}px tall; the three-line one was 62`);

  await page.locator("#ns-peek").click();
  const after = await page.evaluate(() => [
    document.querySelector("#ns-peek .pk-main").textContent,
    !!document.querySelector("#ns-peek .pk-ico"),
  ]);
  assert.match(after[0], /✓/);
  assert.equal(after[1], false, "the icon stays next to the confirmation");
  await page.close();
});

test("settings hide the main view and ask for their height; closing gives it back", async () => {
  const page = await newPopover();
  const mainView = () => page.evaluate(() =>
    [...document.getElementById("panel").children]
      .filter((c) => c.id !== "settings" && !c.hidden)
      .map((c) => getComputedStyle(c).visibility));
  const heights = () => page.evaluate(() =>
    window.__emits.filter(([n]) => n === "netscope:popoverheight").map(([, h]) => h));

  await page.click("#alerts-btn");
  await page.waitForTimeout(300); // the .15s cross-fade
  assert.ok((await mainView()).every((v) => v === "hidden"), "main view shows through the settings");
  const asked = await heights();
  assert.equal(asked.length, 1);
  assert.ok(asked[0] > 270, `asked for ${asked[0]}px, no taller than the popover`);
  const fits = await page.evaluate(() => {
    const b = document.querySelector(".set-body");
    return b.scrollHeight - b.clientHeight;
  });
  // At 320×270 the body overflows; the height asked for is what removes that.
  assert.ok(fits > 0, "test setup: settings already fit, so the height check proves nothing");

  await page.click("#set-close");
  await page.waitForTimeout(300);
  assert.ok((await mainView()).every((v) => v === "visible"), "main view did not come back");
  assert.deepEqual((await heights()).slice(-1), [0], "closing did not hand the height back");

  await page.click("#alerts-btn");
  await page.keyboard.press("Escape");
  await page.waitForTimeout(300);
  assert.ok((await mainView()).every((v) => v === "visible"), "Escape left the main view hidden");
  assert.deepEqual((await heights()).slice(-1), [0], "Escape did not hand the height back");
  await page.close();
});

test("the peek leaves the next name in the list reachable", async () => {
  // Below the name, the peek sat over the next row: moving down the list
  // landed on the peek, and the next name could not be hovered at all.
  const page = await newPopover();
  await hover(page, 0);
  assert.ok(await waitPeek(page, "Safari", 1000));
  const r1 = await page.locator(".nm").nth(1).boundingBox();
  const hit = await page.evaluate(([x, y]) => document.elementFromPoint(x, y).className,
    [r1.x + 8, r1.y + r1.height / 2]);
  assert.equal(hit, "nm", `the next name is under the ${hit}`);
  await page.close();
});
