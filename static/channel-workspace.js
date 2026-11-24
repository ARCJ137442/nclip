import { t } from "./i18n.js";
import { preferences, savePreferences } from "./preferences.js";

const mode = document.querySelector("#channel-mode");
if (mode) {
  const openButton = document.querySelector("#channel-open");
  const publicToggle = document.querySelector("#channel-public");
  const publicOnly = document.querySelector("#channel-public-only");
  const panel = document.querySelector("#channel-sender");
  const statusLine = document.querySelector("#channel-sender-state");
  const link = document.querySelector("#channel-link");
  const identity = document.querySelector("#channel-identity");
  const decisions = document.querySelector("#channel-decisions");
  const errorLine = document.querySelector("#channel-error");
  const admitButton = document.querySelector("#channel-admit");
  const rejectButton = document.querySelector("#channel-reject");
  const closeButton = document.querySelector("#channel-close");
  const copyButton = document.querySelector("#channel-copy");
  let channelId = new URL(location.href).searchParams.get("channel");
  let createRequestId = "";
  let decisionRequestId = "";
  let decisionKind = "";
  let closeRequestId = "";
  let polling = false;

  function requestId() {
    const bytes = crypto.getRandomValues(new Uint8Array(16));
    let binary = "";
    for (const byte of bytes) binary += String.fromCharCode(byte);
    return btoa(binary).replaceAll("+", "-").replaceAll("/", "_").replace(/=+$/, "");
  }

  const sessionPromise = fetch("/api/v1/session", { cache: "no-store" })
    .then((response) => (response.ok ? response.json() : { enabled: false }))
    .then((session) => {
      if (!session.enabled) {
        publicToggle.closest("label").hidden = true;
        publicOnly.hidden = false;
      }
      if (session.enabled) publicToggle.checked = !!preferences.public;
      return session;
    });

  publicToggle.addEventListener("change", () => {
    savePreferences({ public: publicToggle.checked });
  });

  function showError(message) {
    errorLine.textContent = message;
    errorLine.hidden = false;
  }

  function renderSeat(seat) {
    if (!seat) {
      identity.hidden = true;
      decisions.hidden = true;
      statusLine.textContent = t("channelSenderWaiting");
      return;
    }
    identity.hidden = false;
    const joined = new Date(seat.joined_at).toLocaleTimeString();
    identity.textContent = seat.anonymous
      ? t("channelPendingAnonymous", { time: joined })
      : t("channelPendingUser", { name: "@" + seat.identity, time: joined });
    if (seat.state === "pending_admit") {
      statusLine.textContent = t("channelPendingTitle");
      decisions.hidden = false;
    } else {
      decisions.hidden = true;
      statusLine.textContent = t("channelConnected");
    }
  }

  async function refresh() {
    if (!channelId || polling) return;
    polling = true;
    try {
      const response = await fetch("/api/v1/channels/" + encodeURIComponent(channelId), {
        cache: "no-store",
        credentials: "same-origin",
      });
      if (!response.ok) throw new Error("HTTP " + response.status);
      const state = await response.json();
      renderSeat(state.seat);
      errorLine.hidden = true;
    } catch {
      showError(t("channelError"));
    } finally {
      polling = false;
    }
  }

  function scheduleRefresh() {
    window.setTimeout(async () => {
      await refresh();
      if (channelId) scheduleRefresh();
    }, 3000);
  }

  async function decide(decision) {
    if (!channelId) return;
    if (decisionKind !== decision) {
      decisionKind = decision;
      decisionRequestId = requestId();
    }
    admitButton.disabled = true;
    rejectButton.disabled = true;
    try {
      const response = await fetch("/api/v1/channels/" + encodeURIComponent(channelId) + "/admit", {
        method: "POST",
        credentials: "same-origin",
        headers: { "Content-Type": "application/json", "Idempotency-Key": decisionRequestId },
        body: JSON.stringify({ decision }),
      });
      if (!response.ok) throw new Error("HTTP " + response.status);
      decisionRequestId = "";
      decisionKind = "";
      await refresh();
      if (decision === "reject") statusLine.textContent = t("channelRejected");
    } catch {
      showError(t("channelError"));
    } finally {
      admitButton.disabled = false;
      rejectButton.disabled = false;
    }
  }

  openButton.addEventListener("click", async () => {
    createRequestId ||= requestId();
    openButton.disabled = true;
    errorLine.hidden = true;
    try {
      const session = await sessionPromise;
      const headers = { "Idempotency-Key": createRequestId };
      if (session.csrf_token) headers["X-CSRF-Token"] = session.csrf_token;
      if (!session.enabled || publicToggle.checked) headers["X-Nclip-Visibility"] = "public";
      if (!session.enabled) {
        const apiKey = document.querySelector("#api-key")?.value.trim();
        if (apiKey) headers["X-Api-Key"] = apiKey;
      }
      const response = await fetch("/api/v1/channels", {
        method: "POST",
        credentials: "same-origin",
        headers,
      });
      if (!response.ok) throw new Error("HTTP " + response.status);
      const channel = await response.json();
      createRequestId = "";
      channelId = channel.id;
      link.href = channel.url;
      link.textContent = channel.url;
      panel.hidden = false;
      openButton.hidden = true;
      mode.open = true;
      const next = new URL(location.href);
      next.searchParams.set("channel", channelId);
      history.replaceState({}, "", next);
      statusLine.textContent = t("channelReady");
      scheduleRefresh();
    } catch {
      showError(t("channelError"));
      openButton.disabled = false;
    }
  });

  admitButton.addEventListener("click", () => decide("admit"));
  rejectButton.addEventListener("click", () => decide("reject"));
  copyButton.addEventListener("click", async () => {
    try {
      await navigator.clipboard.writeText(link.href);
      copyButton.textContent = t("channelCopyDone");
    } catch {
      showError(t("channelError"));
    }
  });
  closeButton.addEventListener("click", async () => {
    if (!channelId) return;
    closeRequestId ||= requestId();
    closeButton.disabled = true;
    try {
      const response = await fetch("/api/v1/channels/" + encodeURIComponent(channelId), {
        method: "DELETE",
        credentials: "same-origin",
        headers: { "Idempotency-Key": closeRequestId },
      });
      if (!response.ok && response.status !== 404) throw new Error("HTTP " + response.status);
      channelId = "";
      closeRequestId = "";
      const next = new URL(location.href);
      next.searchParams.delete("channel");
      history.replaceState({}, "", next);
      panel.hidden = true;
      openButton.hidden = false;
      openButton.disabled = false;
      identity.hidden = true;
      decisions.hidden = true;
    } catch {
      showError(t("channelError"));
      closeButton.disabled = false;
    }
  });

  if (channelId) {
    fetch("/api/v1/channels/" + encodeURIComponent(channelId), { cache: "no-store", credentials: "same-origin" })
      .then(async (response) => {
        if (!response.ok) throw new Error("HTTP " + response.status);
        const state = await response.json();
        link.href = location.origin + "/c/" + channelId;
        link.textContent = link.href;
        panel.hidden = false;
        openButton.hidden = true;
        mode.open = true;
        renderSeat(state.seat);
        scheduleRefresh();
      })
      .catch(() => showError(t("channelError")));
  }
}
