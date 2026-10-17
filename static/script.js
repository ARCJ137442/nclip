import { setup, failure } from "./ui.js";
import { access, can, refreshSession, setupNavigation } from "./access.js";
setup();
try {
  await refreshSession();
  setupNavigation();
  const page = document.body.dataset.page;
  if (page === "login") (await import("./account-messages.js")).loginForm();
  if (page === "compose") {
    if (access.enabled && !can("upload")) location.replace("/");
    else {
      document.getElementById("public-option").hidden = !access.enabled;
      document.getElementById("secondary-receive").hidden =
        !access.enabled || !can("download");
      if (access.enabled && can("download"))
        (await import("./receive-entry.js")).startReceiveEntry();
      await (await import("./composer.js")).startComposer();
    }
  }
  if (page === "download")
    (await import("./receive-entry.js")).startReceiveEntry();
  if (page === "admin") await (await import("./admin.js")).startAdmin();
  if (document.body.dataset.page === "receive" && document.body.dataset.slug)
    await (
      await import("./receiver.js")
    ).startReceiver(document.body.dataset.slug);
} catch (error) {
  failure(error);
}
window.addEventListener("accountfailure", (event) =>
  failure(new Error(event.detail)),
);
