// Only preferences are persisted. Clipboard contents and API keys stay in memory.
const storageKey = "nclip.preferences.v1";
const defaults = { language: "system", theme: "system", suggestions: false };
let saved = {};
try {
  if (typeof localStorage !== "undefined")
    saved = JSON.parse(localStorage.getItem(storageKey)) || {};
} catch {
  /* Storage can be unavailable. */
}
export const preferences = { ...defaults, ...saved };
if (!["system", "zh", "en"].includes(preferences.language))
  preferences.language = "system";
if (!["system", "light", "dark"].includes(preferences.theme))
  preferences.theme = "system";
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
