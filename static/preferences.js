// Only preferences are persisted. Clipboard contents and API keys stay in memory.
const storageKey = "nclip.preferences.v1";
const defaults = { language: "system", theme: "system", suggestions: false };
let saved = {};
try {
  saved = JSON.parse(localStorage.getItem(storageKey)) || {};
} catch {
  /* Storage can be unavailable. */
}
export const preferences = { ...defaults, ...saved };
if (!["system", "zh", "en"].includes(preferences.language))
  preferences.language = "system";
if (!["system", "light", "dark"].includes(preferences.theme))
  preferences.theme = "system";
const media = matchMedia("(prefers-color-scheme: dark)");
export function language() {
  if (preferences.language !== "system") return preferences.language;
  for (const value of navigator.languages || [navigator.language]) {
    if (/^zh\b/i.test(value)) return "zh";
    if (/^en\b/i.test(value)) return "en";
  }
  return "zh";
}
export function applyPreferences() {
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
    localStorage.setItem(storageKey, JSON.stringify(preferences));
  } catch {
    /* Keep session preferences. */
  }
  applyPreferences();
  window.dispatchEvent(new Event("preferenceschange"));
}
media.addEventListener("change", applyPreferences);
applyPreferences();
