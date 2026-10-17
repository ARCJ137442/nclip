import { setup, failure } from "./ui.js";
setup();
try {
  if (document.body.dataset.page === "compose")
    await (await import("./composer.js")).startComposer();
  if (document.body.dataset.page === "receive" && document.body.dataset.slug)
    await (
      await import("./receiver.js")
    ).startReceiver(document.body.dataset.slug);
} catch (error) {
  failure(error);
}
