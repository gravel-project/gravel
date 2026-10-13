// The dashboard at a phone's and an iPad's size (gravel#192): for each URL and each size, load the
// page in Chrome, wait until it shows the text it should, fail if it scrolls sideways, and run axe
// at that size. The hub's a11y test (internal/web/a11y_test.go, `make a11y`) runs it beside
// @axe-core/cli, which can only scan at Chrome's default size.
//
//   node scripts/viewport.mjs <chrome> <url> <text it must show> [<url> <text> ...]
//
// With VIEWPORT_SCREENSHOTS set to a directory, it also saves each page at each size there, to look
// at.
//
// axe-core is evaluated through the DevTools protocol, which the page's Content-Security-Policy
// does not govern, so the page is scanned exactly as the hub serves it.
import { mkdir, readFile } from "node:fs/promises";
import { createRequire } from "node:module";

import puppeteer from "puppeteer-core";

const sizes = [
  { name: "phone", width: 390, height: 844, isMobile: true, hasTouch: true },
  { name: "ipad", width: 820, height: 1180, isMobile: true, hasTouch: true },
  { name: "ipad-landscape", width: 1180, height: 820, isMobile: true, hasTouch: true },
];

const [chrome, ...pairs] = process.argv.slice(2);
if (!chrome || pairs.length === 0 || pairs.length % 2 !== 0) {
  console.error("usage: node scripts/viewport.mjs <chrome> <url> <text> [<url> <text> ...]");
  process.exit(2);
}
const axeSource = await readFile(createRequire(import.meta.url).resolve("axe-core/axe.min.js"), "utf8");

const shots = process.env.VIEWPORT_SCREENSHOTS ?? "";
if (shots !== "") {
  await mkdir(shots, { recursive: true });
}

const browser = await puppeteer.launch({ executablePath: chrome, args: ["--no-sandbox"] });
let failed = false;
try {
  for (let i = 0; i < pairs.length; i += 2) {
    const [url, want] = [pairs[i], pairs[i + 1]];
    for (const size of sizes) {
      const page = await browser.newPage();
      await page.setViewport({ width: size.width, height: size.height, isMobile: size.isMobile, hasTouch: size.hasTouch, deviceScaleFactor: 2 });
      await page.goto(url, { waitUntil: "networkidle0" });
      const where = `${url} at ${size.name} (${size.width}×${size.height})`;
      let bad = false;
      try {
        await page.waitForFunction((t) => document.body.innerText.includes(t), { timeout: 10_000 }, want);
      } catch {
        console.error(`FAIL ${where}: the page never showed ${JSON.stringify(want)}`);
        failed = true;
        await page.close();
        continue;
      }
      if (shots !== "") {
        const slug = new URL(url).pathname.replace(/[^a-z0-9]+/gi, "-").replace(/^-|-$/g, "") || "root";
        await page.screenshot({ path: `${shots}/${slug}-${size.name}.png`, fullPage: true });
      }
      const overflow = await page.evaluate(() => document.documentElement.scrollWidth - document.documentElement.clientWidth);
      if (overflow > 0) {
        console.error(`FAIL ${where}: the page scrolls sideways by ${overflow} px`);
        bad = true;
      }
      await page.evaluate(axeSource);
      const result = await page.evaluate(() => globalThis.axe.run(document, { resultTypes: ["violations"] }));
      for (const v of result.violations) {
        console.error(`FAIL ${where}: axe ${v.id} (${v.impact}): ${v.help}`);
        for (const n of v.nodes) {
          console.error(`  ${n.target.join(" ")}: ${n.failureSummary.replace(/\n/g, " ")}`);
        }
        bad = true;
      }
      failed ||= bad;
      if (!bad) {
        console.log(`ok ${where}: no sideways scroll, no axe violations`);
      }
      await page.close();
    }
  }
} finally {
  await browser.close();
}
process.exit(failed ? 1 : 0);
