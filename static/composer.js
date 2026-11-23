import { $, status, failure, authHeaders, renderItems, icon } from "./ui.js";
import { preferences, savePreferences } from "./preferences.js";
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
import { createProgress, timeParts, transferStages } from "./transfer.js";
import { clampTTLHours, safePreview } from "./handoff.js";

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
    transferStartedAt = 0,
    transferFailure = null,
    lastCompleted = null,
    resultMessage = null,
    attempt = 0,
    sentLedger = [],
    configFailure = null,
    summaryRevision = 0,
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
    const over = max > 0 && size > max ? size - max : 0;
    const largest = items
      .map((item) => ({ item, size: item.parts.reduce((n, p) => n + p.blob.size, 0) }))
      .sort((a, b) => b.size - a.size)[0];
    $("draft-overflow").hidden = over <= 0;
    $("draft-overflow").textContent = over > 0
      ? t("draftOverflow", { size: sizeLabel(over), name: largest?.item.name || itemLabel(largest?.item || { kind: "clipboard", parts: [] }) })
      : "";
    void updateSendSummary(items, size, max);
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
  function paintStages(root, states) {
    for (const [stage, state] of Object.entries(states)) {
      const item = root.querySelector(`[data-stage="${stage}"]`);
      if (!item) continue;
      item.dataset.state = state;
      if (state === "active") item.setAttribute("aria-current", "step");
      else item.removeAttribute("aria-current");
    }
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
    const elapsed = (performance.now() - transferStartedAt) / 1000;
    $("transfer-freshness").textContent =
      elapsed >= 1
        ? t("transferElapsed", { time: etaLabel(elapsed) })
        : "";
  }
  function setTransferPhase(phase, error = null) {
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
    paintStages(
      $("transfer-stages"),
      transferStages(phase, { fullySent, status: error?.status || 0 }),
    );
    if (phase === "saving") {
      $("transfer-track").dataset.unknown = "true";
      $("transfer-track").removeAttribute("aria-valuenow");
      $("transfer-percent").textContent = "";
      $("transfer-rate").textContent = "";
      $("transfer-freshness").textContent = t("transferLastBytes", {
        time: new Date().toLocaleTimeString(document.documentElement.lang, {
          hour: "2-digit",
          minute: "2-digit",
          second: "2-digit",
          hour12: false,
        }),
      });
    }
    if (phase === "error") {
      $("transfer-phase").textContent = t(
        fullySent
          ? error?.status
            ? "transferRejected"
            : "transferUnconfirmed"
          : "transferFailed",
      );
      $("transfer-note").textContent = t(
        fullySent
          ? error?.status
            ? "transferRejectedNote"
            : "transferUnconfirmedNote"
          : "transferFailedNote",
      );
    }
    // Only a fully handed-off body can have reached the server.
    if (phase === "canceled") {
      $("transfer-phase").textContent = t(
        fullySent ? "transferCanceledUnknown" : "transferCanceled",
      );
      $("transfer-note").textContent = t(
        fullySent ? "transferUnconfirmedNote" : "transferCanceledEarly",
      );
    }
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
  function itemLabel(item) {
    if (item.kind === "file") return item.name;
    if (item.parts.some((part) => part.mime === "image/png"))
      return t("image");
    if (item.parts.some((part) => part.mime === "text/html")) return t("rich");
    return t("text");
  }
  async function itemPreview(item) {
    const plain = item.parts.find((part) => part.mime === "text/plain");
    if (!plain) return "";
    try {
      // Match the server's bounded preview source exactly (spool head = 512 bytes).
      return safePreview(await plain.blob.slice(0, 512).text());
    } catch {
      return "";
    }
  }
  function currentTTL() {
    const min = Math.ceil((config?.ttl_min_seconds ?? 3600) / 3600);
    const max = Math.floor((config?.ttl_max_seconds ?? 604800) / 3600);
    return clampTTLHours($("ttl-hours").value, min, max);
  }
  async function updateSendSummary(items = draft(), size = draftBytes(items), max = config?.upload_max_bytes || 0) {
    const previewVersion = ++summaryRevision;
    const hours = currentTTL() ?? preferences.ttlHours ?? 24;
    const accessText = $("public-share").checked ? t("summaryPublic") : t("summaryPrivate");
    const facts = [
      t(items.length === 1 ? "draftItemsOne" : "draftItemsMany", { count: items.length }),
      sizeLabel(size),
      t("ttlAfter", { hours }),
      accessText,
      ...($("burn").checked ? [t("summaryBurn")] : []),
    ];
    if (max > 0 && size > max) facts.push(t("draftOverBy", { size: sizeLabel(size - max) }));
    const summary = $("send-summary");
    if (!summary) return;
    summary.textContent = facts.join(" · ");
    const previews = await Promise.all(items.map(itemPreview));
    if (previewVersion !== summaryRevision) return;
    $("send-summary-preview").textContent = previews
      .map((preview) => preview
        ? `${t("receiverPreviewLabel")}: ${preview}`
        : t("previewOmitted"))
      .join(" · ");
  }
  function expiryFacts(expiresAt) {
    const date = expiresAt ? new Date(expiresAt) : null;
    if (!date || Number.isNaN(date.getTime())) return t("expiryUnknown");
    const hours = Math.max(0, (date.getTime() - Date.now()) / 3600000);
    const relative = hours >= 1
      ? t("ttlAfter", { hours: Math.ceil(hours) })
      : t("ttlUnderHour");
    return `${date.toLocaleString(document.documentElement.lang)} · ${relative}`;
  }
  function renderSentLedger() {
    const root = $("sent-log-list");
    root.replaceChildren();
    $("sent-log").hidden = sentLedger.length === 0;
    for (const item of sentLedger) {
      const row = document.createElement("li");
      const copy = document.createElement("button");
      copy.type = "button";
      copy.className = "sent-log-code";
      copy.dataset.icon = "copy";
      copy.textContent = item.slug;
      copy.prepend(icon("copy"));
      copy.setAttribute("aria-label", t("copyCodeAria", { code: item.slug }));
      copy.addEventListener("click", async () => {
        try { await copyText(item.slug); status("copied", "success"); }
        catch { status("copyFailed", "error"); }
      });
      const detail = document.createElement("span");
      detail.textContent = [new Date(item.createdAt).toLocaleTimeString(document.documentElement.lang), expiryFacts(item.expiresAt), item.burn ? t("burn") : "", item.public ? t("publicAccess") : ""].filter(Boolean).join(" · ");
      row.append(copy, detail);
      if (!item.openHidden) {
        const open = document.createElement("a");
        open.href = item.url;
        open.className = "button quiet";
        open.dataset.icon = "download";
        open.textContent = t("view");
        open.prepend(icon("download"));
        row.append(open);
      }
      root.append(row);
    }
  }
  function setResultFacts(items, elapsed, result = {}) {
    lastCompleted = { items, elapsed, ...result };
    const size = draftBytes(items);
    const facts = [
      t("resultFactItems", { count: items.length, size: sizeLabel(size) }),
      t("resultFactTime", { time: etaLabel(elapsed) || "—" }),
    ];
    if (elapsed >= 0.5 && size >= 64 * 1024)
      facts.push(
        t("resultFactRate", {
          rate: sizeLabel(Math.max(1, Math.round(size / elapsed))),
        }),
      );
    facts.push(
      lastCompleted.public ? t("resultPublic") : t("resultAuthenticated"),
    );
    if (lastCompleted.burn) facts.push(t("resultOneTime"));
    facts.push(expiryFacts(lastCompleted.expiresAt));
    const root = $("result-facts");
    root.replaceChildren(
      ...facts.map((text) => {
        const span = document.createElement("span");
        span.textContent = text;
        return span;
      }),
    );
    const names = items.map(itemLabel);
    const visible = names.slice(0, 4).join(" · ");
    $("result-items").textContent =
      visible +
      (names.length > 4
        ? ` · ${t("resultItemMore", { count: names.length - 4 })}`
        : "");
    const previews = lastCompleted.manifest?.items?.map((item) => item.preview || "") || [];
    $("result-preview").textContent = previews.length
      ? previews.map((preview) => preview
        ? `${t("receiverPreviewLabel")}: ${preview}`
        : t("previewOmitted")).join(" · ")
      : t("previewOmitted");
  }
  function setResultMessage(key) {
    resultMessage = key;
    $("result-message").textContent = t(key);
  }
  async function runUpload(items, revision = draftRevision) {
    if (sending) return;
    if (!items.length) {
      status("empty", "error");
      editor.focus();
      return;
    }
    if (!config) {
      status("receiveConfigUnavailable", "error");
      return;
    }
    if (items.length > config.max_items) {
      status("tooMany", "error", { count: config.max_items });
      return;
    }
    const ttlHours = currentTTL();
    if (ttlHours == null) {
      $("ttl-error").textContent = t("ttlInvalid");
      $("ttl-error").hidden = false;
      $("ttl-hours").focus();
      return;
    }
    $("ttl-error").hidden = true;
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
    transferFailure = null;
    attempt++;
    transferStartedAt = performance.now();
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
    $("transfer-attempt").textContent = [
      t("transferAttempt", { count: attempt }),
      t("resultFactItems", {
        count: items.length,
        size: sizeLabel(draftBytes(items)),
      }),
    ].join(" · ");
    $("transfer-freshness").textContent = "";
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
          ...(ttlHours !== Math.ceil((config.default_ttl_seconds || 86400) / 3600)
            ? { "X-TTL": `${ttlHours}h` }
            : {}),
        },
        {
          signal: controller.signal,
          onProgress: ({ loaded, total }) => {
            const at = performance.now();
            if (loaded < total && at - painted < 100) return;
            painted = at;
            paintTransfer(clock.update(loaded, total));
            if (total > 0 && loaded >= total && !fullySent) {
              fullySent = true;
              setTransferPhase("saving");
            }
          },
        },
      );
      if (!result?.url) throw new Error("failed");
      const resultSlug = result.slug || new URL(result.url, location.href).pathname.slice(1);
      $("share-url").value = result.url;
      $("share-code").textContent = resultSlug;
      $("copy-code").setAttribute("aria-label", t("copyCodeAria", { code: resultSlug }));
      $("open-share").href = result.url;
      $("open-share").hidden =
        access.enabled && !can("download") && !(result.public ?? $("public-share").checked);
      $("native-share").hidden = !navigator.share;
      $("transfer").hidden = true;
      hideCandidate();
      setResultFacts(
        items,
        Math.max(0, (performance.now() - transferStartedAt) / 1000),
        {
          expiresAt: result.expires_at,
          manifest: result.manifest,
          burn: result.burn_after_read ?? $("burn").checked,
          public: result.public ?? $("public-share").checked,
        },
      );
      sentLedger.unshift({
        slug: resultSlug,
        url: result.url,
        createdAt: Date.now(),
        expiresAt: result.expires_at,
        burn: result.burn_after_read ?? $("burn").checked,
        public: result.public ?? $("public-share").checked,
        openHidden: $("open-share").hidden,
      });
      sentLedger = sentLedger.slice(0, 20);
      renderSentLedger();
      $("result").hidden = false;
      if (draftRevision !== revision) {
        // The draft no longer matches the sent snapshot: keep it visible,
        // do not steal focus, and do not overwrite the clipboard.
        $("new-share").hidden = true;
        setResultMessage("sentBackground");
        status("sent", "success");
      } else {
        $("composer").hidden = true;
        setResultMessage("sentManual");
        $("result").focus({ preventScroll: true });
        status("sent", "success");
        await copyLink();
      }
    } catch (error) {
      if (error.message === "aborted") {
        setTransferPhase("canceled");
        status("transferCanceled", "error");
      } else {
        transferFailure = error;
        setTransferPhase("error", error);
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
  $("copy-code").addEventListener("click", async () => {
    const code = $("share-code").textContent;
    try {
      await copyText(code);
      status("codeCopied", "success");
    } catch {
      const range = document.createRange();
      range.selectNodeContents($("share-code"));
      const selection = getSelection();
      selection.removeAllRanges();
      selection.addRange(range);
      status("codeCopyFailed", "error");
    }
  });
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
    $("result").hidden = true;
    $("composer").hidden = false;
    attempt = 0;
    transferFailure = null;
    lastCompleted = null;
    resultMessage = null;
    $("transfer-attempt").textContent = "";
    $("transfer-freshness").textContent = "";
    $("result-facts").replaceChildren();
    $("result-items").replaceChildren();
    $("share-code").textContent = "";
    $("result-preview").textContent = "";
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
  function syncOptionPreferences() {
    const ttlHours = currentTTL();
    if (ttlHours == null) return;
    savePreferences({
      burn: $("burn").checked,
      public: access.enabled && can("upload") && !$("public-option").hidden && $("public-share").checked,
      ttlHours,
    });
    updateSendSummary();
  }
  $("burn").checked = !!preferences.burn;
  $("public-share").checked =
    access.enabled && can("upload") && !$("public-option").hidden && !!preferences.public;
  $("ttl-hours").value = clampTTLHours(preferences.ttlHours) ?? 24;
  $("burn").addEventListener("change", syncOptionPreferences);
  $("public-share").addEventListener("change", syncOptionPreferences);
  $("ttl-hours").addEventListener("input", () => {
    const ttlHours = currentTTL();
    $("ttl-error").hidden = ttlHours != null;
    if (ttlHours != null) syncOptionPreferences();
    document.querySelectorAll("[data-ttl-hours]").forEach((button) => {
      button.setAttribute("aria-pressed", String(Number(button.dataset.ttlHours) === ttlHours));
    });
  });
  document.querySelectorAll("[data-ttl-hours]").forEach((button) => {
    button.addEventListener("click", () => {
      $("ttl-hours").value = button.dataset.ttlHours;
      $("ttl-error").hidden = true;
      syncOptionPreferences();
      document.querySelectorAll("[data-ttl-hours]").forEach((preset) =>
        preset.setAttribute("aria-pressed", String(preset === button)),
      );
    });
  });
  $("suggestions").addEventListener("change", () => {
    if (preferences.suggestions) void probe(false); // Enabling is an explicit read gesture.
  });
  document.addEventListener("visibilitychange", () => {
    if (document.visibilityState !== "visible") invalidateRead();
  });
  window.addEventListener("preferenceschange", () => {
    $("burn").checked = !!preferences.burn;
    $("public-share").checked =
      access.enabled && can("upload") && !$("public-option").hidden && !!preferences.public;
    if (document.activeElement !== $("ttl-hours"))
      $("ttl-hours").value = clampTTLHours(preferences.ttlHours) ?? 24;
    document.querySelectorAll("[data-ttl-hours]").forEach((preset) =>
      preset.setAttribute("aria-pressed", String(Number(preset.dataset.ttlHours) === currentTTL())),
    );
    updateLimits();
    if (!$("result").hidden && lastCompleted)
      setResultFacts(lastCompleted.items, lastCompleted.elapsed, lastCompleted);
    if (!$("result").hidden && resultMessage)
      $("result-message").textContent = t(resultMessage);
    renderSentLedger();
    if (!preferences.suggestions) hideCandidate();
    else void probe(true);
    void renderDraft();
    // The phase text replaced the markup's i18n default; re-render it in the
    // newly selected language instead of leaving a stale translation.
    if (!$("transfer").hidden)
      setTransferPhase(
        $("transfer").dataset.phase || "uploading",
        transferFailure,
      );
  });
  async function loadConfig() {
    $("config-retry").disabled = true;
    try {
      config = await (await request("/api/v1/ui-config")).json();
      config.ttl_min_seconds ??= 3600;
      config.ttl_max_seconds ??= 604800;
      config.default_ttl_seconds ??= 86400;
      const selected = clampTTLHours(preferences.ttlHours, Math.ceil(config.ttl_min_seconds / 3600), Math.floor(config.ttl_max_seconds / 3600)) ?? 24;
      $("ttl-hours").min = String(Math.ceil(config.ttl_min_seconds / 3600));
      $("ttl-hours").max = String(Math.floor(config.ttl_max_seconds / 3600));
      $("ttl-hours").value = selected;
      document.querySelectorAll("[data-ttl-hours]").forEach((preset) =>
        preset.setAttribute("aria-pressed", String(Number(preset.dataset.ttlHours) === selected)),
      );
      $("public-option").hidden = !access.enabled || !can("upload");
      $("public-share").checked = access.enabled && can("upload") && !!preferences.public;
      updateLimits();
      $("send").disabled = false;
      $("config-recovery").hidden = true;
      await probe(true);
    } catch (error) {
      config = null;
      $("send").disabled = true;
      $("config-recovery").hidden = false;
      $("config-error").textContent = `${t("receiveConfigUnavailable")} ${t(error.message === "network" ? "network" : "failed")}`;
      failure(error);
    } finally {
      $("config-retry").disabled = false;
    }
  }
  $("config-retry").addEventListener("click", () => void loadConfig());
  await loadConfig();
}
