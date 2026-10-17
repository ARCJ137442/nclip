import test from "node:test";
import assert from "node:assert/strict";
import {
  readClipboard,
  copyItem,
  pastedItems,
  textItem,
  fileItem,
} from "../static/clipboard.js";
import { encodeShare, decodeShare, request } from "../static/share-api.js";

function browser(navigator) {
  globalThis.window = { isSecureContext: true };
  Object.defineProperty(window, "ClipboardItem", {
    get: () => globalThis.ClipboardItem,
  });
  globalThis.document = { hasFocus: () => true, visibilityState: "visible" };
  Object.defineProperty(globalThis, "navigator", {
    configurable: true,
    value: navigator,
  });
}

test("automatic probe never requests clipboard read without an existing grant", async () => {
  let reads = 0;
  browser({
    permissions: { query: async () => ({ state: "prompt" }) },
    clipboard: {
      read: async () => {
        reads++;
        return [];
      },
    },
  });
  assert.deepEqual(await readClipboard({ automatic: true }), []);
  assert.equal(reads, 0);
  navigator.permissions.query = async () => {
    throw new TypeError("unsupported permission");
  };
  assert.deepEqual(await readClipboard({ automatic: true }), []);
  assert.equal(reads, 0);
});

test("one clipboard object preserves plain, HTML and image representations", async () => {
  const blobs = {
    "text/plain": new Blob(["hello"]),
    "text/html": new Blob(["<b>hello</b>"]),
    "image/png": new Blob(["png"]),
  };
  browser({
    clipboard: {
      read: async () => [
        { types: Object.keys(blobs), getType: async (type) => blobs[type] },
      ],
    },
  });
  const items = await readClipboard();
  assert.equal(items.length, 1);
  assert.deepEqual(
    items[0].parts.map((p) => p.mime),
    Object.keys(blobs),
  );
});

test("real pasted files take precedence over the duplicate HTML representation", () => {
  const file = new File(["bytes"], "report.txt", { type: "text/plain" });
  const items = pastedItems({ files: [file], getData: () => "<img>" });
  assert.equal(items.length, 1);
  assert.equal(items[0].kind, "file");
  assert.equal(items[0].parts[0].blob, file);
});

test("clipboard write failures never report a successful rich copy", async () => {
  browser({
    clipboard: {
      write: async () => {
        throw new Error("denied");
      },
      writeText: async () => {
        throw new Error("denied");
      },
    },
  });
  globalThis.ClipboardItem = class {
    static supports() {
      return true;
    }
  };
  const item = {
    kind: "clipboard",
    parts: [{ mime: "image/png", blob: new Blob(["png"]) }],
  };
  await assert.rejects(copyItem(item), /denied/);
});

test("rich copy explicitly reports plain-text fallback", async () => {
  let copied;
  browser({
    clipboard: {
      write: async () => {
        throw new Error("unsupported");
      },
      writeText: async (text) => {
        copied = text;
      },
    },
  });
  globalThis.ClipboardItem = class {
    static supports() {
      return true;
    }
  };
  assert.deepEqual(await copyItem(textItem("  中文\n", "<b>中文</b>")), {
    plain: true,
  });
  assert.equal(copied, "  中文\n");
});

test("multipart round trip preserves filenames, alternate formats and exact whitespace", async () => {
  const original = [
    textItem("  中文\n", "<b>中文</b>"),
    fileItem(new File(["file bytes"], "报告.txt", { type: "text/plain" })),
  ];
  const form = encodeShare(original);
  const manifest = JSON.parse(form.get("manifest"));
  manifest.encoding = "zip-v1";
  for (const item of manifest.items)
    for (const part of item.parts) part.size = form.get(part.key).size;
  form.set("manifest", JSON.stringify(manifest));
  const decoded = await decodeShare(new Response(form));
  assert.equal(decoded.items.length, 2);
  assert.equal(decoded.items[1].name, "报告.txt");
  assert.equal(await decoded.items[0].parts[0].blob.text(), "  中文\n");
  assert.equal(await decoded.items[0].parts[1].blob.text(), "<b>中文</b>");
});

test("request does not retry an unknown upload outcome or expose proxy HTML", async () => {
  let attempts = 0;
  globalThis.fetch = async () => {
    attempts++;
    throw new TypeError("offline");
  };
  await assert.rejects(
    request("/api/v1/shares", { method: "POST" }),
    (e) => e.status === 0 && e.message === "network",
  );
  assert.equal(attempts, 1);
  globalThis.fetch = async () =>
    new Response("<html>proxy</html>", { status: 413 });
  await assert.rejects(
    request("/api/v1/shares"),
    (e) => e.status === 413 && e.message === "failed",
  );
});
