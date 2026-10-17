// Run against an isolated nclip instance. Install Playwright separately; production has no Node dependency.
import { createRequire } from "node:module";
import { mkdir } from "node:fs/promises";
import assert from "node:assert/strict";
const require = createRequire(import.meta.url);
const { chromium } = require(
  process.env.NCLIP_PLAYWRIGHT_MODULE || "playwright",
);
const base = process.env.NCLIP_TEST_URL || "http://127.0.0.1:18080";
const evidence = process.env.NCLIP_EVIDENCE_DIR || "browser-evidence";
await mkdir(evidence, { recursive: true });
const browser = await chromium.launch({
  headless: true,
  ...(process.env.NCLIP_BROWSER_CHANNEL
    ? { channel: process.env.NCLIP_BROWSER_CHANNEL }
    : {}),
});
const context = await browser.newContext({
  viewport: { width: 1100, height: 850 },
  locale: "zh-CN",
  timezoneId: "Asia/Shanghai",
  permissions: ["clipboard-read", "clipboard-write"],
  reducedMotion: "reduce",
});
const page = await context.newPage();
const errors = [];
page.on("pageerror", (error) => errors.push(error.message));
const slugs = new Set();
async function waitVisible(id) {
  await page.locator(`#${id}`).waitFor({ state: "visible" });
}
async function home() {
  await page.goto(base);
  await page.locator("#limits").filter({ hasText: "10.0 MiB" }).waitFor();
}
async function send() {
  await page.locator("#send").click();
  await waitVisible("result");
  const url = await page.locator("#share-url").inputValue();
  assert.match(url, new RegExp(`^${base}/[A-Z2-9]+$`));
  slugs.add(url.split("/").pop());
  return url;
}
async function open(url) {
  await page.goto(url);
  await page.locator("#received-items article").first().waitFor();
}
async function pasteFromSystem() {
  await page.locator("#paste").click();
  await page.waitForFunction(
    () => document.getElementById("message").value.length > 0,
  );
}
try {
  await home();
  assert.equal(
    await page
      .getByRole("heading", { name: "粘贴，然后分享", exact: true })
      .innerText(),
    "粘贴，然后分享",
  );
  await page.locator("#message").fill("  中文跨端测试\n保留末尾换行\n");
  await page.screenshot({
    path: `${evidence}/desktop-compose.png`,
    fullPage: true,
  });
  await page.locator("#message").press("Control+Enter");
  await waitVisible("result");
  const url = await page.locator("#share-url").inputValue();
  slugs.add(url.split("/").pop());
  assert.equal(await page.evaluate(() => navigator.clipboard.readText()), url);
  await open(url);
  await page.getByRole("button", { name: "复制到剪贴板", exact: true }).click();
  await page.locator("#status").filter({ hasText: "已复制" }).waitFor();
  assert.equal(
    (await page.evaluate(() => navigator.clipboard.readText())).replaceAll(
      "\r\n",
      "\n",
    ),
    "  中文跨端测试\n保留末尾换行\n",
  );
  console.log("PASS text -> auto link copy -> recipient clipboard");

  await home();
  await page.evaluate(async () =>
    navigator.clipboard.write([
      new ClipboardItem({
        "text/plain": new Blob(["富文本原文"], { type: "text/plain" }),
        "text/html": new Blob(["<b>富文本原文</b>"], { type: "text/html" }),
      }),
    ]),
  );
  await pasteFromSystem();
  assert.equal(await page.locator("#rich-hint").isVisible(), true);
  const richURL = await send();
  await open(richURL);
  await page.getByRole("button", { name: "复制到剪贴板", exact: true }).click();
  await page.locator("#status").filter({ hasText: "已复制" }).waitFor();
  const html = await page.evaluate(async () => {
    const entry = (await navigator.clipboard.read())[0];
    return (await entry.getType("text/html")).text();
  });
  assert.match(html, /<b[^>]*>富文本原文<\/b>/);
  console.log("PASS rich-text multi-representation round trip");

  await home();
  await page.locator("#message").fill("混合批量");
  const png = Buffer.from(
    "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mP8/x8AAusB9Wl6mZ0AAAAASUVORK5CYII=",
    "base64",
  );
  await page.locator("#files").setInputFiles([
    { name: "截图.png", mimeType: "image/png", buffer: png },
    {
      name: "报告.txt",
      mimeType: "text/plain",
      buffer: Buffer.from("报告文件"),
    },
  ]);
  await page.locator("#draft-items article").nth(1).waitFor();
  const batchURL = await send();
  await open(batchURL);
  assert.equal(await page.locator("#received-items article").count(), 3);
  await page.waitForFunction(() =>
    Array.from(document.querySelectorAll("#received-items img")).every(
      (img) => img.complete && img.naturalWidth > 0,
    ),
  );
  assert.equal(await page.locator("#download-all").isVisible(), true);
  const archive = await context.request.get(
    await page
      .locator("#download-all")
      .getAttribute("href")
      .then((href) => new URL(href, base).href),
  );
  assert.equal((await archive.body()).subarray(0, 2).toString(), "PK");
  const downloadEvent = page.waitForEvent("download");
  await page.getByRole("button", { name: "下载", exact: true }).last().click();
  assert.equal((await downloadEvent).suggestedFilename(), "报告.txt");
  console.log("PASS mixed batch, file names and ZIP download");

  await home();
  await page.locator("#message").fill("一次性秘密");
  await page.locator("#burn").check();
  const burnURL = await send();
  const slug = burnURL.split("/").pop();
  await page.goto(burnURL);
  await waitVisible("receive-gate");
  assert.equal(await page.locator("#received-items").innerText(), "");
  assert.equal(
    (
      await context.request.get(`${base}/api/v1/shares/${slug}/content`)
    ).status(),
    409,
  );
  assert.equal(
    (await context.request.get(`${base}/api/v1/shares/${slug}`)).status(),
    200,
  );
  await page.locator("#receive").click();
  await page.locator("#received-items article").waitFor();
  assert.equal(
    (await context.request.get(`${base}/api/v1/shares/${slug}`)).status(),
    404,
  );
  await page.getByRole("button", { name: "复制到剪贴板", exact: true }).click();
  await page.locator("#status").filter({ hasText: "已复制" }).waitFor();
  assert.equal(
    await page.evaluate(() => navigator.clipboard.readText()),
    "一次性秘密",
  );
  console.log("PASS confirmation gate and copying retained one-time content");

  await home();
  await page.locator("#files").setInputFiles([
    { name: "一.txt", mimeType: "text/plain", buffer: Buffer.from("one") },
    { name: "二.txt", mimeType: "text/plain", buffer: Buffer.from("two") },
  ]);
  await page.locator("#draft-items article").nth(1).waitFor();
  await page.locator("#burn").check();
  const burnBatch = await send();
  await page.goto(burnBatch);
  await waitVisible("receive-gate");
  await page.locator("#receive").click();
  await page.locator("#received-items article").nth(1).waitFor();
  const burnDownload = page.waitForEvent("download");
  await page.locator("#download-all").click();
  assert.match((await burnDownload).suggestedFilename(), /\.zip$/);
  console.log("PASS one-time batch retains a real ZIP after server deletion");

  await home();
  await page.locator("#message").fill("失败后仍保留的输入");
  let uploads = 0;
  await page.route("**/api/v1/shares", (route) => {
    uploads++;
    return route.fulfill({
      status: 413,
      contentType: "text/html",
      body: "<h1>Too large</h1>",
    });
  });
  await page.locator("#send").click();
  await page.locator("#status").filter({ hasText: "超过上传限制" }).waitFor();
  assert.equal(
    await page.locator("#message").inputValue(),
    "失败后仍保留的输入",
  );
  assert.equal(uploads, 1);
  await page.unroute("**/api/v1/shares");
  console.log("PASS proxy error feedback preserves draft without retry");

  await page.locator("#settings summary").click();
  await page.locator("#language").selectOption("en");
  await page.locator("#theme").selectOption("dark");
  await page.keyboard.press("Escape");
  assert.equal(await page.locator("html").getAttribute("lang"), "en");
  assert.equal(await page.locator("html").getAttribute("data-theme"), "dark");
  for (const width of [320, 390]) {
    await page.setViewportSize({ width, height: 820 });
    assert.equal(
      await page.evaluate(
        () => document.documentElement.scrollWidth <= innerWidth,
      ),
      true,
    );
    for (const id of ["paste", "attach", "send"]) {
      const bounds = await page.locator(`#${id}`).boundingBox();
      assert.ok(bounds.width >= 44 && bounds.height >= 44);
    }
    await page.screenshot({
      path: `${evidence}/mobile-${width}-dark-en.png`,
      fullPage: true,
    });
  }
  await page.reload();
  assert.equal(await page.locator("html").getAttribute("data-theme"), "dark");
  console.log(
    "PASS 320/390px controls, English, dark theme and saved preferences",
  );

  const lateContext = await browser.newContext({ locale: "zh-CN" });
  await lateContext.addInitScript(() => {
    localStorage.setItem(
      "nclip.preferences.v1",
      JSON.stringify({ suggestions: true }),
    );
    Object.defineProperty(navigator, "permissions", {
      value: { query: async () => ({ state: "granted" }) },
    });
    Object.defineProperty(navigator, "clipboard", {
      value: {
        read: () =>
          new Promise((resolve) => {
            window.finishRead = () =>
              resolve([
                {
                  types: ["text/plain"],
                  getType: async () => new Blob(["迟到内容"]),
                },
              ]);
          }),
      },
    });
  });
  const latePage = await lateContext.newPage();
  await latePage.goto(base);
  await latePage.waitForFunction(() => typeof window.finishRead === "function");
  await latePage.locator("#message").fill("正在编辑的草稿");
  await latePage.evaluate(async () => {
    window.finishRead();
    await new Promise(requestAnimationFrame);
    await new Promise(requestAnimationFrame);
  });
  assert.equal(
    await latePage.locator("#message").inputValue(),
    "正在编辑的草稿",
  );
  assert.equal(await latePage.locator("#candidate").isHidden(), true);
  await lateContext.close();
  console.log("PASS late clipboard reads cannot replace an active draft");

  const suggestionContext = await browser.newContext({ locale: "zh-CN" });
  await suggestionContext.addInitScript(() => {
    localStorage.setItem(
      "nclip.preferences.v1",
      JSON.stringify({ suggestions: true }),
    );
    window.clipText = "推荐原文";
    Object.defineProperty(navigator, "permissions", {
      value: { query: async () => ({ state: "granted" }) },
    });
    Object.defineProperty(navigator, "clipboard", {
      value: {
        read: async () => [
          {
            types: ["text/plain"],
            getType: async () => new Blob([window.clipText]),
          },
        ],
        writeText: async (text) => {
          window.clipText = text;
        },
      },
    });
  });
  const suggestionPage = await suggestionContext.newPage();
  await suggestionPage.goto(base);
  await suggestionPage.locator("#candidate").waitFor({ state: "visible" });
  await suggestionPage.locator("#message").press("Control+Enter");
  await suggestionPage.locator("#result").waitFor({ state: "visible" });
  const suggestedURL = await suggestionPage.locator("#share-url").inputValue();
  slugs.add(suggestedURL.split("/").pop());
  await suggestionPage
    .locator("#status")
    .filter({ hasText: "已复制" })
    .waitFor();
  await suggestionPage.locator("#new-share").click();
  await suggestionPage.evaluate(async () => {
    await new Promise(requestAnimationFrame);
    await new Promise(requestAnimationFrame);
  });
  assert.equal(await suggestionPage.locator("#candidate").isHidden(), true);
  await suggestionContext.close();
  console.log(
    "PASS visible recommendation sends with shortcut and own links stay suppressed",
  );

  const deniedContext = await browser.newContext({ locale: "zh-CN" });
  await deniedContext.addInitScript(() => {
    Object.defineProperty(navigator, "clipboard", {
      value: {
        writeText: async () => {
          throw new DOMException("Denied", "NotAllowedError");
        },
      },
    });
    document.execCommand = () => false;
  });
  const deniedPage = await deniedContext.newPage();
  await deniedPage.goto(base);
  await deniedPage.locator("#limits").filter({ hasText: "10.0 MiB" }).waitFor();
  await deniedPage.locator("#message").fill("自动复制失败仍可分享");
  await deniedPage.locator("#send").click();
  await deniedPage.locator("#result").waitFor({ state: "visible" });
  await deniedPage
    .locator("#status")
    .filter({ hasText: "未能写入剪贴板" })
    .waitFor();
  slugs.add(
    (await deniedPage.locator("#share-url").inputValue()).split("/").pop(),
  );
  assert.match(await deniedPage.locator("#result-message").innerText(), /点击/);
  assert.equal(await deniedPage.locator("#copy-link").isVisible(), true);
  await deniedContext.close();
  console.log(
    "PASS denied automatic copy retains link and never claims success",
  );

  const touchContext = await browser.newContext({
    viewport: { width: 390, height: 844 },
    isMobile: true,
    hasTouch: true,
    locale: "zh-CN",
  });
  const touchPage = await touchContext.newPage();
  await touchPage.goto(base);
  await touchPage.locator("#limits").filter({ hasText: "10.0 MiB" }).waitFor();
  await touchPage.locator("#message").fill("触摸发送");
  await touchPage.locator("#send").tap();
  await touchPage.locator("#result").waitFor({ state: "visible" });
  slugs.add(
    (await touchPage.locator("#share-url").inputValue()).split("/").pop(),
  );
  await touchPage.screenshot({
    path: `${evidence}/touch-result.png`,
    fullPage: true,
  });
  await touchContext.close();
  console.log("PASS touch-emulated primary send action");

  assert.deepEqual(errors, []);
  console.log("PASS no browser runtime errors");
} catch (error) {
  await page.screenshot({ path: `${evidence}/failure.png`, fullPage: true });
  throw error;
} finally {
  for (const slug of slugs) await context.request.delete(`${base}/${slug}`);
  await browser.close();
}
