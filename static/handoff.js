const secretMarkers = /(?:sk-|ghp_|github_pat_|AKIA|-----BEGIN)/i;
const base64Like = /[A-Za-z0-9+/=_-]{24,}/g;

export function clampTTLHours(value, min = 1, max = 168) {
  const number = typeof value === "number" ? value : Number(value);
  if (!Number.isFinite(number) || !Number.isInteger(number)) return null;
  return Math.min(max, Math.max(min, number));
}

export function safePreview(text, limit = 24) {
  if (typeof text !== "string" || secretMarkers.test(text)) return "";
  for (const token of text.match(base64Like) || []) {
    if (/[A-Za-z]/.test(token) && /[0-9]/.test(token)) return "";
  }
  return Array.from(text.replace(/[\p{Cc}\s]+/gu, " ").trim()).slice(0, limit).join("");
}

// The server derives previews only for clipboard objects with a plain-text part.
// Keep the sender summary on the same rule and inspect the same bounded prefix.
export async function previewForItem(item) {
  if (item?.kind !== "clipboard") return "";
  const plain = item.parts?.find((part) => part.mime === "text/plain");
  if (!plain?.blob) return "";
  try {
    return safePreview(await plain.blob.slice(0, 512).text());
  } catch {
    return "";
  }
}
