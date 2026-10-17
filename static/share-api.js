export class RequestError extends Error {
  constructor(status, code) {
    super(code);
    this.status = status;
  }
}
export async function request(url, options = {}) {
  let response;
  try {
    response = await fetch(url, {
      credentials: "same-origin",
      cache: "no-store",
      ...options,
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
