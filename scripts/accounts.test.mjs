import test from "node:test";
import assert from "node:assert/strict";
import { shareCode } from "../static/share-code.js";
import { access, can, csrfHeaders } from "../static/access.js";
import { request } from "../static/share-api.js";
const origin = "https://clip.example";

test("share input stays within the clipboard site", () => {
  for (const value of [
    " abcde ",
    "https://clip.example/ABCDE",
    "https://clip.example/raw/ABCDE",
    "/ABCDE",
  ])
    assert.equal(shareCode(value, origin), "ABCDE");
  for (const value of [
    "https://other.example/ABCDE",
    "//other.example/ABCDE",
    "javascript:alert(1)",
    "https://clip.example/login?next=ABCDE",
    "ABCDE extra",
    "../admin",
    "https://clip.example/ABCDE#fragment",
  ])
    assert.equal(shareCode(value, origin), null);
});

test("manage does not imply transfer permissions and CSRF accompanies mutations", async () => {
  Object.assign(access, {
    enabled: true,
    user: { permissions: { manage: true } },
    csrf_token: "csrf-fixture",
  });
  assert.equal(can("manage"), true);
  assert.equal(can("upload"), false);
  assert.equal(can("download"), false);
  assert.deepEqual(csrfHeaders(), { "X-CSRF-Token": "csrf-fixture" });
  let sent;
  globalThis.fetch = async (url, options) => {
    sent = options;
    return new Response("{}", { status: 200 });
  };
  await request("/api/v1/users", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: "{}",
  });
  assert.equal(sent.headers.get("X-CSRF-Token"), "csrf-fixture");
  assert.equal(sent.headers.get("Content-Type"), "application/json");
  await request("/api/v1/users");
  assert.equal(sent.headers.has("X-CSRF-Token"), false);
  Object.assign(access, { enabled: false, user: null, csrf_token: "" });
});
