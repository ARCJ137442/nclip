import { $, button, status, failure } from "./ui.js";
import { access, can } from "./access.js";
import { request } from "./share-api.js";
import { t, sizeLabel } from "./i18n.js";
import { accountMessage } from "./account-messages.js";

export async function startAdmin() {
  let users = [],
    editing = null,
    active = "users",
    uploadSettings = null,
    uploadDraftDirty = false,
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
  function setUploadDraft(value) {
    $("upload-limit-number").value = value;
    $("upload-limit-range").value = value;
    uploadDraftDirty = true;
    $("upload-settings-error").textContent = "";
  }
  function validUploadDraft() {
    const value = Number($("upload-limit-number").value);
    return Number.isInteger(value) && value >= 1 && value <= (uploadSettings?.ceiling_mib || 100);
  }
  async function loadUploadSettings({ discardDraft = false } = {}) {
    if (uploadDraftDirty && !discardDraft) return;
    $("upload-settings-state").textContent = t("uploadSettingsLoading");
    $("reload-upload-settings").hidden = true;
    try {
      const response = await request("/api/v1/settings/upload");
      const loaded = await response.json();
      uploadSettings = loaded;
      const value = uploadSettings.upload_limit_mib;
      $("upload-limit-number").max = uploadSettings.ceiling_mib;
      $("upload-limit-range").max = uploadSettings.ceiling_mib;
      $("upload-limit-number").value = value;
      $("upload-limit-range").value = value;
      $("upload-limit-hint").textContent = t("uploadLimitCeiling", {
        size: `${uploadSettings.ceiling_mib} MiB`,
      });
      uploadDraftDirty = false;
      $("reload-upload-settings").hidden = true;
      $("upload-settings-state").textContent = t("uploadSettingsLoaded", {
        size: `${value} MiB`,
      });
    } catch (error) {
      $("upload-settings-state").textContent = "";
      $("upload-settings-error").textContent =
        error.message === "settings_unavailable"
          ? t("uploadSettingsUnavailable")
          : accountMessage(error.message);
      $("reload-upload-settings").hidden = false;
    }
  }
  async function tab(name) {
    active = name;
    for (const key of ["users", "shares", "settings"]) {
      $(key + "-tab").setAttribute("aria-selected", String(key === name));
      $(key + "-panel").hidden = key !== name;
    }
    $("new-user").hidden = name !== "users";
    if (name === "shares") await loadShares();
    if (name === "settings") await loadUploadSettings();
  }
  $("upload-limit-number").addEventListener("input", () => {
    const value = Number($("upload-limit-number").value);
    if (Number.isInteger(value)) $("upload-limit-range").value = value;
    uploadDraftDirty = true;
    $("upload-settings-error").textContent = "";
  });
  $("upload-limit-range").addEventListener("input", () =>
    setUploadDraft($("upload-limit-range").value),
  );
  document.querySelectorAll("[data-upload-limit]").forEach((preset) =>
    preset.addEventListener("click", () => setUploadDraft(preset.dataset.uploadLimit)),
  );
  $("reload-upload-settings").addEventListener("click", () => {
    $("upload-settings-error").textContent = "";
    void loadUploadSettings({ discardDraft: true });
  });
  $("upload-settings-form").addEventListener("submit", async (event) => {
    event.preventDefault();
    if (!uploadSettings || !validUploadDraft() || saving) {
      $("upload-settings-error").textContent = t("invalidRequest");
      return;
    }
    saving = true;
    $("save-upload-settings").disabled = true;
    $("save-upload-settings").setAttribute("aria-busy", "true");
    $("upload-settings-error").textContent = "";
    try {
      const response = await request("/api/v1/settings/upload", {
        method: "PUT",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({
          upload_limit_mib: Number($("upload-limit-number").value),
          revision: uploadSettings.revision,
        }),
      });
      uploadSettings = await response.json();
      uploadDraftDirty = false;
      $("upload-limit-number").max = uploadSettings.ceiling_mib;
      $("upload-limit-range").max = uploadSettings.ceiling_mib;
      $("upload-limit-number").value = uploadSettings.upload_limit_mib;
      $("upload-limit-range").value = uploadSettings.upload_limit_mib;
      $("upload-limit-hint").textContent = t("uploadLimitCeiling", {
        size: `${uploadSettings.ceiling_mib} MiB`,
      });
      $("upload-settings-state").textContent = t("uploadSettingsSaved");
    } catch (error) {
      if (error.message === "upload_limit_changed") {
        try {
          const response = await request("/api/v1/settings/upload");
          const current = await response.json();
          $("upload-settings-state").textContent = t("uploadSettingsLoaded", {
            size: `${current.upload_limit_mib} MiB`,
          });
        } catch {
          /* Keep the draft available even if refreshing the revision fails. */
        }
        $("reload-upload-settings").hidden = false;
        $("upload-settings-error").textContent = t("uploadSettingsChanged");
      } else {
        $("upload-settings-error").textContent =
          error.message === "upload_limit_out_of_range"
            ? t("invalidRequest")
            : t("uploadSettingsFailed");
      }
    } finally {
      saving = false;
      $("save-upload-settings").disabled = false;
      $("save-upload-settings").removeAttribute("aria-busy");
    }
  });
  $("new-user").addEventListener("click", () => edit());
  $("close-user-dialog").addEventListener("click", closeEditor);
  $("user-dialog").addEventListener("cancel", (event) => {
    if (saving) event.preventDefault();
  });
  $("user-dialog").addEventListener("close", () => {
    $("edit-password").value = "";
  });
  $("user-search").addEventListener("input", renderUsers);
  const tabs = ["users", "shares", "settings"];
  for (const key of tabs) {
    $(key + "-tab").addEventListener("click", () => tab(key));
    $(key + "-tab").addEventListener("keydown", (event) => {
      if (["ArrowLeft", "ArrowRight", "Home", "End"].includes(event.key)) {
        event.preventDefault();
        const index = tabs.indexOf(active);
        const next =
          event.key === "Home"
            ? tabs[0]
            : event.key === "End"
              ? tabs[tabs.length - 1]
              : tabs[(index + (event.key === "ArrowRight" ? 1 : tabs.length - 1)) % tabs.length];
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
    if (uploadSettings && !uploadDraftDirty)
      $("upload-settings-state").textContent = t("uploadSettingsLoaded", {
        size: `${uploadSettings.upload_limit_mib} MiB`,
      });
    if ($("upload-limit-hint"))
      $("upload-limit-hint").textContent = t("uploadLimitCeiling", {
        size: `${uploadSettings?.ceiling_mib || 100} MiB`,
      });
  });
  await loadUsers();
}
