// Only preference settings persist in localStorage. Identity comes from the
// server; authentication stays in an HttpOnly cookie.
export const access = { enabled: false, user: null, csrf_token: "" };
export function can(permission) {
  return !access.enabled || !!access.user?.permissions?.[permission];
}
export function csrfHeaders() {
  return access.enabled && access.csrf_token
    ? { "X-CSRF-Token": access.csrf_token }
    : {};
}
export async function refreshSession() {
  const response = await fetch("/api/v1/session", {
    credentials: "same-origin",
    cache: "no-store",
  });
  if (!response.ok) throw new Error("sessionFailed");
  const state = await response.json();
  Object.assign(access, {
    enabled: !!state.enabled,
    user: state.user || null,
    csrf_token: state.csrf_token || "",
  });
  window.dispatchEvent(new Event("sessionchange"));
  return access;
}
export function setupNavigation() {
  const id = (name) => document.getElementById(name);
  function render() {
    document.body.dataset.accounts = String(access.enabled);
    id("account-controls").hidden = !access.enabled;
    id("account-menu").hidden = !access.user;
    id("sign-in").hidden =
      !!access.user || document.body.dataset.page === "login";
    id("sign-in").href =
      "/login?next=" + encodeURIComponent(location.pathname + location.search);
    id("account-name").textContent = access.user?.username || "";
    let visible = 0;
    for (const [name, permission] of [
      ["upload", "upload"],
      ["download", "download"],
      ["admin", "manage"],
    ]) {
      const link = id("nav-" + name);
      link.hidden = !access.enabled || !access.user || !can(permission);
      if (!link.hidden) visible++;
      const page = document.body.dataset.page;
      const active =
        (name === "upload" && page === "compose") ||
        (name === "download" && ["download", "receive"].includes(page)) ||
        (name === "admin" && page === "admin");
      if (active) link.setAttribute("aria-current", "page");
      else link.removeAttribute("aria-current");
    }
    id("task-nav").hidden = visible < 2;
  }
  render();
  window.addEventListener("sessionchange", render);
  document.addEventListener("click", (event) => {
    if (!id("account-menu").contains(event.target))
      id("account-menu").open = false;
  });
  document.addEventListener("keydown", (event) => {
    if (event.key === "Escape") id("account-menu").open = false;
  });
  id("logout").addEventListener("click", async () => {
    id("logout").disabled = true;
    try {
      const response = await fetch("/api/v1/logout", {
        method: "POST",
        credentials: "same-origin",
        headers: csrfHeaders(),
      });
      if (!response.ok) throw new Error("sessionFailed");
      location.assign("/login");
    } catch {
      window.dispatchEvent(
        new CustomEvent("accountfailure", { detail: "sessionFailed" }),
      );
      id("logout").disabled = false;
    }
  });
}
