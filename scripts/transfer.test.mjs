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
} from "../static/share-api.js";
import { clampTTLHours, previewForItem, safePreview } from "../static/handoff.js";
import { normalizePreferences } from "../static/preferences.js";

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
