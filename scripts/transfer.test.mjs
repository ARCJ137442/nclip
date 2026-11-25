import test from "node:test";
import assert from "node:assert/strict";

import {
  percentOf,
  timeParts,
  createProgress,
  transferStages,
} from "../static/transfer.js";
import {
  requestStream,
  requestBytes,
  RequestError,
  CHANNEL_UPLOAD_CHUNK_BYTES,
  createChannelUploadState,
  uploadChannelContent,
  cleanupCanceledChannelUpload,
} from "../static/share-api.js";
import { canKickSeat, recipientHint, captureSeatTarget, isSameSeatTarget } from "../static/channel-workspace-state.js";
import { resolveChannelSession } from "../static/channel-session-state.js";
import { CHANNEL_BROWSER_DOWNLOAD_THRESHOLD_BYTES, shouldUseBrowserDownload, validateChannelDownloadURL, dispatchBrowserDownload } from "../static/channel-transfer-path.js";
import { clampTTLHours, previewForItem, safePreview } from "../static/handoff.js";
import { normalizePreferences, preferences } from "../static/preferences.js";
import { receiverTerminalMessage } from "../static/channel-receiver-state.js";
import { t } from "../static/i18n.js";
import { uploadAuthMessage } from "../static/upload-auth-state.js";

test("released recipients retain visible sender control and their seat target", () => {
  const seat = { seat_id: "seat-a", identity: "anonymous", joined_at: "2026-01-01", state: "released" };
  assert.equal(canKickSeat(seat), true);
  assert.equal(recipientHint(seat), "channelReceiverRecovering");
  assert.equal(isSameSeatTarget({ ...seat, state: "paired" }, captureSeatTarget(seat)), true);
  assert.equal(canKickSeat({ ...seat, state: "pending_admit" }), false);
  assert.equal(canKickSeat(null), false);
  assert.equal(isSameSeatTarget({ ...seat, seat_id: "seat-b" }, captureSeatTarget(seat)), false);
});

test("upload authentication feedback requires an actual upload-key challenge", () => {
  assert.equal(uploadAuthMessage(401, "missing api key", true), "uploadKeyRequired");
  assert.equal(uploadAuthMessage(401, "unauthorized", true), "uploadKeyInvalid");
  for (const [status, code, field] of [[503, "unauthorized", true], [403, "missing api key", true], [401, "unauthorized", false], [401, "login_required", true]])
    assert.equal(uploadAuthMessage(status, code, field), null);
  const previous = preferences.language;
  try {
    preferences.language = "zh";
    assert.match(t("uploadKeyRequired"), /API Key/);
    assert.match(t("uploadKeyInvalid"), /更正/);
    preferences.language = "en";
    assert.match(t("uploadKeyRequired"), /Enter/);
    assert.match(t("uploadKeyInvalid"), /invalid/);
  } finally { preferences.language = previous; }
});

test("receiver rejection requires the exact terminal status, code and reason", () => {
  for (const [status, body, expected] of [
    [410, { error: "join_closed", reason: "sender_rejected" }, "channelRejected"],
    [403, { error: "join_closed", reason: "sender_rejected" }, "channelReceiveGone"],
    [410, { error: "join_closed" }, "channelReceiveGone"],
    [410, { error: "join_closed", reason: "unknown" }, "channelReceiveGone"],
    [410, { error: "pending_expired", reason: "sender_rejected" }, "channelPendingExpired"],
    [410, { error: "delivery_complete", reason: "sender_rejected" }, "channelReceiveGone"],
    [410, { error: "receiver_kicked" }, "channelReceiveGone"],
  ]) assert.equal(receiverTerminalMessage(status, body), expected);
  const previous = preferences.language;
  try {
    preferences.language = "zh";
    assert.equal(t("channelRejected"), "发送者拒绝了本次加入。");
    preferences.language = "en";
    assert.equal(t("channelRejected"), "The sender rejected this join.");
  } finally { preferences.language = previous; }
});

test("percentOf needs a total and clamps the range", () => {
  assert.equal(percentOf(0, 0), null);
  assert.equal(percentOf(50, -1), null);
  assert.equal(percentOf(0, 100), 0);
  assert.equal(percentOf(25, 100), 25);
  assert.equal(percentOf(150, 100), 100);
});

test("timeParts chooses seconds below a minute", () => {
  assert.deepEqual(timeParts(0.4), { unit: "seconds", count: 1 });
  assert.deepEqual(timeParts(30), { unit: "seconds", count: 30 });
  assert.deepEqual(timeParts(61), { unit: "minutes", count: 2 });
  assert.equal(timeParts(-1), null);
  assert.equal(timeParts(Number.NaN), null);
});

test("TTL selection accepts only integer hours and clamps to server bounds", () => {
  assert.equal(clampTTLHours(24), 24);
  assert.equal(clampTTLHours("168", 1, 168), 168);
  assert.equal(clampTTLHours(0), 1);
  assert.equal(clampTTLHours(200), 168);
  assert.equal(clampTTLHours(1.5), null);
  assert.equal(clampTTLHours("bad"), null);
});

test("preferences validate sticky share settings without persisting content", () => {
  assert.deepEqual(
    normalizePreferences({ burn: true, public: true, ttlHours: 72 }),
    { language: "system", theme: "system", suggestions: false, burn: true, public: true, ttlHours: 72 },
  );
  assert.deepEqual(
    normalizePreferences({ burn: "true", public: 1, ttlHours: 2.5, message: "secret" }),
    { language: "system", theme: "system", suggestions: false, burn: false, public: false, ttlHours: 24 },
  );
});

test("channel upload manifest records stable part keys and declared byte sizes", () => {
  let nextID = 0;
  const state = createChannelUploadState([
    {
      kind: "clipboard",
      name: "note",
      parts: [
        { mime: "text/plain", blob: new Blob(["hello"]) },
        { mime: "text/html", blob: new Blob(["<b>hello</b>"]) },
      ],
    },
  ], () => `request-${++nextID}`);
  assert.deepEqual(state.manifest, {
    version: 1,
    items: [{
      kind: "clipboard",
      name: "note",
      parts: [
        { key: "p0", mime: "text/plain", size: 5 },
        { key: "p1", mime: "text/html", size: 12 },
      ],
    }],
  });
  assert.equal(state.totalBytes, 17);
  assert.equal(state.initRequestID, "request-1");
  assert.equal(state.finalizeRequestID, "request-2");
  assert.equal(CHANNEL_UPLOAD_CHUNK_BYTES, 4 * 1024 * 1024);
});

test("channel chunk retries replay the same offset and digest without duplicate bytes", async () => {
  const originalXHR = globalThis.XMLHttpRequest;
  const accepted = new Set();
  const chunkRequests = [];
  let lostResponse = true;
  const fetchCalls = [];

  class FakeXMLHttpRequest {
    constructor() {
      this.listeners = new Map();
      this.upload = { listeners: new Map(), addEventListener: (name, listener) => this.upload.listeners.set(name, listener) };
      this.requestHeaders = new Headers();
      this.responseHeaders = new Headers();
      this.status = 0;
      this.responseText = "";
    }
    open(method, url) { this.method = method; this.url = url; }
    setRequestHeader(name, value) { this.requestHeaders.set(name, value); }
    addEventListener(name, listener) { this.listeners.set(name, listener); }
    getResponseHeader(name) { return this.responseHeaders.get(name); }
    dispatch(target, name, event = {}) { target.get(name)?.(event); }
    send(blob) {
      const offset = this.requestHeaders.get("Upload-Offset");
      const hash = this.requestHeaders.get("X-Chunk-SHA256");
      const replayKey = `${offset}:${hash}`;
      const replayed = accepted.has(replayKey);
      accepted.add(replayKey);
      chunkRequests.push({ offset, hash, replayed, size: blob.size });
      queueMicrotask(() => {
        this.dispatch(this.upload.listeners, "progress", { loaded: blob.size, lengthComputable: true });
        if (lostResponse) {
          lostResponse = false;
          this.dispatch(this.listeners, "error");
          return;
        }
        this.status = 204;
        this.responseHeaders.set("X-Chunk-Replayed", String(replayed));
        this.dispatch(this.listeners, "load");
      });
    }
    abort() { this.dispatch(this.listeners, "abort"); }
  }

  globalThis.XMLHttpRequest = FakeXMLHttpRequest;
  try {
    const state = createChannelUploadState([
      { kind: "file", name: "payload.bin", parts: [{ mime: "application/octet-stream", blob: new Blob(["payload"]) }] },
    ], (() => { let id = 0; return () => `request-${++id}`; })());
    await withFetch(async (url, options = {}) => {
      fetchCalls.push({ url, method: options.method || "GET", headers: new Headers(options.headers || {}) });
      if (url.endsWith("/finalize"))
        return new Response(JSON.stringify({ state: "held" }), { status: 201, headers: { "Content-Type": "application/json" } });
      return new Response(JSON.stringify({ upload_id: "upload-session" }), { status: 201, headers: { "Content-Type": "application/json" } });
    }, async () => {
      await assert.rejects(
        uploadChannelContent("channel-id", state, { "X-Api-Key": "test" }),
        (error) => error instanceof RequestError && error.message === "network",
      );
      assert.equal(state.uploadID, "upload-session");
      assert.equal(state.offsets.get("p0"), 0);
      const completed = await uploadChannelContent("channel-id", state, { "X-Api-Key": "test" });
      assert.deepEqual(completed, { state: "held" });
    });

    assert.equal(fetchCalls.filter((call) => call.method === "POST" && call.url.endsWith("/content")).length, 1);
    assert.equal(fetchCalls.filter((call) => call.url.endsWith("/finalize")).length, 1);
    assert.equal(chunkRequests.length, 2);
    assert.equal(chunkRequests[0].offset, "0");
    assert.equal(chunkRequests[1].offset, "0");
    assert.equal(chunkRequests[0].hash, chunkRequests[1].hash);
    assert.equal(chunkRequests[0].replayed, false);
    assert.equal(chunkRequests[1].replayed, true);
    assert.equal(state.offsets.get("p0"), 7);
  } finally {
    globalThis.XMLHttpRequest = originalXHR;
  }
});

test("channel init and finalize fetch cancellation keep the aborted result", async () => {
  const initState = createChannelUploadState([], () => "init-key");
  await withFetch(async () => { throw new DOMException("cancelled", "AbortError"); }, async () => {
    await assert.rejects(uploadChannelContent("c", initState), (error) => error instanceof RequestError && error.message === "aborted");
  });

  const finalizeState = createChannelUploadState([], (() => { let i = 0; return () => `key-${++i}`; })());
  await withFetch(async (url) => {
    if (url.endsWith("/finalize")) throw new DOMException("cancelled", "AbortError");
    return new Response(JSON.stringify({ upload_id: "upload-1" }), { status: 201, headers: { "Content-Type": "application/json" } });
  }, async () => {
    await assert.rejects(uploadChannelContent("c", finalizeState), (error) => error instanceof RequestError && error.message === "aborted");
  });
  assert.equal(finalizeState.uploadID, "upload-1");
});

test("channel init and finalize response-body cancellation also stays aborted", async () => {
  const abortingJSON = (signal, value, onStarted = () => {}) => ({
    ok: true,
    status: 201,
    json: () => new Promise((resolve, reject) => {
      onStarted();
      if (value !== undefined) { resolve(value); return; }
      signal.addEventListener("abort", () => reject(new DOMException("cancelled", "AbortError")), { once: true });
    }),
  });
  const initState = createChannelUploadState([], () => "init-body-key");
  const initController = new AbortController();
  let initBodyStarted;
  const initBodyPending = new Promise((resolve) => { initBodyStarted = resolve; });
  await withFetch(async (_url, options) => abortingJSON(options.signal, undefined, initBodyStarted), async () => {
    const pending = uploadChannelContent("c", initState, {}, { signal: initController.signal });
    await initBodyPending;
    initController.abort();
    await assert.rejects(pending, (error) => error instanceof RequestError && error.message === "aborted");
  });

  const finalizeState = createChannelUploadState([], (() => { let i = 0; return () => `body-${++i}`; })());
  const finalizeController = new AbortController();
  let finalizeBodyStarted;
  let deletes = 0;
  const finalizeBodyPending = new Promise((resolve) => { finalizeBodyStarted = resolve; });
  await withFetch(async (url, options = {}) => {
    if (options.method === "DELETE") { deletes++; return new Response(null, { status: 204 }); }
    return url.endsWith("/finalize")
      ? abortingJSON(options.signal, undefined, finalizeBodyStarted)
      : abortingJSON(options.signal, { upload_id: "upload-body" });
  }, async () => {
    const pending = uploadChannelContent("c", finalizeState, {}, { signal: finalizeController.signal });
    await finalizeBodyPending;
    finalizeController.abort();
    await assert.rejects(pending, (error) => error instanceof RequestError && error.message === "aborted");
    assert.equal(await cleanupCanceledChannelUpload("c", finalizeState), true);
  });
  assert.equal(finalizeState.uploadID, "upload-body");
  assert.equal(deletes, 1);
});

test("cancel cleanup confirms deletion only after a terminal server response", async () => {
  const state = createChannelUploadState([], () => "same-init-key");
  let deletes = 0;
  await withFetch(async (url, options = {}) => {
    if (options.method === "DELETE") { deletes++; return new Response(null, { status: 204 }); }
    return new Response(JSON.stringify({ upload_id: "session-1" }), { status: 201, headers: { "Content-Type": "application/json" } });
  }, async () => assert.equal(await cleanupCanceledChannelUpload("c", state), true));
  assert.equal(deletes, 1);

  await withFetch(async (url, options = {}) => {
    if (options.method === "DELETE") return new Response(JSON.stringify({ error: "network" }), { status: 503, headers: { "Content-Type": "application/json" } });
    return new Response(JSON.stringify({ upload_id: "session-2" }), { status: 201, headers: { "Content-Type": "application/json" } });
  }, async () => assert.equal(await cleanupCanceledChannelUpload("c", { ...state, uploadID: "session-2" }), false));

  const terminalCases = [
    [404, "channel_not_found", true],
    [410, "content_upload_closed", true],
    [503, "content_upload_closed", false],
    [403, "channel_not_found", false],
    [404, "content_upload_closed", false],
    [410, "channel_not_found", false],
    [410, "content_closed", false],
  ];
  for (const [status, code, expected] of terminalCases) {
    await withFetch(async () => new Response(JSON.stringify({ error: code }), {
      status,
      headers: { "Content-Type": "application/json" },
    }), async () => assert.equal(
      await cleanupCanceledChannelUpload("c", { ...state, uploadID: `session-${status}-${code}` }),
      expected,
      `HTTP ${status} ${code}`,
    ));
  }

  await withFetch(async () => { throw new TypeError("offline"); }, async () => {
    assert.equal(await cleanupCanceledChannelUpload("c", { ...state, uploadID: "session-network" }), false);
  });

  const initCanceledState = createChannelUploadState([], () => "closed-init-key");
  await withFetch(async () => new Response(JSON.stringify({ error: "content_upload_closed" }), {
    status: 410,
    headers: { "Content-Type": "application/json" },
  }), async () => assert.equal(await cleanupCanceledChannelUpload("c", initCanceledState), true));

  await withFetch(async () => new Response(JSON.stringify({ error: "channel_not_found" }), {
    status: 404,
    headers: { "Content-Type": "application/json" },
  }), async () => assert.equal(await cleanupCanceledChannelUpload("c", initCanceledState), true));

});

test("kick confirmation target cannot transfer to a replacement seat", () => {
  const first = { state: "paired", seat_id: "seat-a", identity: "alice", joined_at: "2026-11-25T00:00:00Z" };
  const target = captureSeatTarget(first);
  assert.equal(isSameSeatTarget(first, target), true);
  assert.equal(isSameSeatTarget({ ...first, seat_id: "seat-b", identity: "bob" }, target), false);
  assert.equal(captureSeatTarget({ ...first, state: "pending_admit" }), null);
});

test("failed session checks stay unknown and successful retry restores the ready state", async () => {
  for (const failure of [new TypeError("offline"), new Response("unauthorized", { status: 401 }), new Response("unavailable", { status: 503 })]) {
    const state = await resolveChannelSession(async () => { if (failure instanceof Error) throw failure; return failure; });
    assert.equal(state.status, "error");
    assert.equal(state.enabled, false);
    assert.equal(state.error, failure instanceof Response && failure.status === 401 ? "loginRequired" : "channelSessionUnavailable");
  }
  const recovered = await resolveChannelSession(async () => new Response(JSON.stringify({ enabled: true, csrf_token: "csrf" }), { status: 200, headers: { "Content-Type": "application/json" } }));
  assert.deepEqual(recovered, { enabled: true, csrf_token: "csrf", status: "ready" });
});

test("browser download selection and ticket URL stay bounded to the same channel origin", () => {
  assert.equal(shouldUseBrowserDownload(CHANNEL_BROWSER_DOWNLOAD_THRESHOLD_BYTES - 1), false);
  assert.equal(shouldUseBrowserDownload(CHANNEL_BROWSER_DOWNLOAD_THRESHOLD_BYTES), true);
  assert.equal(shouldUseBrowserDownload(1, false), true);
  const channel = "/api/v1/channels/channel-id";
  const page = "https://clip.test/c/channel-id";
  assert.equal(validateChannelDownloadURL(`${channel}/content/download/ticket`, channel, page), true);
  assert.equal(validateChannelDownloadURL("https://attacker.test/download/ticket", channel, page), false);
  assert.equal(validateChannelDownloadURL(`${channel}/content/download/ticket?leak=1`, channel, page), false);
  assert.equal(validateChannelDownloadURL(`${channel}/content/download-evil/ticket`, channel, page), false);
});

test("sender preview normalizes lines, truncates by rune, and suppresses secrets", () => {
  const preview = safePreview("第一行\r\n第二行\t还有内容和尾部");
  assert.equal(preview, "第一行 第二行 还有内容和尾部");
  assert.ok(Array.from(preview).length <= 24);
  assert.equal(Array.from(safePreview("界".repeat(30))).length, 24);
  for (const secret of [
    "s" + "k-live-secret",
    "ghp" + "_1234567890",
    "github_pat" + "_1234567890",
    "A" + "KIA1234567890123456",
    "-----BEGIN " + "PRIVATE KEY-----",
    "AbCdEf0123456789AbCdEf0123456789",
  ]) assert.equal(safePreview(secret), "");
});

test("sender preview matches the server clipboard-only rule", async () => {
  const plain = { mime: "text/plain", blob: new Blob(["hello\nrecipient"]) };
  assert.equal(await previewForItem({ kind: "clipboard", parts: [plain] }), "hello recipient");
  assert.equal(await previewForItem({ kind: "file", parts: [plain] }), "");
  assert.equal(await previewForItem({ kind: "clipboard", parts: [{ mime: "text/html", blob: new Blob(["<b>secret</b>"]) }] }), "");
});

test("createProgress samples the rate and estimates the remainder", () => {
  let now = 0;
  const clock = createProgress(() => now);
  clock.begin();
  now = 1000;
  let view = clock.update(100, 1000);
  assert.equal(view.percent, 10);
  assert.ok(Math.abs(view.rate - 100) < 1e-9);
  assert.ok(Math.abs(view.eta - 9) < 1e-9);
  now = 2000;
  view = clock.update(600, 1000);
  assert.equal(view.percent, 60);
  assert.ok(Math.abs(view.rate - 260) < 1e-9);
  assert.ok(Math.abs(view.eta - 400 / 260) < 1e-9);
  now = 4000;
  view = clock.update(1000, 1000);
  assert.equal(view.percent, 100);
  assert.equal(view.eta, 0);
});

test("unknown totals never produce a percentage or an ETA", () => {
  let now = 0;
  const clock = createProgress(() => now);
  clock.begin();
  now = 1000;
  const view = clock.update(700, 0);
  assert.equal(view.percent, null);
  assert.equal(view.eta, null);
});

test("transfers shorter than the sample window invent no rate", () => {
  let now = 0;
  const clock = createProgress(() => now);
  clock.begin();
  now = 100;
  const view = clock.update(500, 500);
  assert.equal(view.percent, 100);
  assert.equal(view.rate, 0);
  assert.equal(view.eta, 0);
});

test("transfer stages separate a server rejection from a lost response", () => {
  assert.deepEqual(transferStages("uploading", { fullySent: false }), {
    preparing: "done",
    uploading: "active",
    saving: "pending",
    complete: "pending",
  });
  assert.deepEqual(
    transferStages("error", { fullySent: true, status: 413 }),
    {
      preparing: "done",
      uploading: "done",
      saving: "error",
      complete: "pending",
    },
  );
  assert.deepEqual(
    transferStages("error", { fullySent: true, status: 0 }),
    {
      preparing: "done",
      uploading: "done",
      saving: "unknown",
      complete: "pending",
    },
  );
  assert.deepEqual(transferStages("canceled", { fullySent: false }), {
    preparing: "done",
    uploading: "canceled",
    saving: "pending",
    complete: "pending",
  });
});

// The download path must count the stream it forwards, not buffer a copy.
async function withFetch(stub, run) {
  const original = globalThis.fetch;
  globalThis.fetch = stub;
  try {
    return await run();
  } finally {
    globalThis.fetch = original;
  }
}

function bodyResponse(chunks, headers, status = 200) {
  return new Response(
    new ReadableStream({
      start(controller) {
        for (const chunk of chunks) controller.enqueue(chunk);
        controller.close();
      },
    }),
    { status, headers },
  );
}

test("requestStream reports received bytes against the declared length", async () => {
  const seen = [];
  await withFetch(
    async () =>
      bodyResponse(
        [new Uint8Array([1, 2, 3]), new Uint8Array([4, 5])],
        { "Content-Length": "5", "Content-Type": "application/octet-stream" },
      ),
    async () => {
      const response = await requestStream(
        "/x",
        {},
        { onProgress: (progress) => seen.push(progress) },
      );
      const body = new Uint8Array(await response.arrayBuffer());
      assert.deepEqual([...body], [1, 2, 3, 4, 5]);
      assert.equal(response.headers.get("Content-Length"), null);
    },
  );
  assert.equal(seen[0].received, 0);
  assert.ok(seen.every((progress) => progress.total === 5));
  assert.equal(seen.at(-1).received, 5);
});

test("requestStream never trusts a length that describes encoded bytes", async () => {
  const seen = [];
  await withFetch(
    async () =>
      bodyResponse([new Uint8Array([1, 2, 3, 4])], {
        "Content-Length": "99",
        "Content-Encoding": "gzip",
      }),
    async () => {
      const response = await requestStream(
        "/x",
        {},
        { onProgress: (progress) => seen.push(progress) },
      );
      await response.arrayBuffer();
    },
  );
  assert.ok(seen.every((progress) => progress.total === 0));
  assert.equal(seen.at(-1).received, 4);
});

test("requestStream maps an abort and an HTTP error to RequestError", async () => {
  await withFetch(
    async () => {
      throw new DOMException("The operation was aborted.", "AbortError");
    },
    async () => {
      await assert.rejects(
        requestStream("/x", {}, { onProgress: () => {} }),
        (error) =>
          error instanceof RequestError &&
          error.status === 0 &&
          error.message === "aborted",
      );
    },
  );
  await withFetch(
    async () =>
      new Response(JSON.stringify({ error: "not_found" }), {
        status: 404,
        headers: { "Content-Type": "application/json" },
      }),
    async () => {
      await assert.rejects(
        requestStream("/x", {}, { onProgress: () => {} }),
        (error) =>
          error instanceof RequestError &&
          error.status === 404 &&
          error.message === "not_found",
      );
    },
  );
});

test("requestBytes resumes from retained bytes with a Range request", async () => {
  const seen = [];
  const chunks = [];
  let requestHeaders;
  await withFetch(
    async (_url, options) => {
      requestHeaders = new Headers(options.headers);
      return bodyResponse(
        [new Uint8Array([3, 4, 5]), new Uint8Array([6])],
        {
          "Content-Length": "4",
          "Content-Range": "bytes 2-5/6",
          "Accept-Ranges": "bytes",
          "Content-Type": "application/octet-stream",
        },
        206,
      );
    },
    async () => {
      const result = await requestBytes(
        "/x",
        {},
        {
          start: 2,
          onChunk: (chunk) => chunks.push(chunk),
          onProgress: (p) => seen.push(p),
          onReset: () => assert.fail("server unexpectedly reset"),
        },
      );
      assert.deepEqual(result, {
        received: 6,
        total: 6,
        resumable: true,
        contentType: "application/octet-stream",
      });
    },
  );
  assert.equal(requestHeaders.get("Range"), "bytes=2-");
  assert.equal(seen[0].received, 2);
  assert.equal(seen.at(-1).received, 6);
  assert.deepEqual(
    chunks.flatMap((chunk) => [...chunk]),
    [3, 4, 5, 6],
  );
});

test("requestBytes discards retained bytes when Range is ignored", async () => {
  let reset = 0;
  const chunks = [];
  await withFetch(
    async () =>
      bodyResponse([new Uint8Array([1, 2, 3, 4])], {
        "Accept-Ranges": "bytes",
        "Content-Length": "4",
      }),
    async () => {
      const result = await requestBytes(
        "/x",
        {},
        {
          start: 2,
          onChunk: (chunk) => chunks.push(chunk),
          onReset: () => reset++,
        },
      );
      assert.equal(result.received, 4);
    },
  );
  assert.equal(reset, 1);
  assert.deepEqual(
    chunks.flatMap((chunk) => [...chunk]),
    [1, 2, 3, 4],
  );
});

test("requestBytes rejects mismatched ranges and truncated bodies", async () => {
  await withFetch(
    async () =>
      bodyResponse([new Uint8Array([1])], {
        "Content-Range": "bytes 0-0/4",
        "Accept-Ranges": "bytes",
      }, 206),
    async () => {
      await assert.rejects(
        requestBytes("/x", {}, { start: 2 }),
        (error) => error instanceof RequestError && error.message === "range_mismatch",
      );
    },
  );
  await withFetch(
    async () =>
      bodyResponse([new Uint8Array([1])], {
        "Accept-Ranges": "bytes",
        "Content-Length": "4",
      }),
    async () => {
      await assert.rejects(
        requestBytes("/x", {}, {}),
        (error) => error instanceof RequestError && error.message === "incomplete",
      );
    },
  );
});

test("native download uses download intent and leaves the receiver page loaded", () => {
  const calls = [];
  const link = { click() { calls.push("click"); }, remove() { calls.push("remove"); } };
  const document = { createElement(tag) { assert.equal(tag, "a"); return link; }, body: { append(value) { assert.equal(value, link); calls.push("append"); } } };
  dispatchBrowserDownload("https://example.com/api/v1/channels/test/content/download/ticket", document);
  assert.equal(link.download, "");
  assert.equal(link.target, "_blank");
  assert.equal(link.rel, "noopener noreferrer");
  assert.deepEqual(calls, ["append", "click", "remove"]);
  calls.length = 0;
  link.click = () => { calls.push("click"); throw Error("host failure"); };
  assert.throws(() => dispatchBrowserDownload("https://example.com/download", document), /host failure/);
  assert.deepEqual(calls, ["append", "click", "remove"]);
});
