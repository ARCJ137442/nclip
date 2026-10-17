import { $, status, failure, authHeaders, renderItems } from "./ui.js";
import { preferences } from "./preferences.js";
import { access, can } from "./access.js";
import { t, sizeLabel } from "./i18n.js";
import {
  readClipboard,
  pastedItems,
  fileItem,
  textItem,
  copyText,
} from "./clipboard.js";
import { request, encodeShare } from "./share-api.js";

export async function startComposer() {
  const editor = $("message");
  let config = null,
    attachments = [],
    rich = null,
    candidate = [],
    readingVersion = 0,
    sending = false,
    lastCopiedURL = "";
  function draft() {
    return [
      ...(editor.value.length ? [rich || textItem(editor.value)] : []),
      ...attachments,
    ];
  }
  function invalidateRead() {
    readingVersion++;
  }
  function hideCandidate() {
    candidate = [];
    $("candidate").hidden = true;
    invalidateRead();
  }
  function changed() {
    hideCandidate();
    $("path-hint").hidden = !/^(?:[A-Za-z]:[\\/]|\\\\|file:\/\/)/.test(
      editor.value.trim(),
    );
  }
  async function renderDraft() {
    await renderItems($("draft-items"), attachments, {
      remove: (index) => {
        if (sending) return;
        attachments.splice(index, 1);
        void renderDraft();
        changed();
      },
    });
  }
  function updateLimits() {
    if (config)
      $("limits").textContent = t("limits", {
        size: sizeLabel(config.upload_max_bytes),
        hours: config.default_ttl_seconds / 3600,
      });
  }
  async function append(items) {
    if (sending) return;
    const max = config?.max_items || 32;
    if (draft().length + items.length > max) {
      status("tooMany", "error", { count: max });
      return;
    }
    for (const item of items) {
      const plain = item.parts.find((p) => p.mime === "text/plain");
      if (item.kind === "clipboard" && plain && !editor.value) {
        editor.value = await plain.blob.text();
        rich = item.parts.some((p) => p.mime === "text/html") ? item : null;
        $("rich-hint").hidden = !rich;
      } else attachments.push(item);
    }
    changed();
    await renderDraft();
    editor.focus();
  }
  async function probe(automatic) {
    if (
      sending ||
      $("composer").hidden ||
      (automatic && (!preferences.suggestions || draft().length))
    )
      return;
    const version = ++readingVersion;
    if (!automatic) status("reading");
    try {
      const items = await readClipboard({ automatic });
      if (version !== readingVersion || sending || $("composer").hidden) return;
      if (!items.length) {
        if (!automatic) status("unsupported", "error");
        return;
      }
      const single =
        items.length === 1 &&
        items[0].parts.length === 1 &&
        items[0].parts[0].mime === "text/plain"
          ? await items[0].parts[0].blob.text()
          : null;
      if (version !== readingVersion || (automatic && draft().length)) return;
      if (automatic && single) {
        // Own share links stay suppressed across navigation without persisting clipboard data.
        let ownShare = false;
        try {
          const url = new URL(single);
          ownShare =
            url.origin === location.origin &&
            /^\/[A-Z2-9]{5,7}$/.test(url.pathname);
        } catch {
          /* Ordinary text. */
        }
        if (single === lastCopiedURL || ownShare) return;
      }
      if (!automatic) {
        await append(items);
        status("");
        return;
      }
      candidate = items;
      $("candidate").hidden = false;
      await renderItems($("candidate-preview"), items);
    } catch (error) {
      if (version === readingVersion && !automatic)
        status("permission", "error");
    }
  }
  async function copyLink() {
    try {
      await copyText($("share-url").value);
      lastCopiedURL = $("share-url").value;
      $("result-message").textContent = t("sentCopied");
      status("copied", "success");
    } catch {
      $("share-url").focus();
      $("share-url").select();
      status("copyFailed", "error");
    }
  }
  async function send(useCandidate = false) {
    if (sending) return;
    const items = useCandidate
      ? candidate
      : draft().length
        ? draft()
        : candidate;
    if (!items.length) {
      status("empty", "error");
      editor.focus();
      return;
    }
    if (!config) {
      status("loading");
      return;
    }
    if (items.length > config.max_items) {
      status("tooMany", "error", { count: config.max_items });
      return;
    }
    const form = encodeShare(items);
    // Request framing counts toward the proxy limit too; the server remains authoritative.
    if (
      items.reduce(
        (n, i) => n + i.parts.reduce((sum, p) => sum + p.blob.size, 0),
        0,
      ) > config.upload_max_bytes
    ) {
      status("tooLarge", "error", { size: sizeLabel(config.upload_max_bytes) });
      return;
    }
    sending = true;
    invalidateRead();
    $("draft-controls").disabled = true;
    $("send-candidate").disabled = true;
    $("send").setAttribute("aria-busy", "true");
    status("uploading");
    try {
      const response = await request("/api/v1/shares", {
        method: "POST",
        headers: {
          ...authHeaders(),
          "X-Burn": String($("burn").checked),
          "X-Nclip-Visibility": $("public-share").checked
            ? "public"
            : "authenticated",
        },
        body: form,
      });
      const result = await response.json();
      $("share-url").value = result.url;
      $("open-share").href = result.url;
      $("open-share").hidden =
        access.enabled && !can("download") && !result.public;
      $("composer").hidden = true;
      $("result").hidden = false;
      $("result-message").textContent = t("sentManual");
      $("native-share").hidden = !navigator.share;
      hideCandidate();
      $("result").focus({ preventScroll: true });
      status("sent", "success");
      await copyLink();
    } catch (error) {
      failure(error, config);
    } finally {
      sending = false;
      $("draft-controls").disabled = false;
      $("send-candidate").disabled = false;
      $("send").removeAttribute("aria-busy");
    }
  }
  editor.addEventListener("input", () => {
    rich = null;
    $("rich-hint").hidden = true;
    changed();
  });
  $("composer").addEventListener("paste", async (e) => {
    if (sending || e.target.closest("input")) return;
    const items = pastedItems(e.clipboardData);
    if (!items.length) return;
    if (!e.clipboardData.files.length && editor.value) {
      rich = null;
      $("rich-hint").hidden = true;
      changed();
      return;
    }
    e.preventDefault();
    await append(items);
  });
  $("paste").addEventListener("click", () => probe(false));
  $("attach").addEventListener("click", () => $("files").click());
  $("files").addEventListener("change", async () => {
    await append(Array.from($("files").files, fileItem));
    $("files").value = "";
  });
  $("send").addEventListener("click", () => send());
  $("send-candidate").addEventListener("click", () => send(true));
  $("copy-link").addEventListener("click", copyLink);
  $("native-share").addEventListener("click", async () => {
    try {
      await navigator.share({ url: $("share-url").value });
    } catch (error) {
      if (error.name !== "AbortError") await copyLink();
    }
  });
  $("new-share").addEventListener("click", () => {
    editor.value = "";
    attachments = [];
    rich = null;
    $("rich-hint").hidden = true;
    $("path-hint").hidden = true;
    $("burn").checked = false;
    $("public-share").checked = false;
    $("result").hidden = true;
    $("composer").hidden = false;
    void renderDraft();
    status("");
    editor.focus();
    void probe(true);
  });
  document.addEventListener("keydown", (e) => {
    if (
      (e.ctrlKey || e.metaKey) &&
      e.key === "Enter" &&
      !e.isComposing &&
      !e.repeat &&
      !e.target.closest("#settings,.api-key") &&
      !$("composer").hidden
    ) {
      e.preventDefault();
      void send();
    }
  });
  let dragDepth = 0;
  const isFiles = (e) =>
    Array.from(e.dataTransfer?.types || []).includes("Files");
  document.addEventListener("dragenter", (e) => {
    if (isFiles(e) && !sending && !$("composer").hidden) {
      e.preventDefault();
      dragDepth++;
      $("drop-overlay").hidden = false;
    }
  });
  document.addEventListener("dragover", (e) => {
    if (isFiles(e)) {
      e.preventDefault();
      e.dataTransfer.dropEffect = sending ? "none" : "copy";
    }
  });
  document.addEventListener("dragleave", (e) => {
    if (isFiles(e)) {
      dragDepth = Math.max(0, dragDepth - 1);
      if (!dragDepth) $("drop-overlay").hidden = true;
    }
  });
  document.addEventListener("drop", (e) => {
    if (isFiles(e)) {
      e.preventDefault();
      dragDepth = 0;
      $("drop-overlay").hidden = true;
      if (!$("composer").hidden)
        void append(Array.from(e.dataTransfer.files, fileItem));
    }
  });
  window.addEventListener("blur", () => {
    invalidateRead();
    dragDepth = 0;
    $("drop-overlay").hidden = true;
  });
  window.addEventListener("focus", () => probe(true));
  $("suggestions").addEventListener("change", () => {
    if (preferences.suggestions) void probe(false); // Enabling is an explicit read gesture.
  });
  document.addEventListener("visibilitychange", () => {
    if (document.visibilityState !== "visible") invalidateRead();
  });
  window.addEventListener("preferenceschange", () => {
    updateLimits();
    if (!preferences.suggestions) hideCandidate();
    else void probe(true);
    void renderDraft();
  });
  try {
    config = await (await request("/api/v1/ui-config")).json();
    updateLimits();
    await probe(true);
  } catch (error) {
    failure(error);
  }
}
