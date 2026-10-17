import { $, status, failure, authHeaders, renderItems } from "./ui.js";
import { t, sizeLabel } from "./i18n.js";
import { copyItem } from "./clipboard.js";
import { request, decodeShare } from "./share-api.js";

export async function startReceiver(slug) {
  let metadata = null,
    items = null,
    busy = false,
    consumed = false,
    dead = false,
    bundleURL = null;
  const endpoint = `/api/v1/shares/${encodeURIComponent(slug)}`;
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
    status(consume ? "receiving" : "loading");
    try {
      const response = await request(
        endpoint + (consume ? "/receive" : "/content"),
        consume ? { method: "POST", headers: { "X-Nclip-Confirm": "1" } } : {},
      );
      const decoded = await decodeShare(response);
      items = decoded.items;
      consumed = consume && metadata.burn_after_read;
      $("receive-gate").hidden = true;
      await render();
      details();
      $("delete-share").hidden = consumed;
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
      failure(error);
      if (!consume) $("retry").hidden = false;
      else if (error.status === 0 || !error.status) {
        dead = true;
        status("oneTimeNetwork", "error");
      } else if (error.status === 404) {
        dead = true;
      }
    } finally {
      busy = false;
      $("receive").disabled = dead;
      $("receive").removeAttribute("aria-busy");
    }
  }
  $("receive").addEventListener("click", () => load(true));
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
  });
  window.addEventListener("pagehide", () => {
    if (bundleURL) URL.revokeObjectURL(bundleURL);
    items = null;
  });
  metadata = await (await request(endpoint)).json();
  details();
  if (metadata.burn_after_read) {
    $("receive-gate").hidden = false;
    status("");
  } else await load();
}
