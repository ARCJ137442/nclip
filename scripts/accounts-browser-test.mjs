// Use only an isolated accounts-enabled instance; the helper removes its own users/shares.
import { createRequire } from "node:module";
import { mkdir } from "node:fs/promises";
import assert from "node:assert/strict";
const require = createRequire(import.meta.url);
const { chromium } = require(
  process.env.NCLIP_PLAYWRIGHT_MODULE || "playwright",
);
const base = process.env.NCLIP_TEST_URL || "http://127.0.0.1:18082";
const evidence = process.env.NCLIP_EVIDENCE_DIR || "accounts-evidence";
await mkdir(evidence, { recursive: true });
const browser = await chromium.launch({
  headless: true,
  ...(process.env.NCLIP_BROWSER_CHANNEL
    ? { channel: process.env.NCLIP_BROWSER_CHANNEL }
    : {}),
});
const contexts = [],
  users = [],
  shares = [],
  errors = [];
const suffix = Date.now().toString(36),
  adminName = process.env.NCLIP_TEST_ADMIN || "admin",
  adminPassword = process.env.NCLIP_TEST_PASSWORD || "test-admin-password";
async function context() {
  const ctx = await browser.newContext({
    locale: "zh-CN",
    viewport: { width: 1100, height: 880 },
    permissions: ["clipboard-read", "clipboard-write"],
  });
  contexts.push(ctx);
  return ctx;
}
async function session(ctx) {
  return (await ctx.request.get(base + "/api/v1/session")).json();
}
async function mutate(ctx, path, method = "POST", data) {
  const state = await session(ctx);
  return ctx.request.fetch(base + path, {
    method,
    data,
    headers: { "X-CSRF-Token": state.csrf_token, Origin: base },
  });
}
async function signIn(ctx, name, password, url = "/") {
  const page = await ctx.newPage();
  page.on("pageerror", (error) => errors.push(error.message));
  await page.goto(base + url);
  await page.locator("#login-form").waitFor();
  // Setting native values without input events models password-manager autofill.
  await page.evaluate(
    ({ name, password }) => {
      document.getElementById("username").value = name;
      document.getElementById("password").value = password;
    },
    { name, password },
  );
  await Promise.all([
    page.waitForURL((u) => u.pathname !== "/login"),
    page.locator("#login-submit").click(),
  ]);
  return page;
}
async function newUser(ctx, name, permissions) {
  const result = await mutate(ctx, "/api/v1/users", "POST", {
    username: name,
    password: "fixture-user-password",
    permissions,
  });
  assert.equal(result.status(), 200, await result.text());
  const user = (await result.json()).user;
  users.push(user.id);
  return user;
}
async function send(page, publicShare = false, burn = false) {
  await page.locator("#limits").filter({ hasText: "10.0 MiB" }).waitFor();
  await page.locator("#message").fill("账号与权限分享验收");
  await page.locator("#public-share").setChecked(publicShare);
  await page.locator("#burn").setChecked(burn);
  await page.locator("#send").click();
  await page.locator("#result").waitFor({ state: "visible" });
  const url = await page.locator("#share-url").inputValue();
  shares.push(url.split("/").pop());
  return url;
}
let adminContext;
try {
  adminContext = await context();
  const adminPage = await signIn(adminContext, adminName, adminPassword);
  await adminPage.locator("#limits").filter({ hasText: "10.0 MiB" }).waitFor();
  assert.equal(await adminPage.locator("#secondary-receive").isVisible(), true);
  await adminPage.locator("#nav-admin").click();
  await adminPage.locator("#user-list article").waitFor();
  const admin = (await session(adminContext)).user;
  await adminPage
    .locator(`[data-user-id="${admin.id}"]`)
    .getByRole("button", { name: "修改", exact: true })
    .click();
  await adminPage.locator("#edit-disabled").check();
  await adminPage.locator("#save-user").click();
  await adminPage
    .locator("#user-form-error")
    .filter({ hasText: "必须保留" })
    .waitFor();
  assert.equal(await adminPage.locator("#user-dialog").isVisible(), true);
  await adminPage.locator("#close-user-dialog").click();
  console.log("PASS native login, manager UI and last-manager guard");

  await adminPage.locator("#new-user").click();
  await adminPage.locator("#edit-username").fill("upload-" + suffix);
  await adminPage.locator("#edit-password").fill("fixture-user-password");
  await adminPage.locator("#permission-upload").check();
  await adminPage.locator("#save-user").click();
  await adminPage.locator("#user-dialog").waitFor({ state: "hidden" });
  await adminPage.locator("#status").filter({ hasText: "已保存" }).waitFor();
  const uploader = (
    await (await adminContext.request.get(base + "/api/v1/users")).json()
  ).items.find((user) => user.username === "upload-" + suffix);
  assert.ok(uploader);
  users.push(uploader.id);
  const reader = await newUser(adminContext, "read-" + suffix, {
    download: true,
  });
  const manager = await newUser(adminContext, "manage-" + suffix, {
    manage: true,
  });
  const uploadContext = await context(),
    uploadPage = await signIn(
      uploadContext,
      uploader.username,
      "fixture-user-password",
    );
  await uploadPage.locator("#limits").filter({ hasText: "10.0 MiB" }).waitFor();
  assert.equal(await uploadPage.locator("#nav-download").isHidden(), true);
  assert.equal(await uploadPage.locator("#nav-admin").isHidden(), true);
  assert.equal(await uploadPage.locator("#secondary-receive").isHidden(), true);
  const privateURL = await send(uploadPage);
  assert.equal(await uploadPage.locator("#open-share").isHidden(), true);
  assert.equal(
    (await mutate(uploadContext, "/api/v1/users", "POST", {})).status(),
    403,
  );
  console.log(
    "PASS uploader-only landing, hidden download/admin actions and server denial",
  );

  const readerContext = await context(),
    readerPage = await signIn(
      readerContext,
      reader.username,
      "fixture-user-password",
      new URL(privateURL).pathname,
    );
  await readerPage.locator("#received-items article").waitFor();
  assert.equal(readerPage.url(), privateURL);
  assert.equal(await readerPage.locator("#delete-share").isHidden(), true);
  await readerPage.goto(base);
  await readerPage.locator("#receive-form").waitFor();
  assert.equal(await readerPage.locator("#nav-upload").isHidden(), true);
  await readerPage.locator("#receive-code").fill(shares[0]);
  await readerPage.locator("#open-code").click();
  await readerPage.locator("#received-items article").waitFor();
  assert.equal(
    (await mutate(readerContext, "/api/v1/shares", "POST", {})).status(),
    403,
  );
  await readerPage.screenshot({
    path: `${evidence}/reader.png`,
    fullPage: true,
  });
  console.log(
    "PASS direct-share login return, reader landing and code navigation",
  );

  const anonymous = await context();
  assert.equal(
    (
      await anonymous.request.get(base + "/api/v1/shares/" + shares[0])
    ).status(),
    401,
  );
  await uploadPage.goto(base);
  const publicURL = await send(uploadPage, true, true);
  assert.equal(await uploadPage.locator("#open-share").isVisible(), true);
  const publicPage = await anonymous.newPage();
  await publicPage.goto(publicURL);
  await publicPage.locator("#receive-gate").waitFor({ state: "visible" });
  assert.equal(
    (
      await anonymous.request.get(base + "/api/v1/shares/" + shares.at(-1))
    ).status(),
    200,
  );
  await publicPage.locator("#receive").click();
  await publicPage.locator("#received-items article").waitFor();
  assert.equal(
    (
      await anonymous.request.get(base + "/api/v1/shares/" + shares.at(-1))
    ).status(),
    404,
  );
  assert.equal(await publicPage.locator("#delete-share").isHidden(), true);
  console.log(
    "PASS protected-by-default shares and anonymous opt-in public one-time receive",
  );

  const manageContext = await context(),
    managePage = await signIn(
      manageContext,
      manager.username,
      "fixture-user-password",
    );
  await managePage.locator("#user-list article").first().waitFor();
  assert.equal(new URL(managePage.url()).pathname, "/admin");
  assert.equal(await managePage.locator("#nav-upload").isHidden(), true);
  assert.equal(await managePage.locator("#nav-download").isHidden(), true);
  await managePage.locator("#shares-tab").click();
  await managePage.locator("#share-list article").first().waitFor();
  assert.equal(await managePage.locator("#share-list a").count(), 0);
  assert.equal(
    (await manageContext.request.get(base + "/raw/" + shares[0])).status(),
    403,
  );
  await managePage.screenshot({
    path: `${evidence}/manager.png`,
    fullPage: true,
  });
  console.log(
    "PASS manage-only user/content administration without private downloads",
  );

  const current = (
    await (await adminContext.request.get(base + "/api/v1/users")).json()
  ).items.find((user) => user.id === reader.id);
  assert.equal(
    (
      await mutate(adminContext, "/api/v1/users/" + reader.id, "PUT", {
        username: reader.username,
        permissions: { download: true },
        disabled: true,
        version: current.version,
      })
    ).status(),
    200,
  );
  assert.equal(
    (
      await readerContext.request.get(base + "/api/v1/shares/" + shares[0])
    ).status(),
    401,
  );
  const stale = await mutate(
    adminContext,
    "/api/v1/users/" + reader.id,
    "PUT",
    {
      username: reader.username,
      permissions: { upload: true },
      disabled: false,
      version: current.version,
    },
  );
  assert.equal(stale.status(), 409);
  const noCSRF = await adminContext.request.post(base + "/api/v1/users", {
    data: {},
    headers: { Origin: base },
  });
  assert.equal(noCSRF.status(), 403);
  const crossOrigin = await adminContext.request.post(base + "/api/v1/login", {
    data: { username: adminName, password: adminPassword },
    headers: { Origin: "https://untrusted.invalid" },
  });
  assert.equal(crossOrigin.status(), 403);
  console.log(
    "PASS immediate revocation, conflicting account update and CSRF rejection",
  );

  await adminPage.goto(base + "/admin");
  await adminPage.locator("#user-list article").first().waitFor();
  for (const width of [320, 390]) {
    await adminPage.setViewportSize({ width, height: 850 });
    assert.equal(
      await adminPage.evaluate(
        () => document.documentElement.scrollWidth <= innerWidth,
      ),
      true,
    );
    await adminPage.screenshot({
      path: `${evidence}/admin-${width}.png`,
      fullPage: true,
    });
  }
  await adminPage.locator("#settings summary").click();
  await adminPage.locator("#language").selectOption("en");
  await adminPage.locator("#theme").selectOption("dark");
  await adminPage.keyboard.press("Escape");
  await adminPage.screenshot({
    path: `${evidence}/admin-dark-en.png`,
    fullPage: true,
  });
  assert.equal(
    await adminPage
      .getByRole("heading", { name: "Manage", exact: true })
      .count(),
    1,
  );
  await uploadPage.goto(base);
  await uploadPage.locator("#account-menu summary").click();
  await uploadPage.locator("#logout").click();
  await uploadPage.locator("#login-form").waitFor();
  assert.equal(
    (await uploadContext.request.get(base + "/api/v1/ui-config")).status(),
    401,
  );
  const loginResponse = await anonymous.request.get(base + "/login");
  assert.equal(loginResponse.status(), 200);
  assert.equal(loginResponse.headers()["www-authenticate"], undefined);
  assert.deepEqual(errors, []);
  console.log(
    "PASS narrow layouts, English/dark UI, logout and no Basic Auth challenge",
  );
} catch (error) {
  for (const ctx of contexts) {
    const page = ctx.pages().at(-1);
    if (page && !page.isClosed()) {
      await page
        .screenshot({
          path: `${evidence}/failure-${contexts.indexOf(ctx)}.png`,
          fullPage: true,
        })
        .catch(() => {});
    }
  }
  throw error;
} finally {
  if (adminContext) {
    for (const slug of shares) await mutate(adminContext, "/" + slug, "DELETE");
    for (const id of users)
      await mutate(adminContext, "/api/v1/users/" + id, "DELETE");
  }
  await browser.close();
}
