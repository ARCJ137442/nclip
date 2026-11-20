import { $, status, failure, authHeaders, renderItems } from "./ui.js";
import { t, sizeLabel } from "./i18n.js";
import { copyItem } from "./clipboard.js";
import { request, requestStream, decodeShare } from "./share-api.js";
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
    receiveController = null;
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
  }
  function setReceivePhase(phase) {
    $("receive-progress").dataset.phase = phase;
    $("receive-phase").textContent = t(
      {
        connecting: "receiveConnecting",
        downloading: "receiveDownloading",
        parsing: "receiveParsing",
      }[phase] || "receiveConnecting",
    );
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
    setReceivePhase("connecting");
    paintReceive({ received: 0, total: 0, percent: 0 });
    status(consume ? "receiving" : "loading");
    const clock = createProgress();
    clock.begin();
    const controller = new AbortController();
    receiveController = controller;
    try {
      const response = await requestStream(
        endpoint + (consume ? "/receive" : "/content"),
        consume ? { method: "POST", headers: { "X-Nclip-Confirm": "1" } } : {},
        {
          signal: controller.signal,
          onProgress: ({ received, total }) => {
            setReceivePhase("downloading");
            paintReceive(clock.update(received, total));
          },
        },
      );
      setReceivePhase("parsing");
      // Let the phase change paint before the multipart parser takes over.
      await new Promise((resolve) => requestAnimationFrame(resolve));
      const decoded = await decodeShare(response);
      items = decoded.items;
      consumed = consume && metadata.burn_after_read;
      $("receive-progress").hidden = true;
      $("receive-gate").hidden = true;
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
    } catch (error) {
      $("receive-progress").hidden = true;
      if (error.message === "aborted") {
        if (burn) {
          dead = true;
          status("oneTimeNetwork", "error");
        } else {
          status("receiveCanceled", "error");
          $("retry").hidden = false;
        }
      } else {
        failure(error);
        if (!consume) $("retry").hidden = false;
        else if (error.status === 0 || !error.status) {
          dead = true;
          status("oneTimeNetwork", "error");
        } else if (error.status === 404) {
          dead = true;
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
    // The phase replaced the markup's i18n default; re-render it in the newly
    // selected language instead of leaving a stale translation.
    if (!$("receive-progress").hidden) {
      setReceivePhase($("receive-progress").dataset.phase || "connecting");
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
