import { preferences, savePreferences } from "./preferences.js";
import { t, translate, sizeLabel } from "./i18n.js";

export const $ = (id) => document.getElementById(id);
const paths = {
  clipboard: "M9 4H6a2 2 0 0 0-2 2v14h16V6a2 2 0 0 0-2-2h-3M9 2h6v5H9z",
  settings:
    "M12 8a4 4 0 1 0 0 8 4 4 0 0 0 0-8M9 3h6l1 3 3 1 2 5-2 5-3 1-1 3H9l-1-3-3-1-2-5 2-5 3-1z",
  attach: "m8 12 7-7a3 3 0 0 1 4 4L9 19a5 5 0 0 1-7-7L12 2m-7 13 9-9",
  send: "m3 3 19 9-19 9 4-9zm4 9h15",
  copy: "M9 9h12v12H9zM5 15H3V3h12v2",
  check: "m5 12 4 4L19 6",
  file: "M14 2H4v20h16V8zm0 0v6h6",
  image: "M3 3h18v18H3zm0 14 6-7 5 6 3-3 4 5M16 7h.01",
  close: "m6 6 12 12M18 6 6 18",
  download: "M12 3v12m-5-5 5 5 5-5M4 17v4h16v-4",
};
export function icon(name) {
  const svg = document.createElementNS("http://www.w3.org/2000/svg", "svg");
  svg.setAttribute("viewBox", "0 0 24 24");
  svg.setAttribute("aria-hidden", "true");
  const path = document.createElementNS(svg.namespaceURI, "path");
  path.setAttribute("d", paths[name] || paths.file);
  svg.append(path);
  return svg;
}
export function icons(root = document) {
  root.querySelectorAll("[data-icon]").forEach((el) => {
    if (!el.querySelector("svg")) el.prepend(icon(el.dataset.icon));
  });
}
let currentStatus = null;
export function status(key, tone = "", values = {}) {
  currentStatus = { key, tone, values };
  if ($("status")) {
    $("status").textContent = key ? t(key, values) : "";
    $("status").dataset.tone = tone;
  }
}
export function failure(error, config = {}) {
  let key =
    error.status === 413
      ? "tooLarge"
      : error.status === 401 || error.status === 403
        ? "authentication"
        : error.status === 404
          ? "notFound"
          : error.status === 0
            ? "network"
            : ["unsupported", "copyFailed", "permission"].includes(
                  error.message,
                )
              ? error.message
              : "failed";
  status(key, "error", { size: sizeLabel(config.upload_max_bytes || 0) });
}
export function authHeaders() {
  const key = $("api-key")?.value.trim();
  return key ? { "X-Api-Key": key } : {};
}
export function button(key, action, style = "secondary") {
  const el = document.createElement("button");
  el.type = "button";
  el.className = `button ${style}`;
  el.textContent = t(key);
  el.dataset.i18n = key;
  el.addEventListener("click", action);
  return el;
}
export function download(blob, name) {
  const url = URL.createObjectURL(blob);
  const a = document.createElement("a");
  a.href = url;
  a.download = name;
  a.click();
  setTimeout(() => URL.revokeObjectURL(url), 30000);
}
// Each preview region owns its object URLs. Replacing a region releases only its own URLs.
const resources = new WeakMap();
export async function renderItems(
  root,
  items,
  { remove, receive = false, copy } = {},
) {
  for (const url of resources.get(root) || []) URL.revokeObjectURL(url);
  const urls = [];
  resources.set(root, urls);
  root.replaceChildren();
  for (const [index, item] of items.entries()) {
    const row = document.createElement("article");
    row.className = "item";
    const image = item.parts.find((p) => p.mime.startsWith("image/"));
    const plain = item.parts.find((p) => p.mime === "text/plain");
    const rich = item.parts.some((p) => p.mime === "text/html");
    row.append(
      icon(image ? "image" : item.kind === "file" ? "file" : "clipboard"),
    );
    const content = document.createElement("div");
    content.className = "item-content";
    const title = document.createElement("div");
    title.className = "item-title";
    title.textContent =
      item.kind === "file"
        ? item.name
        : t(image ? "image" : rich ? "rich" : "text");
    content.append(title);
    const description = document.createElement("div");
    description.className = "item-description";
    description.textContent = `${sizeLabel(item.parts.reduce((n, p) => n + p.blob.size, 0))} · ${item.parts.map((p) => p.mime).join(" / ")}`;
    content.append(description);
    if (plain) {
      const pre = document.createElement("pre");
      content.append(pre);
      pre.textContent = (await plain.blob.text()).slice(
        0,
        receive ? 100000 : 400,
      );
    }
    if (
      image &&
      ["image/png", "image/jpeg", "image/gif", "image/webp"].includes(
        image.mime,
      )
    ) {
      const img = document.createElement("img");
      const url = URL.createObjectURL(image.blob);
      urls.push(url);
      img.src = url;
      img.alt = item.name;
      content.append(img);
    }
    if (receive) {
      const actions = document.createElement("div");
      actions.className = "item-actions";
      if (plain || rich || item.parts.some((p) => p.mime === "image/png"))
        actions.append(button("copy", () => copy(item)));
      if (rich && plain)
        actions.append(button("copyPlain", () => copy(item, true)));
      const preferred =
        item.parts.find((p) => p.mime === "text/html") || item.parts[0];
      actions.append(
        button("download", () =>
          download(preferred.blob, rich ? "content.html" : item.name),
        ),
      );
      content.append(actions);
    }
    row.append(content);
    if (remove) {
      const b = button("remove", () => remove(index), "remove");
      b.replaceChildren(icon("close"));
      b.removeAttribute("data-i18n");
      b.setAttribute("aria-label", `${t("remove")} ${item.name}`);
      row.append(b);
    }
    if (resources.get(root) !== urls) {
      for (const url of urls) URL.revokeObjectURL(url);
      return;
    }
    root.append(row);
  }
}
function refresh() {
  translate();
  icons();
  document
    .querySelectorAll("[data-help]")
    .forEach((el) => el.setAttribute("aria-label", t(el.dataset.help)));
  if (currentStatus)
    status(currentStatus.key, currentStatus.tone, currentStatus.values);
}
export function setup() {
  refresh();
  for (const name of ["language", "theme"]) {
    $(name).value = preferences[name];
    $(name).addEventListener("change", () =>
      savePreferences({ [name]: $(name).value }),
    );
  }
  $("suggestions").checked = !!preferences.suggestions;
  $("suggestions").addEventListener("change", () =>
    savePreferences({ suggestions: $("suggestions").checked }),
  );
  window.addEventListener("preferenceschange", refresh);
  document.addEventListener("keydown", (e) => {
    if (e.key === "Escape") $("settings").open = false;
  });
  document.addEventListener("click", (e) => {
    if (!$("settings").contains(e.target)) $("settings").open = false;
  });
  window.addEventListener("pagehide", () => {
    document.querySelectorAll(".items").forEach((root) => {
      for (const url of resources.get(root) || []) URL.revokeObjectURL(url);
    });
  });
}
