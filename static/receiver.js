import { $, status, failure, authHeaders, renderItems, icon } from "./ui.js";
import { t, sizeLabel } from "./i18n.js";
import { copyItem } from "./clipboard.js";
import { request, requestBytes, decodeShare } from "./share-api.js";
import { createProgress, timeParts } from "./transfer.js";
import { access, can } from "./access.js";

export function adaptReceiverNavigation() {
  let path = "/",
    key = "backHome";
  if (!access.enabled || can("upload")) key = "newShare";
  else if (can("download")) {
    path = "/receive";
    key = "receiveAnother";
  } else if (can("manage")) {
    path = "/admin";
    key = "administration";
  }
  document.querySelectorAll("[data-share-home]").forEach((link) => {
    link.href = path;
    link.dataset.i18n = key;
    link.textContent = t(key);
  });
}

export async function startReceiver(slug) {
  let metadata = null,
    items = null,
    busy = false,
    consumed = false,
    dead = false,
    bundleURL = null,
    receiveController = null,
    receiveStartedAt = 0,
    receiveAttemptStartedAt = 0,
    receivePhase = "",
    receiveAttempt = 0,
    receiveStats = null,
    retained = [],
    receiveResumable = false;
  const endpoint = `/api/v1/shares/${encodeURIComponent(slug)}`;
  function etaLabel(seconds) {
    const part = timeParts(seconds);
    if (!part) return "";
    return t(part.unit === "minutes" ? "timeMinutes" : "timeSeconds", {
      count: part.count,
    });
  }
  function paintReceive({ received, total, percent, rate, eta }) {
    const track = $("receive-track");
    $("receive-percent").textContent =
      percent == null ? "" : `${Math.floor(percent)}%`;
    $("receive-fill").style.width = percent == null ? "" : `${percent}%`;
    track.dataset.unknown = String(percent == null);
    // An indeterminate bar must not claim a value; only remove the attribute.
    if (percent == null) track.removeAttribute("aria-valuenow");
    else track.setAttribute("aria-valuenow", String(Math.floor(percent)));
    $("receive-bytes").textContent =
      total > 0
        ? t("transferBytes", {
            sent: sizeLabel(received),
            total: sizeLabel(total),
          })
        : sizeLabel(received);
    $("receive-rate").textContent =
      rate > 0
        ? `${t("transferRate", { rate: sizeLabel(rate) })}${
            eta != null ? ` · ${t("transferEta", { time: etaLabel(eta) })}` : ""
          }`
        : "";
    const elapsed = (performance.now() - receiveAttemptStartedAt) / 1000;
    $("receive-freshness").textContent =
      elapsed >= 1
        ? t("transferElapsed", { time: etaLabel(elapsed) })
        : "";
  }
  function paintReceiveStages(phase) {
    const order = ["connecting", "downloading", "verifying", "ready"];
    const active = order.indexOf(phase);
    for (const [index, stage] of order.entries()) {
      const item = $("receive-stages").querySelector(
        `[data-stage="${stage}"]`,
      );
      if (!item) continue;
      const state =
        phase === "ready"
          ? "done"
          : index < active
            ? "done"
            : index === active
              ? "active"
              : "pending";
      item.dataset.state = state;
      if (state === "active") item.setAttribute("aria-current", "step");
      else item.removeAttribute("aria-current");
    }
  }
  function failReceiveStage(state) {
    const item = $("receive-stages").querySelector(
      `[data-stage="${receivePhase || "connecting"}"]`,
    );
    if (item) {
      item.dataset.state = state;
      item.removeAttribute("aria-current");
    }
  }
  function setReceivePhase(phase) {
    receivePhase = phase;
    $("receive-progress").dataset.phase = phase;
    $("receive-phase").textContent = t(
      {
        connecting: "receiveConnecting",
        downloading: "receiveDownloading",
        parsing: "receiveParsing",
      }[phase] || "receiveConnecting",
    );
    paintReceiveStages(phase);
  }
  async function copy(item, plainOnly = false) {
    try {
      const result = await copyItem(item, { plainOnly });
      status(result.plain ? "plainFallback" : "copied", "success");
    } catch (error) {
      status("copyFailed", "error");
    }
  }
  async function render() {
    if (items)
      await renderItems($("received-items"), items, { receive: true, copy });
  }
  function details() {
    if (!metadata) return;
    $("receive-subtitle").textContent =
      `${metadata.manifest.items.length} · ${sizeLabel(metadata.manifest.items.reduce((sum, item) => sum + item.parts.reduce((n, p) => n + p.size, 0), 0))}`;
    const root = $("metadata-values");
    root.replaceChildren();
    for (const [label, value] of [
      ["ID", slug],
      [t("created"), metadata.created_at],
      [t("expires"), metadata.expires_at],
    ]) {
      const dt = document.createElement("dt");
      dt.textContent = label;
      const dd = document.createElement("dd");
      dd.textContent =
        label === "ID"
          ? value
          : value
            ? new Date(value).toLocaleString(document.documentElement.lang)
            : "—";
      root.append(dt, dd);
    }
    const title =
      metadata.burn_after_read && !consumed ? "oneTimeTitle" : "contents";
    $("receive-title").dataset.i18n = title;
    $("receive-title").textContent = t(title);
    if (consumed) $("receive-subtitle").textContent += ` · ${t("received")}`;
  }
  function renderReceiveOutcome() {
    if (!receiveStats) {
      $("receive-outcome").hidden = true;
      return;
    }
    $("receive-outcome").hidden = false;
    $("receive-outcome-title").textContent = t("receiveReadyTitle");
    const facts = [
      t("receiveOutcome", {
        size: sizeLabel(receiveStats.size),
        time: etaLabel(receiveStats.elapsed),
      }),
    ];
    if (receiveStats.consumed) facts.push(t("receiveCopyRemoved"));
    else {
      facts.push(t("receiveCopyKept"));
      facts.push(t("receiveWillRemain", {
        date: receiveStats.expiresAt
          ? new Date(receiveStats.expiresAt).toLocaleString(document.documentElement.lang)
          : t("expiryUnknown"),
      }));
    }
    $("receive-outcome-facts").textContent = facts.join(" · ");
  }
  function confirmDeleteShare() {
    return new Promise((resolve) => {
      const dialog = $("receive-delete-dialog");
      $("receive-delete-title").textContent = t("confirmDelete");
      $("receive-delete-description").textContent = t("deleteConfirm");
      $("receive-delete-error").textContent = "";
      let settled = false;
      const finish = (value) => {
        if (settled) return;
        settled = true;
        dialog.removeEventListener("close", onClose);
        $("receive-delete-cancel").removeEventListener("click", onCancel);
        $("receive-delete-confirm").removeEventListener("click", onConfirm);
        if (dialog.open) dialog.close();
        resolve(value);
      };
      const onCancel = () => finish(false);
      const onConfirm = () => finish(true);
      const onClose = () => finish(false);
      dialog.addEventListener("close", onClose);
      $("receive-delete-cancel").addEventListener("click", onCancel);
      $("receive-delete-confirm").addEventListener("click", onConfirm);
      dialog.showModal();
      $("receive-delete-cancel").focus();
    });
  }
  function manifestTotals() {
    const manifest = metadata?.manifest;
    const size = manifest?.items?.reduce(
      (sum, item) => sum + item.parts.reduce((partSum, part) => partSum + part.size, 0),
      0,
    ) || 0;
    return { count: manifest?.items?.length || 0, size };
  }
  function renderReceiveGate() {
    if (!metadata) return;
    const total = manifestTotals();
    const oneTime = !!metadata.burn_after_read;
    const expiry = metadata.expires_at
      ? new Date(metadata.expires_at).toLocaleString(document.documentElement.lang)
      : t("expiryUnknown");
    $("receive-gate-title").textContent = t(oneTime ? "oneTimeTitle" : "receiveStart");
    $("receive-gate-hint").textContent = t(oneTime ? "receiveOneTimeLocked" : "receiveGateHint");
    const actionLabel = $("receive").querySelector("[data-i18n]");
    actionLabel.dataset.i18n = oneTime ? "receive" : "receiveStart";
    actionLabel.textContent = t(actionLabel.dataset.i18n);
    $("receive-gate-total").textContent = t("resultFactItems", {
      count: total.count,
      size: sizeLabel(total.size),
    });
    $("receive-gate-expires").textContent = t("receiveTtl") + ": " + expiry;
    const list = $("receive-gate-items");
    list.replaceChildren();
    for (const item of metadata.manifest.items) {
      const row = document.createElement("li");
      row.prepend(icon(item.kind === "file" ? "file" : "clipboard"));
      const kind = document.createElement("strong");
      kind.textContent = item.kind === "file" ? t("file") : t("clipboardItem");
      const name = document.createElement("span");
      name.textContent = item.name;
      const size = item.parts.reduce((sum, part) => sum + part.size, 0);
      const detail = document.createElement("span");
      detail.textContent = sizeLabel(size);
      row.append(kind, name, detail);
      if (item.preview) {
        const preview = document.createElement("small");
        preview.className = "gate-preview";
        preview.textContent = `${t("receiverPreviewLabel")}: ${item.preview}`;
        row.append(preview);
      }
      list.append(row);
    }
  }
  function retainedSize() {
    return retained.reduce((sum, chunk) => sum + chunk.byteLength, 0);
  }
  // Keep receive resumability policy in one place. P1 will define whether a
  // locked one-time delivery token permits same-page continuation.
  function canResumeCurrentShare() {
    return !metadata?.burn_after_read;
  }
  function updateResumeHint() {
    const size = retainedSize();
    const visible =
      canResumeCurrentShare() &&
      receiveResumable &&
      size > 0 &&
      !["ready", "parsing"].includes(receivePhase);
    $("receive-resume").hidden = !visible;
    if (!visible) return;
    $("receive-resume").textContent =
      receivePhase === "error"
        ? t("receiveWillResume", { size: sizeLabel(size) })
        : t("receiveResumeReady", { size: sizeLabel(size) });
  }
  async function recheckOneTime(error) {
    try {
      const current = await (await request(endpoint)).json();
      if (
        !current.burn_after_read ||
        (current.expires_at && Date.now() >= Date.parse(current.expires_at))
      )
        throw new Error("not_found");
      metadata = current;
      dead = false;
      $("receive-gate").hidden = false;
      $("receive").disabled = false;
      status("oneTimeRetryAvailable", "error", { reason: error.message });
    } catch {
      dead = true;
      $("receive-gate").hidden = true;
      $("receive").disabled = true;
      status("oneTimeGone", "error");
    }
  }
  async function load(consume = false) {
    if (busy || dead || consumed) return;
    busy = true;
    $("receive").disabled = true;
    $("receive").setAttribute("aria-busy", "true");
    $("retry").hidden = true;
    const burn = !!metadata?.burn_after_read;
    // P0 exposes cancellation only where the server copy remains available.
    $("receive-cancel").hidden = burn;
    $("receive-note").hidden = !burn;
    if (burn) $("receive-note").textContent = t("receiveOneTimeNote");
    $("receive-progress").hidden = false;
    $("receive-resume").hidden = true;
    $("receive-gate").hidden = true;
    receiveStats = null;
    renderReceiveOutcome();
    receiveAttempt++;
    receiveStartedAt = receiveAttemptStartedAt = performance.now();
    const expectedSize = metadata?.manifest?.items?.reduce(
      (sum, item) =>
        sum + item.parts.reduce((total, part) => total + part.size, 0),
      0,
    );
    $("receive-attempt").textContent = [
      t("receiveAttempt", { count: receiveAttempt }),
      expectedSize > 0 ? sizeLabel(expectedSize) : "",
    ]
      .filter(Boolean)
      .join(" · ");
    $("receive-freshness").textContent = "";
    setReceivePhase("connecting");
    const retainedBytes = consume ? 0 : retainedSize();
    paintReceive({ received: retainedBytes, total: 0, percent: null });
    status(
      consume ? "receiving" : retainedBytes > 0 ? "receiveResuming" : "loading",
    );
    const clock = createProgress();
    clock.begin();
    const controller = new AbortController();
    receiveController = controller;
    let downloaded = false;
    const responseChunks = [];
    try {
      const meta = await requestBytes(
        endpoint + (consume ? "/receive" : "/content"),
        consume ? { method: "POST", headers: { "X-Nclip-Confirm": "1" } } : {},
        {
          signal: controller.signal,
          start: !consume && canResumeCurrentShare() ? retainedSize() : 0,
          onReset: () => {
            retained = [];
            responseChunks.length = 0;
          },
          onChunk: (chunk) => {
            // Every attempt needs its own response bytes to parse. Only a
            // failed, resumable ordinary attempt moves these into `retained`.
            if (chunk?.byteLength) responseChunks.push(chunk);
          },
          onProgress: ({ received, total, resumable }) => {
            receiveResumable = resumable;
            setReceivePhase("downloading");
            paintReceive(clock.update(received, total));
            updateResumeHint();
          },
        },
      );
      downloaded = true;
      clock.begin();
      receiveResumable = meta.resumable;
      setReceivePhase("parsing");
      // Let the phase change paint before the multipart parser takes over.
      await new Promise((resolve) => requestAnimationFrame(resolve));
      const body = new Blob([...retained, ...responseChunks], {
        type: meta.contentType,
      });
      const decoded = await decodeShare(
        new Response(body, {
          headers: { "Content-Type": meta.contentType },
        }),
      );
      retained = [];
      responseChunks.length = 0;
      items = decoded.items;
      consumed = consume && metadata.burn_after_read;
      $("receive-progress").hidden = true;
      $("receive-resume").hidden = true;
      $("receive-gate").hidden = true;
      setReceivePhase("ready");
      await render();
      details();
      $("delete-share").hidden = consumed || !can("manage");
      $("download-all").hidden = metadata.manifest.encoding !== "zip-v1";
      if (consumed) {
        if (decoded.archive instanceof Blob) {
          bundleURL = URL.createObjectURL(decoded.archive);
          $("download-all").href = bundleURL;
          $("download-all").download = `${slug}.zip`;
        }
        status("received", "success");
        if (items.length === 1) {
          try {
            await copyItem(items[0]);
            status("receivedCopied", "success");
          } catch {
            /* Per-item copy remains available with a fresh gesture. */
          }
        }
      } else status("");
      const contentSize = items.reduce(
        (sum, item) =>
          sum + item.parts.reduce((total, part) => total + part.blob.size, 0),
        0,
      );
      receiveStats = {
        size: contentSize,
        elapsed: Math.max(
          0,
          (performance.now() - receiveStartedAt) / 1000,
        ),
        consumed,
        expiresAt: metadata.expires_at,
      };
      renderReceiveOutcome();
    } catch (error) {
      if (error.message === "aborted") {
        failReceiveStage("canceled");
        if (!canResumeCurrentShare()) {
          retained = [];
          responseChunks.length = 0;
          await recheckOneTime(error);
        } else {
          retained.push(...responseChunks);
          status("receiveCanceled", "error");
          $("retry").hidden = false;
          updateResumeHint();
        }
      } else {
        failReceiveStage("error");
        failure(error);
        if (!consume) {
          $("retry").hidden = false;
          if (downloaded) {
            retained = [];
            responseChunks.length = 0;
          } else retained.push(...responseChunks);
          updateResumeHint();
          if (receiveResumable && retained.length) {
            status("receiveWillResume", "error", {
              size: sizeLabel(retainedSize()),
            });
          }
        }
        else if (error.status === 0 || !error.status) {
          retained = [];
          responseChunks.length = 0;
          await recheckOneTime(error);
        } else if (error.status === 404) {
          retained = [];
          responseChunks.length = 0;
          dead = true;
          $("receive-gate").hidden = true;
          status("oneTimeGone", "error");
        }
      }
    } finally {
      receiveController = null;
      busy = false;
      $("receive").disabled = dead;
      $("receive").removeAttribute("aria-busy");
    }
  }
  $("receive").addEventListener("click", () => load(!!metadata?.burn_after_read));
  $("receive-cancel").addEventListener("click", () =>
    receiveController?.abort(),
  );
  $("retry").addEventListener("click", () => load());
  $("delete-share").addEventListener("click", async () => {
    if (!(await confirmDeleteShare())) return;
    $("delete-share").disabled = true;
    status("deleting");
    try {
      await request(`/${encodeURIComponent(slug)}`, {
        method: "DELETE",
        headers: authHeaders(),
      });
      dead = true;
      items = null;
      receiveStats = null;
      renderReceiveOutcome();
      $("received-items").replaceChildren();
      $("receive-gate").hidden = true;
      $("delete-share").hidden = true;
      $("download-all").hidden = true;
      status("deleted", "success");
    } catch (error) {
      failure(error);
      $("delete-share").disabled = false;
    }
  });
  window.addEventListener("preferenceschange", () => {
    details();
    void render();
    renderReceiveGate();
    renderReceiveOutcome();
    // The phase replaced the markup's i18n default; re-render it in the newly
    // selected language instead of leaving a stale translation.
    if (!$("receive-progress").hidden) {
      setReceivePhase(receivePhase || "connecting");
      if (metadata?.burn_after_read && !$("receive-note").hidden)
        $("receive-note").textContent = t("receiveOneTimeNote");
    }
  });
  window.addEventListener("pagehide", (event) => {
    if (!event.persisted) {
      if (bundleURL) URL.revokeObjectURL(bundleURL);
      items = null;
    }
  });
  window.addEventListener("pageshow", () => {
    if (consumed && items) void render();
  });
  window.addEventListener("beforeunload", (event) => {
    if (!metadata?.burn_after_read || !busy) return;
    event.preventDefault();
    event.returnValue = "";
  });
  metadata = await (await request(endpoint)).json();
  $("delete-share").hidden = !can("manage");
  details();
  renderReceiveGate();
  const total = manifestTotals();
  const needsPreflight = metadata.burn_after_read || total.size > 8 * 1024 * 1024 || total.count >= 3;
  if (needsPreflight) {
    $("receive-gate").hidden = false;
    status("");
  } else await load();
}
