import { $, button, status, failure } from "./ui.js";
import { access, can } from "./access.js";
import { request } from "./share-api.js";
import { t, sizeLabel } from "./i18n.js";
import { accountMessage } from "./account-messages.js";

export async function startAdmin() {
  let users = [],
    editing = null,
    active = "users",
    sharePage = 1,
    sharePages = 1,
    shareResult = null,
    shareRequest = 0,
    confirmAction = null,
    saving = false;
  function badge(label, extra = "") {
    const el = document.createElement("span");
    el.className = "badge " + extra;
    el.textContent = label;
    return el;
  }
  function row(title, description) {
    const el = document.createElement("article");
    el.className = "admin-row";
    const content = document.createElement("div");
    content.className = "admin-row-main";
    const name = document.createElement("strong");
    name.textContent = title;
    const sub = document.createElement("div");
    sub.className = "row-description";
    sub.textContent = description;
    content.append(name, sub);
    el.append(content);
    return { el, content };
  }
  function renderUsers() {
    const filter = $("user-search").value.toLowerCase(),
      list = $("user-list");
    list.replaceChildren();
    const selected = users.filter((user) =>
      user.username.toLowerCase().includes(filter),
    );
    $("user-count").textContent = t("userCount", { count: selected.length });
    for (const user of selected) {
      const { el, content } = row(
        user.username,
        user.disabled ? t("disabled") : t("active"),
      );
      el.dataset.userId = user.id;
      const permissions = document.createElement("div");
      permissions.className = "permission-badges";
      for (const key of ["upload", "download", "manage"])
        if (user.permissions[key])
          permissions.append(
            badge(t("permission" + key[0].toUpperCase() + key.slice(1))),
          );
      if (!permissions.children.length)
        permissions.append(badge(t("noPermissions")));
      content.append(permissions);
      const actions = document.createElement("div");
      actions.className = "row-actions";
      actions.append(
        button("edit", () => edit(user)),
        button(
          "deleteUser",
          () =>
            confirm(
              t("deleteUserPrompt", { name: user.username }),
              async () => {
                await request("/api/v1/users/" + user.id, { method: "DELETE" });
                if (user.id === access.user.id) {
                  location.assign("/login");
                  return;
                }
                await loadUsers();
                status("userDeleted", "success");
              },
            ),
          "danger",
        ),
      );
      el.append(actions);
      list.append(el);
    }
    if (!selected.length) list.textContent = t("noUsers");
  }
  async function loadUsers() {
    const response = await request("/api/v1/users");
    users = (await response.json()).items;
    renderUsers();
  }
  function edit(user = null) {
    editing = user;
    $("user-form").reset();
    $("user-form-error").textContent = "";
    $("user-dialog-title").textContent = t(user ? "editUser" : "createUser");
    $("edit-username").value = user?.username || "";
    $("edit-password").required = !user;
    $("edit-disabled").checked = !!user?.disabled;
    for (const key of ["upload", "download", "manage"])
      $("permission-" + key).checked = !!user?.permissions[key];
    $("user-dialog").showModal();
    $("edit-username").focus();
  }
  function closeEditor() {
    if (saving) return;
    $("user-dialog").close();
    $("edit-password").value = "";
  }
  function confirm(description, action) {
    confirmAction = action;
    $("confirm-description").textContent = description;
    $("confirm-error").textContent = "";
    $("confirm-dialog").showModal();
    $("confirm-cancel").focus();
  }
  function renderShares() {
    if (!shareResult) return;
    const list = $("share-list");
    list.replaceChildren();
    $("share-count").textContent = t("shareCount", {
      count: shareResult.total,
      size: sizeLabel(shareResult.total_bytes),
    });
    for (const share of shareResult.items) {
      const names =
        share.share?.items.map((item) => item.name).join(" · ") ||
        share.content_type;
      const { el, content } = row(share.id, names);
      el.dataset.shareId = share.id;
      const meta = document.createElement("div");
      meta.className = "permission-badges";
      meta.append(
        badge(sizeLabel(share.size)),
        badge(
          t(share.public ? "publicAccess" : "authorizedUsers"),
          share.public ? "public-badge" : "",
        ),
      );
      if (share.burn_after_read) meta.append(badge(t("burn")));
      const expiry = document.createElement("span");
      expiry.className = "hint";
      expiry.textContent =
        t("expires") +
        " " +
        (share.expires_at
          ? new Date(share.expires_at).toLocaleString(
              document.documentElement.lang,
            )
          : "—");
      meta.append(expiry);
      content.append(meta);
      const actions = document.createElement("div");
      actions.className = "row-actions";
      if (can("download") || share.public) {
        const link = document.createElement("a");
        link.className = "button secondary";
        link.href = "/" + share.id;
        link.textContent = t("view");
        actions.append(link);
      }
      actions.append(
        button(
          "delete",
          () =>
            confirm(t("deleteSharePrompt", { code: share.id }), async () => {
              await request("/api/v1/admin/shares/" + share.id, {
                method: "DELETE",
              });
              await loadShares();
              status("deleted", "success");
            }),
          "danger",
        ),
      );
      el.append(actions);
      list.append(el);
    }
    if (!shareResult.items.length) list.textContent = t("noShares");
    $("page-number").textContent = t("pageCount", {
      page: shareResult.page,
      pages: shareResult.pages,
    });
    $("previous-page").disabled = shareResult.page <= 1;
    $("next-page").disabled = shareResult.page >= shareResult.pages;
  }
  async function loadShares() {
    const version = ++shareRequest;
    status("loading");
    try {
      const response = await request(
        "/api/v1/admin/shares?" +
          new URLSearchParams({ page: sharePage, q: $("share-search").value }),
      );
      const result = await response.json();
      if (version !== shareRequest) return;
      shareResult = result;
      sharePage = result.page;
      sharePages = result.pages;
      renderShares();
      status("");
    } catch (error) {
      if (version === shareRequest) failure(error);
    }
  }
  async function tab(name) {
    active = name;
    for (const key of ["users", "shares"]) {
      $(key + "-tab").setAttribute("aria-selected", String(key === name));
      $(key + "-panel").hidden = key !== name;
    }
    $("new-user").hidden = name !== "users";
    if (name === "shares") await loadShares();
  }
  $("new-user").addEventListener("click", () => edit());
  $("close-user-dialog").addEventListener("click", closeEditor);
  $("user-dialog").addEventListener("cancel", (event) => {
    if (saving) event.preventDefault();
  });
  $("user-dialog").addEventListener("close", () => {
    $("edit-password").value = "";
  });
  $("user-search").addEventListener("input", renderUsers);
  for (const key of ["users", "shares"]) {
    $(key + "-tab").addEventListener("click", () => tab(key));
    $(key + "-tab").addEventListener("keydown", (event) => {
      if (["ArrowLeft", "ArrowRight", "Home", "End"].includes(event.key)) {
        event.preventDefault();
        const next =
          event.key === "Home"
            ? "users"
            : event.key === "End"
              ? "shares"
              : active === "users"
                ? "shares"
                : "users";
        $(next + "-tab").focus();
        void tab(next);
      }
    });
  }
  $("user-form").addEventListener("submit", async (event) => {
    event.preventDefault();
    if (saving) return;
    saving = true;
    $("save-user").disabled = true;
    $("save-user").setAttribute("aria-busy", "true");
    $("user-form-error").textContent = "";
    const permissions = Object.fromEntries(
      ["upload", "download", "manage"].map((key) => [
        key,
        $("permission-" + key).checked,
      ]),
    );
    try {
      const response = await request(
        "/api/v1/users" + (editing ? "/" + editing.id : ""),
        {
          method: editing ? "PUT" : "POST",
          headers: { "Content-Type": "application/json" },
          body: JSON.stringify({
            username: $("edit-username").value,
            password: $("edit-password").value,
            permissions,
            disabled: $("edit-disabled").checked,
            version: editing?.version || 0,
          }),
        },
      );
      const result = await response.json();
      $("user-dialog").close();
      if (result.user.id === access.user.id) {
        location.assign("/login");
        return;
      }
      await loadUsers();
      status("userSaved", "success");
      document
        .querySelector(`[data-user-id="${CSS.escape(result.user.id)}"] button`)
        ?.focus();
    } catch (error) {
      $("user-form-error").textContent = accountMessage(error.message);
      if (error.message === "user_changed") void loadUsers();
    } finally {
      saving = false;
      $("save-user").disabled = false;
      $("save-user").removeAttribute("aria-busy");
    }
  });
  $("confirm-cancel").addEventListener("click", () =>
    $("confirm-dialog").close(),
  );
  $("confirm-delete").addEventListener("click", async () => {
    if (!confirmAction) return;
    $("confirm-delete").disabled = true;
    try {
      await confirmAction();
      $("confirm-dialog").close();
    } catch (error) {
      $("confirm-error").textContent = accountMessage(error.message);
    } finally {
      $("confirm-delete").disabled = false;
    }
  });
  $("share-search-form").addEventListener("submit", (event) => {
    event.preventDefault();
    sharePage = 1;
    void loadShares();
  });
  $("previous-page").addEventListener("click", () => {
    if (sharePage > 1) {
      sharePage--;
      void loadShares();
    }
  });
  $("next-page").addEventListener("click", () => {
    if (sharePage < sharePages) {
      sharePage++;
      void loadShares();
    }
  });
  window.addEventListener("preferenceschange", () => {
    renderUsers();
    renderShares();
    $("user-dialog-title").textContent = t(editing ? "editUser" : "createUser");
  });
  await loadUsers();
}
