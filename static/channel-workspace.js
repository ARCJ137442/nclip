import { getChannelSession, setChannelArmed, setChannelSendTarget, subscribeChannelSession, updateChannelSession } from "./channel-session.js";
import { t } from "./i18n.js";
import { preferences, savePreferences } from "./preferences.js";
import { authHeaders } from "./ui.js";

function requestID() {
  const bytes = crypto.getRandomValues(new Uint8Array(16));
  let binary = "";
  for (const byte of bytes) binary += String.fromCharCode(byte);
  return btoa(binary).replaceAll("+", "-").replaceAll("/", "_").replace(/=+$/, "");
}

function elapsedLabel(joinedAt) {
  const seconds = Math.max(0, Math.floor((Date.now() - new Date(joinedAt).getTime()) / 1000));
  if (seconds < 60) return t("timeSeconds", { count: Math.max(1, seconds) });
  return t("timeMinutes", { count: Math.ceil(seconds / 60) });
}

const mode = document.querySelector("#channel-mode");
if (mode) {
  const openButton = document.querySelector("#channel-open");
  const publicToggle = document.querySelector("#channel-public");
  const publicOnly = document.querySelector("#channel-public-only");
  const panel = document.querySelector("#channel-sender");
  const statusLine = document.querySelector("#channel-sender-state");
  const link = document.querySelector("#channel-link");
  const identity = document.querySelector("#channel-identity");
  const errorLine = document.querySelector("#channel-error");
  const reviewButton = document.querySelector("#channel-review");
  const kickButton = document.querySelector("#channel-kick");
  const refreshButton = document.querySelector("#channel-refresh");
  const useForSendButton = document.querySelector("#channel-use-send");
  const useShareButton = document.querySelector("#channel-use-share");
  const closeButton = document.querySelector("#channel-close");
  const copyButton = document.querySelector("#channel-copy");
  const approvalDialog = document.querySelector("#channel-approval-dialog");
  const approvalIdentity = document.querySelector("#channel-approval-identity");
  const approvalEffect = document.querySelector("#channel-approval-effect");
  const approvalSnapshot = document.querySelector("#channel-approval-snapshot");
  const approvalSnapshotFacts = document.querySelector("#channel-approval-snapshot-facts");
  const approvalSnapshotPreview = document.querySelector("#channel-approval-snapshot-preview");
  const approvalLaterButton = document.querySelector("#channel-approval-later");
  const admitButton = document.querySelector("#channel-admit");
  const rejectButton = document.querySelector("#channel-reject");
  const actionDialog = document.querySelector("#channel-action-dialog");
  const actionTitle = document.querySelector("#channel-action-title");
  const actionDescription = document.querySelector("#channel-action-description");
  const actionCancelButton = document.querySelector("#channel-action-cancel");
  const actionConfirmButton = document.querySelector("#channel-action-confirm");

  let channelId = new URL(location.href).searchParams.get("channel") || "";
  let createRequestId = "";
  let decisionRequestId = "";
  let decisionKind = "";
  let decisionSeatID = "";
  let closeRequestId = "";
  let kickRequestId = "";
  let action = "";
  let currentRecord = null;
  let currentSeat = null;
  let polling = false;
  let refreshDelay = 5000;
  let refreshTimer = 0;
  let observedAt = 0;
  let deferredApprovalSeatID = "";
  let approvalDialogSeatID = "";
  let decisionInFlightSeatID = "";
  let sessionState = { enabled: false, csrf_token: "" };

  const sessionPromise = fetch("/api/v1/session", { cache: "no-store", credentials: "same-origin" })
    .then((response) => (response.ok ? response.json() : { enabled: false }))
    .then((session) => {
      sessionState = session;
      if (!session.enabled) {
        publicToggle.closest("label").hidden = true;
        publicOnly.hidden = false;
      } else {
        publicToggle.checked = !!preferences.public;
      }
      return session;
    })
    .catch(() => {
      publicToggle.closest("label").hidden = true;
      publicOnly.hidden = false;
      return sessionState;
    });

  function publish(transportError = "") {
    if (!channelId) {
      updateChannelSession({ channelId: "" });
      return;
    }
    updateChannelSession({
      channelId,
      visibility: currentRecord?.visibility || (sessionState.enabled && !publicToggle.checked ? "authenticated" : "public"),
      channelState: currentRecord?.state || "open",
      senderOnline: currentRecord?.sender_online !== false,
      contentState: currentRecord?.content_state || "empty",
      seat: currentSeat,
      observedAt,
      transportError,
    });
  }

  function showError(key) {
    const messages = {
      sender_offline: "channelOffline",
      seats_full: "channelSeatFull",
      pending_expired: "channelSenderPendingExpired",
      channel_not_found: "channelGone",
      login_required: "loginRequired",
      permission_denied: "permissionDenied",
      csrf_failed: "channelError",
      rate_limited: "channelRateLimited",
      channel_capacity_reached: "channelUnavailable",
    };
    const known = new Set([
      "channelError", "channelOffline", "channelSenderReconnecting", "channelSenderPendingExpired",
      "channelPendingExpired", "channelClosed", "channelGone", "channelCopyFailed", "channelRateLimited",
      "channelUnavailable", "loginRequired", "permissionDenied",
    ]);
    const message = messages[key] || (known.has(key) ? key : "channelError");
    errorLine.textContent = t(message);
    errorLine.hidden = false;
  }

  function sessionCSRFHeaders() {
    return sessionState.csrf_token ? { "X-CSRF-Token": sessionState.csrf_token } : {};
  }

  function forgetClosedChannel(messageKey) {
    channelId = "";
    currentRecord = null;
    currentSeat = null;
    if (refreshTimer) clearTimeout(refreshTimer);
    if (approvalDialog.open) approvalDialog.close("seat-ended");
    updateChannelSession({ channelId: "" });
    setChannelArmed(false);
    setChannelSendTarget("share");
    const next = new URL(location.href);
    next.searchParams.delete("channel");
    history.replaceState({}, "", next);
    panel.hidden = true;
    openButton.hidden = false;
    openButton.disabled = false;
    publicToggle.disabled = false;
    renderActions();
    errorLine.hidden = true;
    showError(messageKey);
  }

  function recipientLabel(seat) {
    const joined = new Date(seat.joined_at).toLocaleTimeString(document.documentElement.lang);
    return seat.anonymous
      ? t("channelPendingAnonymous", { time: joined })
      : t("channelPendingUser", { name: "@" + seat.identity, time: joined });
  }

  function renderActions() {
    const session = getChannelSession();
    useForSendButton.hidden = !channelId || session.sendTarget === "channel";
    useShareButton.hidden = !channelId || session.sendTarget !== "channel";
    copyButton.hidden = !channelId;
    closeButton.hidden = !channelId;
    refreshButton.hidden = !channelId || !session.transportError;
    reviewButton.hidden = !currentSeat || currentSeat.state !== "pending_admit";
    kickButton.hidden = !currentSeat || currentSeat.state !== "paired";
  }

  function renderRecord(record) {
    currentRecord = record;
    currentSeat = record.seat || null;
    observedAt = Date.now();
    const online = record.sender_online !== false;
    if (!online) {
      statusLine.textContent = t("channelSenderOffline");
    } else if (record.state === "closed") {
      statusLine.textContent = t("channelClosed");
    } else if (currentSeat?.state === "pending_admit") {
      statusLine.textContent = t("channelPendingTitle");
      openApproval(currentSeat);
    } else if (currentSeat?.state === "paired" && record.content_state === "held") {
      statusLine.textContent = t("channelOccupiedFor", { time: elapsedLabel(currentSeat.joined_at) });
    } else if (currentSeat?.state === "paired" && record.content_state === "uploading") {
      statusLine.textContent = t("channelContentUploading");
    } else if (currentSeat?.state === "paired") {
      statusLine.textContent = t("channelConnected");
    } else {
      statusLine.textContent = t("channelSenderWaiting");
    }

    if (currentSeat) {
      identity.textContent = recipientLabel(currentSeat);
      identity.hidden = false;
    } else {
      identity.textContent = "";
      identity.hidden = true;
      if (approvalDialog.open) approvalDialog.close("seat-ended");
      approvalDialogSeatID = "";
      deferredApprovalSeatID = "";
    }
    renderActions();
    publish();
  }

  function renderApproval(seat = currentSeat) {
    if (!seat || seat.state !== "pending_admit") return;
    const session = getChannelSession();
    approvalIdentity.textContent = recipientLabel(seat);
    approvalSnapshot.hidden = !session.armed;
    approvalEffect.textContent = t(session.armed ? "channelApprovalArmedEffect" : "channelApprovalEffect");
    admitButton.textContent = t(session.armed ? "channelAdmitAndSend" : "channelAdmit");
  }

  function openApproval(seat = currentSeat, force = false) {
    if (!seat || seat.state !== "pending_admit") return;
    if (approvalDialog.open && approvalDialogSeatID !== seat.seat_id) approvalDialog.close("replaced");
    renderApproval(seat);
    if (!approvalDialog.open && (force || seat.seat_id !== deferredApprovalSeatID)) {
      approvalDialog.showModal();
      approvalDialogSeatID = seat.seat_id;
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
      if (!response.ok) {
        const body = await response.json().catch(() => ({}));
        if ([404, 410].includes(response.status)) {
          forgetClosedChannel(body.error === "channel_not_found" ? "channelGone" : "channelClosed");
          return;
        }
        throw new Error(body.error || "channel_unavailable");
      }
      const state = await response.json();
      refreshDelay = 5000;
      renderRecord(state);
      errorLine.hidden = true;
    } catch {
      const lastConfirmed = observedAt
        ? ` · ${t("channelConnectionLostAt", { time: new Date(observedAt).toLocaleTimeString(document.documentElement.lang) })}`
        : "";
      statusLine.textContent = `${t("channelSenderReconnecting")}${lastConfirmed}`;
      showError("channelSenderReconnecting");
      refreshDelay = Math.min(document.hidden ? 25000 : 15000, Math.max(5000, refreshDelay * 2));
      publish("network");
      renderActions();
    } finally {
      polling = false;
      scheduleRefresh();
    }
  }

  function scheduleRefresh() {
    if (refreshTimer) clearTimeout(refreshTimer);
    if (!channelId) return;
    const minimum = document.hidden ? 25000 : 5000;
    refreshTimer = window.setTimeout(() => { void refresh(); }, Math.max(minimum, refreshDelay));
  }

  async function decide(decision) {
    const targetSeatID = currentSeat?.seat_id || "";
    if (!channelId || !targetSeatID || decisionInFlightSeatID) return false;
    if (decisionSeatID !== targetSeatID || decisionKind !== decision) {
      decisionSeatID = targetSeatID;
      decisionKind = decision;
      decisionRequestId = requestID();
    }
    decisionInFlightSeatID = targetSeatID;
    admitButton.disabled = true;
    rejectButton.disabled = true;
    try {
      const response = await fetch("/api/v1/channels/" + encodeURIComponent(channelId) + "/admit", {
        method: "POST",
        credentials: "same-origin",
        headers: {
          "Content-Type": "application/json",
          "Idempotency-Key": decisionRequestId,
          ...sessionCSRFHeaders(),
        },
        body: JSON.stringify({ seat_id: targetSeatID, decision }),
      });
      if (!response.ok) {
        const result = await response.json().catch(() => ({}));
        await refresh();
        if (currentSeat?.seat_id === targetSeatID) {
          showError(result.error === "pending_expired" ? "channelSenderPendingExpired" : "channelError");
        }
        return false;
      }
      decisionRequestId = "";
      decisionSeatID = "";
      decisionKind = "";
      if (approvalDialog.open) approvalDialog.close(decision);
      await refresh();
      if (decision === "reject") statusLine.textContent = t("channelRejected");
      return true;
    } catch {
      showError("channelError");
      return false;
    } finally {
      decisionInFlightSeatID = "";
      admitButton.disabled = false;
      rejectButton.disabled = false;
    }
  }

  function openAction(actionName) {
    action = actionName;
    const kick = actionName === "kick";
    actionTitle.textContent = t(kick ? "channelActionKickTitle" : "channelActionCloseTitle");
    actionDescription.textContent = t(kick ? "channelActionKickDescription" : "channelActionCloseDescription");
    actionConfirmButton.textContent = t(kick ? "channelActionKickConfirm" : "channelActionCloseConfirm");
    if (!actionDialog.open) actionDialog.showModal();
  }

  async function confirmAction() {
    if (!channelId || !action) return;
    const actionName = action;
    const seatID = currentSeat?.seat_id || "";
    const request = actionName === "kick" ? (kickRequestId ||= requestID()) : (closeRequestId ||= requestID());
    actionConfirmButton.disabled = true;
    try {
      const response = actionName === "kick"
        ? await fetch("/api/v1/channels/" + encodeURIComponent(channelId) + "/kick", {
          method: "POST",
          credentials: "same-origin",
          headers: { "Content-Type": "application/json", "Idempotency-Key": request, ...sessionCSRFHeaders() },
          body: JSON.stringify({ seat_id: seatID }),
        })
        : await fetch("/api/v1/channels/" + encodeURIComponent(channelId), {
          method: "DELETE",
          credentials: "same-origin",
          headers: { "Idempotency-Key": request, ...sessionCSRFHeaders() },
        });
      if (!response.ok && !(actionName === "close" && response.status === 404)) {
        const body = await response.json().catch(() => ({}));
        throw new Error(body.error || "channel_error");
      }
      actionDialog.close("confirmed");
      action = "";
      if (actionName === "kick") {
        kickRequestId = "";
        await refresh();
      } else {
        channelId = "";
        closeRequestId = "";
        currentRecord = null;
        currentSeat = null;
        if (refreshTimer) clearTimeout(refreshTimer);
        setChannelArmed(false);
        setChannelSendTarget("share");
        const next = new URL(location.href);
        next.searchParams.delete("channel");
        history.replaceState({}, "", next);
        updateChannelSession({ channelId: "" });
        panel.hidden = true;
        openButton.hidden = false;
        openButton.disabled = false;
        publicToggle.disabled = false;
        mode.open = true;
      }
    } catch {
      showError("channelError");
      actionConfirmButton.disabled = false;
    }
  }

  subscribeChannelSession((session) => {
    if (approvalDialog.open && currentSeat?.state === "pending_admit") renderApproval(currentSeat);
    useForSendButton.hidden = !channelId || session.sendTarget === "channel";
    useShareButton.hidden = !channelId || session.sendTarget !== "channel";
  });

  publicToggle.addEventListener("change", () => savePreferences({ public: publicToggle.checked }));
  reviewButton.addEventListener("click", () => openApproval(currentSeat, true));
  admitButton.addEventListener("click", () => { void decide("admit"); });
  rejectButton.addEventListener("click", () => { void decide("reject"); });
  approvalLaterButton.addEventListener("click", () => {
    deferredApprovalSeatID = currentSeat?.seat_id || "";
    approvalDialog.close("later");
  });
  approvalDialog.addEventListener("cancel", () => {
    deferredApprovalSeatID = currentSeat?.seat_id || "";
  });
  approvalDialog.addEventListener("close", () => {
    approvalDialogSeatID = "";
    if (approvalDialog.returnValue === "later") deferredApprovalSeatID = currentSeat?.seat_id || "";
  });
  kickButton.addEventListener("click", () => openAction("kick"));
  refreshButton.addEventListener("click", () => { refreshDelay = 5000; void refresh(); });
  closeButton.addEventListener("click", () => openAction("close"));
  actionCancelButton.addEventListener("click", () => actionDialog.close("cancelled"));
  actionDialog.addEventListener("close", () => { action = ""; actionConfirmButton.disabled = false; });
  actionConfirmButton.addEventListener("click", () => { void confirmAction(); });
  useForSendButton.addEventListener("click", () => setChannelSendTarget("channel"));
  useShareButton.addEventListener("click", () => setChannelSendTarget("share"));
  copyButton.addEventListener("click", async () => {
    try {
      await navigator.clipboard.writeText(link.href);
      copyButton.textContent = t("channelCopyDone");
    } catch {
        const selection = window.getSelection();
        const range = document.createRange();
        range.selectNodeContents(link);
        selection?.removeAllRanges();
        selection?.addRange(range);
      showError("channelCopyFailed");
    }
  });
  document.addEventListener("visibilitychange", () => {
    if (!channelId) return;
    if (!document.hidden) {
      if (refreshTimer) clearTimeout(refreshTimer);
      void refresh();
    } else {
      scheduleRefresh();
    }
  });
  window.addEventListener("channelrefresh", () => { void refresh(); });

  openButton.addEventListener("click", async () => {
    createRequestId ||= requestID();
    openButton.disabled = true;
    errorLine.hidden = true;
    try {
      const session = await sessionPromise;
      const headers = { "Idempotency-Key": createRequestId, ...sessionCSRFHeaders() };
      if (!session.enabled || publicToggle.checked) headers["X-Nclip-Visibility"] = "public";
      Object.assign(headers, authHeaders());
      const response = await fetch("/api/v1/channels", {
        method: "POST",
        credentials: "same-origin",
        headers,
      });
      if (!response.ok) {
        const body = await response.json().catch(() => ({}));
        throw new Error(body.error || `HTTP ${response.status}`);
      }
      const channel = await response.json();
      createRequestId = "";
      channelId = channel.id;
      currentRecord = { state: "open", visibility: channel.visibility, sender_online: true, content_state: "empty" };
      currentSeat = null;
      observedAt = Date.now();
      link.href = channel.url;
      link.textContent = channel.url;
      panel.hidden = false;
      openButton.hidden = true;
      publicToggle.disabled = true;
      mode.open = true;
      const next = new URL(location.href);
      next.searchParams.set("channel", channelId);
      history.replaceState({}, "", next);
      setChannelArmed(false);
      renderRecord(currentRecord);
      setChannelSendTarget("channel");
      scheduleRefresh();
    } catch (error) {
      showError(error.message || "channelError");
      openButton.disabled = false;
    }
  });

  if (channelId) {
    mode.open = true;
    panel.hidden = false;
    openButton.hidden = true;
    publicToggle.disabled = true;
    link.href = location.origin + "/c/" + encodeURIComponent(channelId);
    link.textContent = link.href;
    publish();
    setChannelSendTarget("channel");
    void refresh();
  }
}
