import test from "node:test";
import assert from "node:assert/strict";

import {
  percentOf,
  timeParts,
  createProgress,
  transferStages,
} from "../static/transfer.js";
import { requestStream, RequestError } from "../static/share-api.js";

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
