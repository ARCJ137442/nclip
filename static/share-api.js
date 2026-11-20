import { csrfHeaders } from "./access.js";
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
  } catch {
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
