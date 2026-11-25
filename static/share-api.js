import { csrfHeaders } from "./access.js";

export const CHANNEL_UPLOAD_CHUNK_BYTES = 4 * 1024 * 1024;

function requestID() {
  const bytes = crypto.getRandomValues(new Uint8Array(16));
  let binary = "";
  for (const byte of bytes) binary += String.fromCharCode(byte);
  return btoa(binary).replaceAll("+", "-").replaceAll("/", "_").replace(/=+$/, "");
}

export function createChannelUploadState(items, makeRequestID = requestID) {
  let index = 0;
  const parts = [];
  const manifest = {
    version: 1,
    items: items.map((item) => ({
      kind: item.kind,
      name: item.name,
      parts: item.parts.map((part) => {
        const key = `p${index++}`;
        parts.push({ key, blob: part.blob, mime: part.mime });
        return { key, mime: part.mime, size: part.blob.size };
      }),
    })),
  };
  return {
    manifest,
    parts,
    offsets: new Map(parts.map((part) => [part.key, 0])),
    initRequestID: makeRequestID(),
    finalizeRequestID: makeRequestID(),
    uploadID: "",
    totalBytes: parts.reduce((sum, part) => sum + part.blob.size, 0),
  };
}

async function digestHex(blob) {
  const digest = new Uint8Array(await crypto.subtle.digest("SHA-256", await blob.arrayBuffer()));
  return Array.from(digest, (byte) => byte.toString(16).padStart(2, "0")).join("");
}

function uploadChannelChunk(url, blob, headers, { onProgress, signal }) {
  return new Promise((resolve, reject) => {
    if (signal?.aborted) {
      reject(new RequestError(0, "aborted"));
      return;
    }
    const xhr = new XMLHttpRequest();
    let settled = false;
    const finish = (callback, value) => {
      if (settled) return;
      settled = true;
      signal?.removeEventListener("abort", abort);
      callback(value);
    };
    const abort = () => xhr.abort();
    xhr.open("PUT", url);
    xhr.withCredentials = true;
    for (const [key, value] of headers) xhr.setRequestHeader(key, value);
    xhr.upload.addEventListener("progress", (event) => {
      onProgress?.(event.lengthComputable ? Math.min(event.loaded, blob.size) : 0);
    });
    xhr.addEventListener("load", () => {
      if (xhr.status >= 200 && xhr.status < 300) {
        finish(resolve, { replayed: xhr.getResponseHeader("X-Chunk-Replayed") === "true" });
        return;
      }
      let code = "failed";
      try { code = JSON.parse(xhr.responseText).error || code; } catch { /* Proxy errors may be HTML. */ }
      finish(reject, new RequestError(xhr.status, code));
    });
    xhr.addEventListener("error", () => finish(reject, new RequestError(0, "network")));
    xhr.addEventListener("abort", () => finish(reject, new RequestError(0, "aborted")));
    signal?.addEventListener("abort", abort, { once: true });
    xhr.send(blob);
  });
}

// The session object is retained only in the current page. Each accepted chunk
// advances its offset after the server response; a lost response repeats the
// same offset and digest so the server can replay the idempotent result.
export async function uploadChannelContent(channelID, upload, headers = {}, { onProgress, onSaving, signal } = {}) {
  const baseURL = `/api/v1/channels/${encodeURIComponent(channelID)}/content`;
  if (!upload.uploadID) {
    const response = await request(baseURL, {
      method: "POST",
      headers: {
        ...headers,
        "Content-Type": "application/json",
        "Idempotency-Key": upload.initRequestID,
      },
      body: JSON.stringify({ manifest: upload.manifest }),
      signal,
    });
    const result = await readResponseJSON(response);
    if (!result.upload_id) throw new RequestError(0, "invalid_upload_session");
    upload.uploadID = result.upload_id;
  }

  let acceptedBytes = [...upload.offsets.values()].reduce((sum, offset) => sum + offset, 0);
  for (const part of upload.parts) {
    let offset = upload.offsets.get(part.key) || 0;
    while (offset < part.blob.size) {
      if (signal?.aborted) throw new RequestError(0, "aborted");
      const chunk = part.blob.slice(offset, Math.min(offset + CHANNEL_UPLOAD_CHUNK_BYTES, part.blob.size));
      const hash = await digestHex(chunk);
      const chunkHeaders = new Headers({ ...headers, ...csrfHeaders() });
      chunkHeaders.set("Upload-Offset", String(offset));
      chunkHeaders.set("X-Chunk-SHA256", hash);
      await uploadChannelChunk(`${baseURL}/${encodeURIComponent(upload.uploadID)}/parts/${encodeURIComponent(part.key)}`, chunk, chunkHeaders, {
        signal,
        onProgress: (loaded) => onProgress?.({ loaded: acceptedBytes + loaded, total: upload.totalBytes }),
      });
      offset += chunk.size;
      acceptedBytes += chunk.size;
      upload.offsets.set(part.key, offset);
      onProgress?.({ loaded: acceptedBytes, total: upload.totalBytes });
    }
  }

  onSaving?.();
  const response = await request(`${baseURL}/${encodeURIComponent(upload.uploadID)}/finalize`, {
    method: "POST",
    headers: { ...headers, "Idempotency-Key": upload.finalizeRequestID },
    signal,
  });
  return readResponseJSON(response);
}

async function readResponseJSON(response) {
  try {
    return await response.json();
  } catch (error) {
    if (error?.name === "AbortError") throw new RequestError(0, "aborted");
    throw error;
  }
}

export async function abortChannelContent(channelID, upload, headers = {}) {
  if (!upload) return;
  const baseURL = `/api/v1/channels/${encodeURIComponent(channelID)}/content`;
  if (!upload.uploadID) {
    const initialized = await request(baseURL, {
      method: "POST",
      headers: {
        ...headers,
        "Content-Type": "application/json",
        "Idempotency-Key": upload.initRequestID,
      },
      body: JSON.stringify({ manifest: upload.manifest }),
    });
    upload.uploadID = (await initialized.json()).upload_id || "";
  }
  if (!upload.uploadID) throw new RequestError(0, "invalid_upload_session");
  await request(`${baseURL}/${encodeURIComponent(upload.uploadID)}`, {
    method: "DELETE",
    headers,
  });
}

export async function cleanupCanceledChannelUpload(channelID, upload, headers = {}) {
  try {
    await abortChannelContent(channelID, upload, headers);
    return true;
  } catch (error) {
    return (error?.status === 404 && error.message === "channel_not_found") ||
      (error?.status === 410 && error.message === "content_upload_closed");
  }
}

export class RequestError extends Error {
  constructor(status, code) {
    super(code);
    this.status = status;
  }
}
export async function request(url, options = {}) {
  let response;
  const headers = new Headers(options.headers || {});
  if (
    !["GET", "HEAD", "OPTIONS"].includes(
      (options.method || "GET").toUpperCase(),
    )
  )
    for (const [key, value] of Object.entries(csrfHeaders()))
      headers.set(key, value);
  try {
    response = await fetch(url, {
      credentials: "same-origin",
      cache: "no-store",
      ...options,
      headers,
    });
  } catch (error) {
    if (error?.name === "AbortError") throw new RequestError(0, "aborted");
    throw new RequestError(0, "network");
  }
  if (!response.ok) {
    let code = "failed";
    try {
      const error = await response.json();
      code = error.error || code;
    } catch {
      /* Proxy errors can be HTML. */
    }
    throw new RequestError(response.status, code);
  }
  return response;
}

// XHR is the only widely supported way to observe request-body progress.
// It keeps the same status/error contract as request() and resolves the JSON
// body the server returns after storing the share.
export function uploadShare(form, headers = {}, { onProgress, signal } = {}) {
  return new Promise((resolve, reject) => {
    const xhr = new XMLHttpRequest();
    xhr.open("POST", "/api/v1/shares");
    const merged = new Headers(headers);
    for (const [key, value] of Object.entries(csrfHeaders()))
      merged.set(key, value);
    for (const [key, value] of merged) xhr.setRequestHeader(key, value);
    xhr.upload.addEventListener("progress", (event) => {
      if (onProgress)
        onProgress({
          loaded: event.loaded,
          total: event.lengthComputable ? event.total : 0,
        });
    });
    xhr.addEventListener("load", () => {
      let body = null;
      try {
        body = xhr.responseText ? JSON.parse(xhr.responseText) : null;
      } catch {
        /* Proxy errors can be HTML. */
      }
      if (xhr.status >= 200 && xhr.status < 300) resolve(body);
      else reject(new RequestError(xhr.status, body?.error || "failed"));
    });
    xhr.addEventListener("error", () => reject(new RequestError(0, "network")));
    xhr.addEventListener("abort", () => reject(new RequestError(0, "aborted")));
    signal?.addEventListener("abort", () => xhr.abort(), { once: true });
    xhr.send(form);
  });
}

// Downloads pass through a reporting stream, so Content-Length can drive a
// determinate bar without buffering an extra copy of the payload. Without a
// usable length the caller receives total=0 and must show bytes, not a percent.
export async function requestStream(url, options = {}, { onProgress, signal } = {}) {
  const method = (options.method || "GET").toUpperCase();
  const headers = new Headers(options.headers || {});
  if (!["GET", "HEAD", "OPTIONS"].includes(method))
    for (const [key, value] of Object.entries(csrfHeaders()))
      headers.set(key, value);
  let response;
  try {
    response = await fetch(url, {
      credentials: "same-origin",
      cache: "no-store",
      ...options,
      headers,
      signal,
    });
  } catch (error) {
    if (error?.name === "AbortError") throw new RequestError(0, "aborted");
    throw new RequestError(0, "network");
  }
  if (!response.ok) {
    let code = "failed";
    try {
      const error = await response.json();
      code = error.error || code;
    } catch {
      /* Proxy errors can be HTML. */
    }
    throw new RequestError(response.status, code);
  }
  if (!response.body || !onProgress) return response;
  const encoding = response.headers.get("Content-Encoding");
  const declared = Number(response.headers.get("Content-Length")) || 0;
  const total = encoding && encoding !== "identity" ? 0 : declared;
  const reader = response.body.getReader();
  let received = 0,
    painted = 0;
  const report = (force) => {
    const at = performance.now();
    if (!force && at - painted < 100) return;
    painted = at;
    onProgress({ received, total });
  };
  report(true);
  const body = new ReadableStream({
    async pull(controller) {
      let step;
      try {
        step = await reader.read();
      } catch (error) {
        controller.error(error);
        return;
      }
      if (step.done) {
        report(true);
        controller.close();
        return;
      }
      received += step.value?.byteLength || 0;
      report(false);
      controller.enqueue(step.value);
    },
    cancel(reason) {
      return reader.cancel(reason);
    },
  });
  const passthrough = new Headers(response.headers);
  // The payload is decoded by fetch; these no longer describe the stream.
  passthrough.delete("Content-Encoding");
  passthrough.delete("Content-Length");
  return new Response(body, {
    status: response.status,
    statusText: response.statusText,
    headers: passthrough,
  });
}

function contentRange(value) {
  const match = /^bytes (\d+)-(\d+)\/(\d+)$/i.exec(value || "");
  if (!match) return null;
  const start = Number(match[1]);
  const end = Number(match[2]);
  const total = Number(match[3]);
  if (![start, end, total].every(Number.isSafeInteger) || end < start)
    return null;
  return { start, end, total };
}

// Downloads are retained by the caller so an interrupted ordinary share can
// resume from the exact byte offset. The response bytes are stable, including
// multipart framing, because the server derives its boundary deterministically.
export async function requestBytes(
  url,
  options = {},
  {
    onProgress,
    onChunk,
    onReset,
    onMeta,
    start = 0,
    signal,
  } = {},
) {
  const method = (options.method || "GET").toUpperCase();
  const headers = new Headers(options.headers || {});
  if (!["GET", "HEAD", "OPTIONS"].includes(method))
    for (const [key, value] of Object.entries(csrfHeaders()))
      headers.set(key, value);
  if (start > 0) headers.set("Range", `bytes=${start}-`);
  let response;
  try {
    response = await fetch(url, {
      credentials: "same-origin",
      cache: "no-store",
      ...options,
      headers,
      signal,
    });
  } catch (error) {
    if (error?.name === "AbortError") throw new RequestError(0, "aborted");
    throw new RequestError(0, "network");
  }
  if (!response.ok) {
    let code = "failed";
    try {
      const error = await response.json();
      code = error.error || code;
    } catch {
      /* Proxy errors can be HTML. */
    }
    throw new RequestError(response.status, code);
  }
  if (!response.body) throw new RequestError(0, "no_body");

  const encoding = response.headers.get("Content-Encoding");
  const encoded = !!encoding && encoding !== "identity";
  const range = contentRange(response.headers.get("Content-Range"));
  let offset = 0;
  let total = 0;
  if (start > 0 && response.status === 200) {
    // A proxy or older server ignored Range. Discard prior retained bytes.
    onReset?.();
    offset = 0;
  } else if (start > 0) {
    if (response.status !== 206 || encoded || !range || range.start !== start)
      throw new RequestError(0, "range_mismatch");
    const partialLength = Number(response.headers.get("Content-Length")) || 0;
    if (
      range.end >= range.total ||
      (partialLength > 0 && range.end - range.start + 1 !== partialLength)
    )
      throw new RequestError(0, "range_mismatch");
    offset = start;
    total = range.total;
  }
  if (total === 0 && !encoded)
    total = Number(response.headers.get("Content-Length")) || 0;
  const resumable =
    !encoded &&
    (response.headers.get("Accept-Ranges") || "").toLowerCase() === "bytes";
  onMeta?.({ resumable, total, etag: response.headers.get("ETag") || "" });

  const reader = response.body.getReader();
  let received = offset,
    painted = 0;
  if (total > 0 && received > total)
    throw new RequestError(0, "range_mismatch");
  const report = (force) => {
    const at = performance.now();
    if (!force && at - painted < 100) return;
    painted = at;
    onProgress?.({ received, total, resumable });
  };
  report(true);
  try {
    while (true) {
      const step = await reader.read();
      if (step.done) break;
      const chunk = step.value;
      received += chunk?.byteLength || 0;
      onChunk?.(chunk);
      report(false);
    }
  } catch (error) {
    if (error?.name === "AbortError") throw new RequestError(0, "aborted");
    throw new RequestError(0, "network");
  }
  report(true);
  if (total > 0 && received !== total)
    throw new RequestError(0, "incomplete");
  return {
    received,
    total,
    resumable,
    contentType: response.headers.get("Content-Type") || "",
  };
}

export function encodeShare(items) {
  const form = new FormData();
  let index = 0;
  const manifest = {
    version: 1,
    items: items.map((item) => ({
      kind: item.kind,
      name: item.name,
      parts: item.parts.map((part) => {
        const key = `p${index++}`;
        form.append(key, part.blob, item.name);
        return { key, mime: part.mime };
      }),
    })),
  };
  form.append("manifest", JSON.stringify(manifest));
  return form;
}
export async function decodeShare(response) {
  const form = await response.formData();
  const manifest = JSON.parse(form.get("manifest"));
  if (
    manifest.version !== 1 ||
    !Array.isArray(manifest.items) ||
    manifest.items.length > 32
  )
    throw new Error("unsupported");
  const items = manifest.items.map((item) => ({
    ...item,
    parts: item.parts.map((part) => {
      const blob = form.get(part.key);
      if (!(blob instanceof Blob) || blob.size !== part.size)
        throw new Error("failed");
      return { ...part, blob: blob.slice(0, blob.size, part.mime) };
    }),
  }));
  return { items, archive: form.get("archive") };
}
