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
  draggedLinks,
} from "./clipboard.js";
import { request, uploadShare, encodeShare } from "./share-api.js";
import { createProgress, timeParts } from "./transfer.js";

export async function startComposer() {
  const editor = $("message");
  let config = null,
    attachments = [],
    rich = null,
    candidate = [],
    readingVersion = 0,
    sending = false,
    draftRevision = 0,
    fullySent = false,
    lastAttempt = null,
    transferController = null,
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
  function draftBytes(items = draft()) {
    return items.reduce(
      (total, item) =>
        total +
        item.parts.reduce((sum, part) => sum + part.blob.size, 0),
      0,
    );
  }
  function updateDraftMeter() {
    if (!$("draft-meter")) return;
    const items = draft(),
      size = draftBytes(items),
      max = config?.upload_max_bytes || 0;
    $("draft-size").textContent = sizeLabel(size);
    $("draft-count").textContent = t(
      items.length === 1 ? "draftItemsOne" : "draftItemsMany",
      { count: items.length },
    );
    $("draft-meter-fill").style.width =
      max > 0 ? `${Math.min(100, (size / max) * 100)}%` : "0%";
    $("draft-meter").dataset.over = String(max > 0 && size > max);
  }
  function changed() {
    // Revision, not a transient flag: edits made before a retry still count,
    // so a retried snapshot can never discard the newer draft on success.
    draftRevision++;
    hideCandidate();
    $("path-hint").hidden = !/^(?:[A-Za-z]:[\\/]|\\\\|file:\/\/)/.test(
      editor.value.trim(),
    );
    updateDraftMeter();
  }
  function etaLabel(seconds) {
    const part = timeParts(seconds);
    if (!part) return "";
    return t(part.unit === "minutes" ? "timeMinutes" : "timeSeconds", {
      count: part.count,
    });
  }
  function paintTransfer({ received, total, percent, rate, eta }) {
    const track = $("transfer-track");
    $("transfer-percent").textContent =
      percent == null ? "" : `${Math.floor(percent)}%`;
    $("transfer-fill").style.width = percent == null ? "" : `${percent}%`;
    track.dataset.unknown = String(percent == null);
    // An indeterminate bar must not claim a value; only remove the attribute.
    if (percent == null) track.removeAttribute("aria-valuenow");
    else track.setAttribute("aria-valuenow", String(Math.floor(percent)));
    $("transfer-bytes").textContent =
      total > 0
        ? t("transferBytes", {
            sent: sizeLabel(received),
            total: sizeLabel(total),
          })
        : sizeLabel(received);
    $("transfer-rate").textContent =
      rate > 0
        ? `${t("transferRate", { rate: sizeLabel(rate) })}${
            eta != null ? ` · ${t("transferEta", { time: etaLabel(eta) })}` : ""
          }`
        : "";
  }
  function setTransferPhase(phase) {
    $("transfer").dataset.phase = phase;
    $("transfer-phase").textContent = t(
      {
        uploading: "transferUploading",
        saving: "transferSaving",
        error: "transferFailed",
        canceled: "transferCanceled",
      }[phase] || "transferUploading",
    );
    $("transfer-cancel").hidden = !["uploading", "saving"].includes(phase);
    $("transfer-actions").hidden = !["error", "canceled"].includes(phase);
    $("transfer-note").hidden = !["error", "canceled"].includes(phase);
    if (phase === "saving") {
      $("transfer-track").dataset.unknown = "true";
      $("transfer-track").removeAttribute("aria-valuenow");
      $("transfer-percent").textContent = "";
      $("transfer-rate").textContent = "";
    }
    if (phase === "error") $("transfer-note").textContent = t("transferFailedNote");
    // Only a fully handed-off body can have reached the server.
    if (phase === "canceled")
      $("transfer-note").textContent = t(
        fullySent ? "transferCanceledNote" : "transferCanceledEarly",
      );
  }
  async function renderDraft() {
    await renderItems($("draft-items"), attachments, {
      remove: (index) => {
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
    updateDraftMeter();
  }
  async function append(items) {
    const max = config?.max_items || 32;
    if (draft().length + items.length > max) {
      status("tooMany", "error", { count: max });
      return;
    }
    // Adding content is the recovery gesture for every composer error.
    status("");
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
      (automatic && sending) ||
      $("composer").hidden ||
      (automatic && (!preferences.suggestions || draft().length))
    )
      return;
    const version = ++readingVersion;
    if (!automatic) status("reading");
    try {
      const items = await readClipboard({ automatic });
      if (version !== readingVersion || (automatic && sending) || $("composer").hidden)
        return;
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
  async function runUpload(items, revision = draftRevision) {
    if (sending) return;
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
    const form = encodeShare(items);
    // The revision travels with the snapshot so a retry can tell whether the
    // draft still holds exactly the content being sent.
    lastAttempt = { items, revision };
    fullySent = false;
    sending = true;
    invalidateRead();
    // The sent content is a snapshot: the editor stays usable for the next
    // message while this transfer runs, and edits never change what is sent.
    $("send").disabled = true;
    $("send-candidate").disabled = true;
    $("send").setAttribute("aria-busy", "true");
    $("result").hidden = true;
    $("new-share").hidden = false;
    $("transfer").hidden = false;
    setTransferPhase("uploading");
    paintTransfer({ received: 0, total: 0, percent: 0 });
    status("uploading");
    const clock = createProgress();
    clock.begin();
    const controller = new AbortController();
    transferController = controller;
    let painted = 0;
    try {
      const result = await uploadShare(
        form,
        {
          ...authHeaders(),
          "X-Burn": String($("burn").checked),
          "X-Nclip-Visibility": $("public-share").checked
            ? "public"
            : "authenticated",
        },
        {
          signal: controller.signal,
          onProgress: ({ loaded, total }) => {
            const at = performance.now();
            if (loaded < total && at - painted < 100) return;
            painted = at;
            paintTransfer(clock.update(loaded, total));
            if (total > 0 && loaded >= total) {
              fullySent = true;
              setTransferPhase("saving");
            }
          },
        },
      );
      if (!result?.url) throw new Error("failed");
      $("share-url").value = result.url;
      $("open-share").href = result.url;
      $("open-share").hidden =
        access.enabled && !can("download") && !result.public;
      $("native-share").hidden = !navigator.share;
      $("transfer").hidden = true;
      hideCandidate();
      $("result").hidden = false;
      if (draftRevision !== revision) {
        // The draft no longer matches the sent snapshot: keep it visible,
        // do not steal focus, and do not overwrite the clipboard.
        $("new-share").hidden = true;
        $("result-message").textContent = t("sentBackground");
        status("sent", "success");
      } else {
        $("composer").hidden = true;
        $("result-message").textContent = t("sentManual");
        $("result").focus({ preventScroll: true });
        status("sent", "success");
        await copyLink();
      }
    } catch (error) {
      if (error.message === "aborted") {
        setTransferPhase("canceled");
        status("transferCanceled", "error");
      } else {
        setTransferPhase("error");
        failure(error, config);
      }
    } finally {
      transferController = null;
      sending = false;
      $("send").disabled = false;
      $("send-candidate").disabled = false;
      $("send").removeAttribute("aria-busy");
    }
  }
  async function send(useCandidate = false) {
    if (sending) return;
    await runUpload(
      useCandidate ? candidate : draft().length ? draft() : candidate,
    );
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
  $("transfer-cancel").addEventListener("click", () =>
    transferController?.abort(),
  );
  $("transfer-retry").addEventListener("click", () => {
    if (!sending && lastAttempt)
      void runUpload(lastAttempt.items, lastAttempt.revision);
  });
  $("transfer-dismiss").addEventListener("click", () => {
    $("transfer").hidden = true;
    status("");
  });
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
    updateDraftMeter();
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
  // A dragged link carries only a URI. file: URIs cannot be read by the page,
  // so refuse them with an actionable message instead of silently sharing the
  // path as text; ordinary links keep their previous insertion outcome.
  const isLink = (e) =>
    !isFiles(e) &&
    Array.from(e.dataTransfer?.types || []).includes("text/uri-list");
  $("composer").addEventListener("dragover", (e) => {
    if (isLink(e) && !sending && !$("composer").hidden) {
      e.preventDefault();
      e.dataTransfer.dropEffect = "copy";
    }
  });
  $("composer").addEventListener("drop", (e) => {
    if (!isLink(e) || sending || $("composer").hidden) return;
    const link = draggedLinks(e.dataTransfer);
    if (!link) return;
    e.preventDefault();
    dragDepth = 0;
    $("drop-overlay").hidden = true;
    if (link.local) {
      status("localDrop", "error");
      // Keep the guidance next to the composer; the status row can sit below
      // the fold on short screens.
      $("path-hint").textContent = t("localDrop");
      $("path-hint").hidden = false;
      $("attach").focus();
      return;
    }
    // Native link drops insert at the pointer; the caret is the closest
    // stable approximation when the page has to handle the drop itself.
    status("");
    editor.setRangeText(
      link.text,
      editor.selectionStart ?? editor.value.length,
      editor.selectionEnd ?? editor.value.length,
      "end",
    );
    rich = null;
    $("rich-hint").hidden = true;
    changed();
    editor.focus();
  });
  window.addEventListener("blur", () => {
    invalidateRead();
    dragDepth = 0;
    $("drop-overlay").hidden = true;
  });
  // Navigation aborts the request. Ask before discarding an active transfer.
  window.addEventListener("beforeunload", (event) => {
    if (!sending) return;
    event.preventDefault();
    event.returnValue = "";
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
    // The phase text replaced the markup's i18n default; re-render it in the
    // newly selected language instead of leaving a stale translation.
    if (!$("transfer").hidden)
      setTransferPhase($("transfer").dataset.phase || "uploading");
  });
  try {
    config = await (await request("/api/v1/ui-config")).json();
    updateLimits();
    await probe(true);
  } catch (error) {
    failure(error);
  }
}
