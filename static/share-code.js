// Resolve clipboard/user input locally; return only a code from this origin.
export function shareCode(value, origin) {
  value = value.trim();
  if (/^[A-Za-z2-9]{5,7}$/.test(value)) return value.toUpperCase();
  if (!value.startsWith("/") && !/^https?:\/\//i.test(value)) return null;
  try {
    const url = new URL(value, origin);
    if (url.origin !== origin || url.search || url.hash) return null;
    const match = /^\/(?:raw\/)?([A-Za-z2-9]{5,7})\/?$/.exec(url.pathname);
    return match ? match[1].toUpperCase() : null;
  } catch {
    return null;
  }
}
