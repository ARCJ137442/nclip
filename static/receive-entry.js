import { readClipboard } from "./clipboard.js";
import { preferences } from "./preferences.js";
import { t } from "./i18n.js";

import { shareCode } from "./share-code.js";
export function startReceiveEntry() {
  const form = document.getElementById("receive-form");
  if (!form) return;
  const input = document.getElementById("receive-code"),
    error = document.getElementById("receive-error"),
    suggestion = document.getElementById("open-suggested-code");
  const shortcut = document.getElementById("open-clipboard-receive");
  let version = 0,
    suggestedCode = null;
  if (document.body.dataset.page === "download")
    input.focus({ preventScroll: true });
  function open(value) {
    const code = shareCode(value, location.origin);
    if (!code) {
      error.textContent = t("invalidShareCode");
      input.focus();
      return;
    }
    location.assign("/" + code);
  }
  async function probe(automatic) {
    if (automatic && (!preferences.suggestions || input.value)) return;
    const current = ++version;
    suggestion.hidden = true;
    if (shortcut) shortcut.hidden = true;
    suggestedCode = null;
    try {
      const items = await readClipboard({ automatic }),
        plain = items
          .flatMap((item) => item.parts)
          .find((part) => part.mime === "text/plain");
      if (!plain) return;
      const value = await plain.blob.text();
      if (current !== version || (automatic && input.value)) return;
      const code = shareCode(value, location.origin);
      if (!code) {
        if (!automatic) error.textContent = t("invalidShareCode");
        return;
      }
      if (!automatic) {
        input.value = value.trim();
        error.textContent = "";
        input.focus();
        return;
      }
      suggestedCode = code;
      suggestion.textContent = t("openClipboardCode", { code });
      suggestion.hidden = false;
      if (shortcut) {
        shortcut.textContent = t("receiveClipboardShort", { code });
        shortcut.hidden = false;
      }
    } catch {
      if (!automatic) error.textContent = t("permission");
    }
  }
  form.addEventListener("submit", (event) => {
    event.preventDefault();
    open(input.value);
  });
  input.addEventListener("input", () => {
    version++;
    suggestion.hidden = true;
    if (shortcut) shortcut.hidden = true;
    error.textContent = "";
  });
  document
    .getElementById("paste-code")
    .addEventListener("click", () => probe(false));
  suggestion.addEventListener("click", () => {
    if (suggestedCode) open(suggestedCode);
  });
  shortcut?.addEventListener("click", () => {
    if (suggestedCode) open(suggestedCode);
  });
  window.addEventListener("blur", () => {
    version++;
  });
  window.addEventListener("focus", () => probe(true));
  window.addEventListener("preferenceschange", () => {
    if (!preferences.suggestions) {
      suggestion.hidden = true;
      if (shortcut) shortcut.hidden = true;
    } else void probe(true);
    if (suggestedCode)
      suggestion.textContent = t("openClipboardCode", { code: suggestedCode });
    if (suggestedCode && shortcut)
      shortcut.textContent = t("receiveClipboardShort", {
        code: suggestedCode,
      });
  });
  void probe(true);
}
