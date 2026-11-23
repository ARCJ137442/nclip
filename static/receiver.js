import { $, status, failure, authHeaders, renderItems } from "./ui.js";
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
    $("receive-outcome-facts").textContent = [
      t("receiveOutcome", {
        size: sizeLabel(receiveStats.size),
        time: etaLabel(receiveStats.elapsed),
      }),
      receiveStats.consumed ? t("receiveCopyRemoved") : t("receiveCopyKept"),
    ].join(" · ");
  }
  function retainedSize() {
    return retained.reduce((sum, chunk) => sum + chunk.byteLength, 0);
  }
  function updateResumeHint() {
    const size = retainedSize();
    const visible =
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
    // A one-time transfer cannot be resumed; offering cancel would only
    // invite data loss. Ordinary shares keep their server copy.
    $("receive-cancel").hidden = burn;
    $("receive-note").hidden = !burn;
    if (burn) $("receive-note").textContent = t("receiveOneTimeNote");
    $("receive-progress").hidden = false;
    $("receive-resume").hidden = true;
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
    try {
      const meta = await requestBytes(
        endpoint + (consume ? "/receive" : "/content"),
        consume ? { method: "POST", headers: { "X-Nclip-Confirm": "1" } } : {},
        {
          signal: controller.signal,
          start: consume ? 0 : retainedSize(),
          onReset: () => {
            retained = [];
          },
          onChunk: (chunk) => {
            if (chunk?.byteLength) retained.push(chunk);
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
      const body = new Blob(retained, { type: meta.contentType });
      const decoded = await decodeShare(
        new Response(body, {
          headers: { "Content-Type": meta.contentType },
        }),
      );
      retained = [];
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
      };
      renderReceiveOutcome();
    } catch (error) {
      if (error.message === "aborted") {
        failReceiveStage("canceled");
        if (burn) {
          retained = [];
          await recheckOneTime(error);
        } else {
          status("receiveCanceled", "error");
          $("retry").hidden = false;
          updateResumeHint();
        }
      } else {
        failReceiveStage("error");
        failure(error);
        if (!consume) {
          $("retry").hidden = false;
          if (downloaded) retained = [];
          updateResumeHint();
          if (receiveResumable && retained.length) {
            status("receiveWillResume", "error", {
              size: sizeLabel(retainedSize()),
            });
          }
        }
        else if (error.status === 0 || !error.status) {
          retained = [];
          await recheckOneTime(error);
        } else if (error.status === 404) {
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
  $("receive").addEventListener("click", () => load(true));
  $("receive-cancel").addEventListener("click", () =>
    receiveController?.abort(),
  );
  $("retry").addEventListener("click", () => load());
  $("delete-share").addEventListener("click", async () => {
    if (!confirm(t("deleteConfirm"))) return;
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
    renderReceiveOutcome();
    // The phase replaced the markup's i18n default; re-render it in the newly
    // selected language instead of leaving a stale translation.
    if (!$("receive-progress").hidden) {
      setReceivePhase(receivePhase || "connecting");
      if (metadata?.burn_after_read && !$("receive-note").hidden)
        $("receive-note").textContent = t("receiveOneTimeNote");
    }
  });
  window.addEventListener("pagehide", () => {
    if (bundleURL) URL.revokeObjectURL(bundleURL);
    items = null;
  });
  metadata = await (await request(endpoint)).json();
  $("delete-share").hidden = !can("manage");
  details();
  if (metadata.burn_after_read) {
    $("receive-gate").hidden = false;
    status("");
  } else await load();
}
