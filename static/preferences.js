// Only preferences are persisted. Clipboard contents and API keys stay in memory.
const storageKey = "nclip.preferences.v1";
const defaults = {
  language: "system",
  theme: "system",
  suggestions: false,
  burn: false,
  public: false,
  ttlHours: 24,
};
let saved = {};
try {
  if (typeof localStorage !== "undefined")
    saved = JSON.parse(localStorage.getItem(storageKey)) || {};
} catch {
  /* Storage can be unavailable. */
}
export function normalizePreferences(value) {
  const source = value && typeof value === "object" && !Array.isArray(value) ? value : {};
  const normalized = { ...defaults };
  for (const key of Object.keys(defaults))
    if (Object.hasOwn(source, key)) normalized[key] = source[key];
  if (!["system", "zh", "en"].includes(normalized.language))
    normalized.language = defaults.language;
  if (!["system", "light", "dark"].includes(normalized.theme))
    normalized.theme = defaults.theme;
  for (const key of ["suggestions", "burn", "public"])
    if (typeof normalized[key] !== "boolean") normalized[key] = defaults[key];
  if (!Number.isInteger(normalized.ttlHours)) normalized.ttlHours = defaults.ttlHours;
  normalized.ttlHours = Math.min(168, Math.max(1, normalized.ttlHours));
  return normalized;
}
export const preferences = normalizePreferences(saved);
const media =
  typeof matchMedia === "function"
    ? matchMedia("(prefers-color-scheme: dark)")
    : { matches: false, addEventListener() {} };
export function language() {
  if (preferences.language !== "system") return preferences.language;
  const languages =
    typeof navigator === "undefined"
      ? ["zh"]
      : navigator.languages || [navigator.language];
  for (const value of languages) {
    if (/^zh\b/i.test(value)) return "zh";
    if (/^en\b/i.test(value)) return "en";
  }
  return "zh";
}
export function applyPreferences() {
  if (typeof document === "undefined") return;
  if (typeof window === "undefined") return;
  document.documentElement.lang = language() === "en" ? "en" : "zh-CN";
  document.documentElement.dataset.theme =
    preferences.theme === "system"
      ? media.matches
        ? "dark"
        : "light"
      : preferences.theme;
}
export function savePreferences(patch) {
  Object.assign(preferences, patch);
  try {
    if (typeof localStorage !== "undefined")
      localStorage.setItem(storageKey, JSON.stringify(preferences));
  } catch {
    /* Keep session preferences. */
  }
  applyPreferences();
  if (typeof window !== "undefined")
    window.dispatchEvent(new Event("preferenceschange"));
}
media.addEventListener("change", applyPreferences);
applyPreferences();
