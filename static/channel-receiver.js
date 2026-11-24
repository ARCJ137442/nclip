import { t } from "./i18n.js";

const page = document.querySelector("#channel-page");
if (page) {
  const channelId = page.dataset.channelId;
  const statusLine = document.querySelector("#receiver-status");
  const errorLine = document.querySelector("#receiver-error");
  const joinButton = document.querySelector("#channel-join");
  const leaveButton = document.querySelector("#channel-leave");
  let seatToken = "";
  let claimToken = "";
  let joinRequestId = "";
  let polling = false;

  function requestId() {
    const bytes = crypto.getRandomValues(new Uint8Array(16));
    let binary = "";
    for (const byte of bytes) binary += String.fromCharCode(byte);
    return btoa(binary).replaceAll("+", "-").replaceAll("/", "_").replace(/=+$/, "");
  }

  async function csrfHeaders() {
    const response = await fetch("/api/v1/session", { cache: "no-store" });
    if (!response.ok) return {};
    const session = await response.json();
    return session.csrf_token ? { "X-CSRF-Token": session.csrf_token } : {};
  }

  function setState(key) {
    statusLine.textContent = t(key);
  }

  function setError(key) {
    errorLine.textContent = t(key);
    errorLine.hidden = false;
  }

  async function refresh() {
    if (!seatToken || polling) return;
    polling = true;
    try {
      const response = await fetch("/api/v1/channels/" + encodeURIComponent(channelId), {
        cache: "no-store",
        credentials: "same-origin",
        headers: { "X-Nclip-Seat-Token": seatToken },
      });
      if (response.status === 410) {
        const body = await response.json();
        setError(body.error === "pending_expired" ? "channelPendingExpired" : "channelRejected");
        seatToken = "";
        claimToken = "";
        leaveButton.hidden = true;
        joinButton.hidden = false;
        joinButton.disabled = false;
        joinRequestId = "";
        return;
      }
      if (!response.ok) throw new Error("HTTP " + response.status);
      const state = await response.json();
      errorLine.hidden = true;
      setState(state.seat_state === "pending_admit" ? "channelWaitingApproval" : "channelConnected");
    } catch {
      setError("channelJoinNetwork");
    } finally {
      polling = false;
      if (seatToken) window.setTimeout(refresh, 2000);
    }
  }

  joinButton.addEventListener("click", async () => {
    joinRequestId ||= requestId();
    joinButton.disabled = true;
    errorLine.hidden = true;
    setState("channelJoinWorking");
    try {
      const response = await fetch("/api/v1/channels/" + encodeURIComponent(channelId) + "/join", {
        method: "POST",
        credentials: "same-origin",
        headers: { "Idempotency-Key": joinRequestId, ...(await csrfHeaders()) },
      });
      const body = await response.json().catch(() => ({}));
      if (!response.ok) {
        if (response.status === 410) {
          setError(body.error === "sender_offline" ? "channelOffline" : "channelPendingExpired");
          joinRequestId = "";
        } else if (response.status === 409 && body.error === "seats_full") {
          setError("channelSeatFull");
          joinRequestId = "";
        } else if (response.status === 429) {
          setError("channelRateLimited");
          joinRequestId = "";
        } else {
          throw new Error("HTTP " + response.status);
        }
        joinButton.disabled = false;
        return;
      }
      seatToken = body.seat_token;
      claimToken = body.claim_token;
      joinButton.hidden = true;
      leaveButton.hidden = false;
      setState(body.state === "pending_admit" ? "channelWaitingApproval" : "channelConnected");
      await refresh();
    } catch {
      setError("channelJoinNetwork");
      joinButton.textContent = t("channelRetryJoin");
      joinButton.disabled = false;
    }
  });

  leaveButton.addEventListener("click", async () => {
    if (!seatToken) return;
    leaveButton.disabled = true;
    try {
      const response = await fetch("/api/v1/channels/" + encodeURIComponent(channelId) + "/leave", {
        method: "POST",
        credentials: "same-origin",
        headers: {
          "Idempotency-Key": requestId(),
          "X-Nclip-Seat-Token": seatToken,
        },
      });
      if (!response.ok) throw new Error("HTTP " + response.status);
      seatToken = "";
      claimToken = "";
      joinRequestId = "";
      leaveButton.hidden = true;
      leaveButton.disabled = false;
      joinButton.hidden = false;
      joinButton.disabled = false;
      joinButton.textContent = t("channelJoin");
      setState("channelLeft");
    } catch {
      leaveButton.disabled = false;
      setError("channelError");
    }
  });
}
