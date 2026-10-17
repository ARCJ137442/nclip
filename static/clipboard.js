// Browser permissions and clipboard formats are isolated from sending decisions.
export async function readClipboard({ automatic = false } = {}) {
  if (!window.isSecureContext || !navigator.clipboard)
    throw new Error("unsupported");
  if (automatic) {
    if (!document.hasFocus() || document.visibilityState !== "visible")
      return [];
    try {
      const permission = await navigator.permissions.query({
        name: "clipboard-read",
      });
      if (permission.state !== "granted") return [];
    } catch {
      return [];
    } // Safari/Firefox require a user gesture for each read.
  }
  if (navigator.clipboard.read) {
    const entries = await navigator.clipboard.read();
    const result = [];
    for (const entry of entries) {
      const parts = [];
      for (const type of ["text/plain", "text/html", "image/png"]) {
        if (entry.types.includes(type))
          parts.push({ mime: type, blob: await entry.getType(type) });
      }
      if (parts.length)
        result.push({
          kind: "clipboard",
          name: parts.some((p) => p.mime === "image/png")
            ? "clipboard.png"
            : "clipboard.txt",
          parts,
        });
    }
    return result;
  }
  const text = await navigator.clipboard.readText();
  return text ? [textItem(text)] : [];
}
export function textItem(text, html = "") {
  const parts = text
    ? [{ mime: "text/plain", blob: new Blob([text], { type: "text/plain" }) }]
    : [];
  if (html)
    parts.push({
      mime: "text/html",
      blob: new Blob([html], { type: "text/html" }),
    });
  return { kind: "clipboard", name: "text.txt", parts };
}
export function fileItem(file) {
  return {
    kind: "file",
    name: file.name,
    parts: [{ mime: file.type || "application/octet-stream", blob: file }],
  };
}
export function pastedItems(data) {
  // File objects take precedence over the HTML containing the same pasted image.
  if (data.files?.length) return Array.from(data.files, fileItem);
  const plain = data.getData("text/plain");
  const html = data.getData("text/html");
  return plain || html ? [textItem(plain, html)] : [];
}
export async function copyText(text) {
  if (navigator.clipboard?.writeText && window.isSecureContext) {
    try {
      await navigator.clipboard.writeText(text);
      return;
    } catch {
      /* Try the explicit selection fallback. */
    }
  }
  const focus = document.activeElement;
  const area = document.createElement("textarea");
  area.value = text;
  area.style.cssText = "position:fixed;left:-9999px;top:0";
  document.body.append(area);
  area.select();
  try {
    if (!document.execCommand("copy")) throw new Error("copyFailed");
  } finally {
    area.remove();
    focus?.focus({ preventScroll: true });
  }
}
export async function copyItem(item, { plainOnly = false } = {}) {
  const plain = item.parts.find((p) => p.mime === "text/plain");
  if (!plainOnly && navigator.clipboard?.write && window.ClipboardItem) {
    const types = {};
    for (const part of item.parts) {
      if (
        ["text/plain", "text/html", "image/png"].includes(part.mime) &&
        (!ClipboardItem.supports || ClipboardItem.supports(part.mime))
      )
        types[part.mime] = part.blob;
    }
    if (Object.keys(types).length) {
      try {
        await navigator.clipboard.write([new ClipboardItem(types)]);
        return { plain: false };
      } catch (error) {
        if (!plain) throw error;
      }
    }
  }
  if (!plain) throw new Error("unsupported");
  await copyText(await plain.blob.text());
  return { plain: plainOnly || item.parts.some((p) => p.mime === "text/html") };
}
