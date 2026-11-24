import { csrfHeaders } from "./access.js";
import { copyItem } from "./clipboard.js";
import { decodeShare, requestBytes } from "./share-api.js";
import { createProgress, timeParts } from "./transfer.js";
import { renderItems, status } from "./ui.js";
import { sizeLabel, t } from "./i18n.js";

const page = document.querySelector("#channel-page");
if (page) {
  const channelId = page.dataset.channelId;
  const channelURL = `/api/v1/channels/${encodeURIComponent(channelId)}`;
  const contentURL = `${channelURL}/content`;
  const statusLine = document.querySelector("#receiver-status");
  const errorLine = document.querySelector("#receiver-error");
  const joinButton = document.querySelector("#channel-join");
  const leaveButton = document.querySelector("#channel-leave");
  const stages = document.querySelector("#channel-receive-stages");
  const progressPanel = document.querySelector("#channel-receive-progress");
  const retryButton = document.querySelector("#channel-receive-retry");
  const resultsPanel = document.querySelector("#channel-received");
  const results = document.querySelector("#channel-received-items");
  const archiveLink = document.querySelector("#channel-download-archive");

  let seatToken = "";
  let claimToken = "";
  let joinRequestId = "";
  let leaveRequestId = "";
  let polling = false;
  let receiving = false;
  let leaving = false;
  let completed = false;
  let transferETag = "";
  let retained = [];
  let receiveAttempt = 0;
  let receiveController = null;
  let archiveURL = "";
  let receiveAttemptStartedAt = 0;

  function requestId() {
    const bytes = crypto.getRandomValues(new Uint8Array(16));
    let binary = "";
    for (const byte of bytes) binary += String.fromCharCode(byte);
    return btoa(binary).replaceAll("+", "-").replaceAll("/", "_").replace(/=+$/, "");
  }

  async function csrf() {
    const response = await fetch("/api/v1/session", { cache: "no-store" });
    if (!response.ok) return {};
    const session = await response.json();
    return session.csrf_token ? { "X-CSRF-Token": session.csrf_token } : {};
  }

  function setState(key, values = {}) {
    statusLine.textContent = t(key, values);
  }

  function setError(key, values = {}) {
    errorLine.textContent = t(key, values);
    errorLine.hidden = false;
  }

  function setStage(phase) {
    const order = ["connecting", "downloading", "verifying", "ready"];
    const active = order.indexOf(phase);
    stages.hidden = false;
    for (const [index, name] of order.entries()) {
      const item = stages.querySelector(`[data-stage="${name}"]`);
      const state = phase === "ready" ? "done" : index < active ? "done" : index === active ? "active" : "pending";
      item.dataset.state = state;
      if (state === "active") item.setAttribute("aria-current", "step");
      else item.removeAttribute("aria-current");
    }
  }

  function retainedBytes() {
    return retained.reduce((sum, chunk) => sum + chunk.byteLength, 0);
  }

  function paintProgress({ received, total, percent, rate, eta }) {
    const track = document.querySelector("#channel-receive-track");
    document.querySelector("#channel-receive-percent").textContent = percent == null ? "" : `${Math.floor(percent)}%`;
    document.querySelector("#channel-receive-fill").style.width = percent == null ? "" : `${percent}%`;
    track.dataset.unknown = String(percent == null);
    if (percent == null) track.removeAttribute("aria-valuenow");
    else track.setAttribute("aria-valuenow", String(Math.floor(percent)));
    document.querySelector("#channel-receive-bytes").textContent = total > 0
      ? `${sizeLabel(received)} / ${sizeLabel(total)}`
      : sizeLabel(received);
    const etaPart = timeParts(eta);
    document.querySelector("#channel-receive-rate").textContent = rate > 0
      ? `${sizeLabel(rate)}/s${etaPart ? ` · ${t(etaPart.unit === "minutes" ? "timeMinutes" : "timeSeconds", { count: etaPart.count })}` : ""}`
      : "";
    const elapsedPart = timeParts((performance.now() - receiveAttemptStartedAt) / 1000);
    document.querySelector("#channel-receive-freshness").textContent = elapsedPart
      ? t("transferElapsed", { time: t(elapsedPart.unit === "minutes" ? "timeMinutes" : "timeSeconds", { count: elapsedPart.count }) })
      : "";
  }

  function hideLease() {
    seatToken = "";
    claimToken = "";
    joinRequestId = "";
    leaveRequestId = "";
    leaveButton.hidden = true;
    leaveButton.disabled = false;
    joinButton.hidden = false;
    joinButton.disabled = false;
    joinButton.textContent = t("channelJoin");
  }

  function pageLeave() {
    if (!seatToken || completed) return;
    leaveRequestId ||= requestId();
    receiveController?.abort();
    void fetch(`${channelURL}/leave`, {
      method: "POST",
      credentials: "same-origin",
      cache: "no-store",
      keepalive: true,
      headers: {
        "Idempotency-Key": leaveRequestId,
        "X-Nclip-Seat-Token": seatToken,
      },
    });
  }

  async function leave() {
    if (!seatToken || leaving) return;
    leaving = true;
    leaveRequestId ||= requestId();
    receiveController?.abort();
    leaveButton.disabled = true;
    try {
      const response = await fetch(`${channelURL}/leave`, {
        method: "POST",
        credentials: "same-origin",
        cache: "no-store",
        headers: {
          "Idempotency-Key": leaveRequestId,
          "X-Nclip-Seat-Token": seatToken,
        },
      });
      const body = response.ok ? {} : await response.json().catch(() => ({}));
      if (!response.ok && !["join_closed", "delivery_complete", "receiver_kicked"].includes(body.error))
        throw new Error(body.error || `HTTP ${response.status}`);
      hideLease();
      retained = [];
      transferETag = "";
      retryButton.hidden = true;
      progressPanel.hidden = true;
      if (!completed) setState("channelLeft");
    } catch {
      leaveButton.disabled = false;
      setError("channelError");
    } finally {
      leaving = false;
    }
  }

  async function receiveContent() {
    if (!seatToken || !claimToken || receiving || completed || leaving) return;
    receiving = true;
    errorLine.hidden = true;
    progressPanel.hidden = false;
    retryButton.hidden = true;
    receiveAttempt++;
    receiveAttemptStartedAt = performance.now();
    const initialBytes = retainedBytes();
    const chunks = [];
    const progress = createProgress();
    progress.begin();
    receiveController = new AbortController();
    setStage("connecting");
    document.querySelector("#channel-receive-phase").textContent = t("channelDownloading");
    document.querySelector("#channel-receive-attempt").textContent = t("receiveAttempt", { count: receiveAttempt });
    document.querySelector("#channel-receive-note").textContent = t("channelCancelNote");
    document.querySelector("#channel-receive-note").hidden = false;
    paintProgress({ received: initialBytes, total: 0, percent: null, rate: 0, eta: null });
    setState(initialBytes ? "channelResuming" : "channelDownloading", { size: sizeLabel(initialBytes) });
    let responseComplete = false;
    try {
      const headers = {
        "X-Nclip-Seat-Token": seatToken,
        "X-Nclip-Claim-Token": claimToken,
      };
      if (initialBytes && transferETag) headers["If-Range"] = transferETag;
      const transfer = await requestBytes(contentURL, { headers }, {
        start: initialBytes,
        signal: receiveController.signal,
        onMeta: ({ etag }) => { if (etag) transferETag = etag; },
        onReset: () => { retained = []; chunks.length = 0; transferETag = ""; },
        onChunk: (chunk) => { if (chunk?.byteLength) chunks.push(chunk); },
        onProgress: (value) => {
          setStage("downloading");
          paintProgress(progress.update(value.received, value.total));
        },
      });
      responseComplete = true;
      setStage("verifying");
      document.querySelector("#channel-receive-phase").textContent = t("channelVerifyResponse");
      await new Promise((resolve) => requestAnimationFrame(resolve));
      const body = new Blob([...retained, ...chunks], { type: transfer.contentType });
      const decoded = await decodeShare(new Response(body, { headers: { "Content-Type": transfer.contentType } }));
      retained = [];
      chunks.length = 0;
      await renderItems(results, decoded.items, {
        receive: true,
        copy: async (item, plainOnly) => {
          try {
            const result = await copyItem(item, { plainOnly });
            status(result.plain ? "plainFallback" : "copied", "success");
          } catch {
            status("copyFailed", "error");
          }
        },
      });
      archiveLink.replaceChildren();
      archiveLink.hidden = !(decoded.archive instanceof Blob);
      if (decoded.archive instanceof Blob) {
        if (archiveURL) URL.revokeObjectURL(archiveURL);
        archiveURL = URL.createObjectURL(decoded.archive);
        const link = document.createElement("a");
        link.className = "button";
        link.href = archiveURL;
        link.download = `${channelId}.zip`;
        link.textContent = t("channelDownloadArchive");
        archiveLink.append(link);
      }
      resultsPanel.hidden = false;
      progressPanel.hidden = true;
      retryButton.hidden = true;
      setStage("ready");
      completed = true;
      hideLease();
      errorLine.hidden = true;
      setState("channelReceiveReady");
    } catch (error) {
      if (leaving) return;
      if (!seatToken && !completed) {
        retained = [];
        chunks.length = 0;
        setError("channelReceiveGone");
        return;
      }
      if (["receiver_kicked", "delivery_complete", "join_closed", "content_upload_closed"].includes(error?.message) || [403, 404, 410].includes(error?.status)) {
        retained = [];
        chunks.length = 0;
        hideLease();
        setError("channelReceiveGone");
      } else if (error?.status === 416 || error?.message === "range_mismatch" || error?.message === "content_offset_conflict" || error?.message === "range_gap") {
        retained = [];
        chunks.length = 0;
        transferETag = "";
        retryButton.hidden = false;
        retryButton.dataset.action = "restart";
        retryButton.textContent = t("channelRestartReceive");
        setError("channelRangeRestart");
      } else if (responseComplete) {
        retained = [];
        chunks.length = 0;
        transferETag = "";
        retryButton.hidden = false;
        retryButton.textContent = t("channelRestartReceive");
        setError("channelReceiveVerifyFailed");
      } else {
        retained.push(...chunks);
        chunks.length = 0;
        retryButton.hidden = false;
        retryButton.dataset.action = "resume";
        retryButton.textContent = t("channelRetryReceive");
        setError("channelResuming", { size: sizeLabel(retainedBytes()) });
      }
    } finally {
      receiveController = null;
      receiving = false;
    }
  }

  async function refresh() {
    if (!seatToken || polling || completed) return;
    polling = true;
    try {
      const response = await fetch(channelURL, {
        cache: "no-store",
        credentials: "same-origin",
        headers: { "X-Nclip-Seat-Token": seatToken },
      });
      if (completed) return;
      if (response.status === 410) {
        const body = await response.json().catch(() => ({}));
        if (receiving && body.error === "delivery_complete") return;
        hideLease();
        setError(body.error === "pending_expired" ? "channelPendingExpired" : "channelReceiveGone");
        return;
      }
      if (!response.ok) throw new Error(`HTTP ${response.status}`);
      const state = await response.json();
      errorLine.hidden = true;
      if (state.seat_state === "pending_admit") setState("channelWaitingApproval");
      else if (state.content_state === "uploading") {
        if (!receiving) setState("channelContentUploading");
      } else if (state.content_state === "held") {
        if (!receiving) void receiveContent();
      } else if (!receiving) setState("channelWaitingContent");
    } catch {
      if (!completed && !leaving) setError("channelJoinNetwork");
    } finally {
      polling = false;
      if (seatToken) window.setTimeout(refresh, 2000);
    }
  }

  joinButton.addEventListener("click", async () => {
    completed = false;
    resultsPanel.hidden = true;
    results.replaceChildren();
    archiveLink.replaceChildren();
    archiveLink.hidden = true;
    if (archiveURL) URL.revokeObjectURL(archiveURL);
    archiveURL = "";
    joinRequestId ||= requestId();
    joinButton.disabled = true;
    errorLine.hidden = true;
    setState("channelJoinWorking");
    try {
      const response = await fetch(`${channelURL}/join`, {
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
        } else throw new Error(`HTTP ${response.status}`);
        joinButton.disabled = false;
        return;
      }
      seatToken = body.seat_token || "";
      claimToken = body.claim_token || "";
      leaveButton.textContent = t("channelLeaveAndCancel");
      joinButton.hidden = true;
      leaveButton.hidden = false;
      setState(body.state === "pending_admit" ? "channelWaitingApproval" : "channelWaitingContent");
      await refresh();
    } catch {
      setError("channelJoinNetwork");
      joinButton.textContent = t("channelRetryJoin");
      joinButton.disabled = false;
    }
  });

  leaveButton.addEventListener("click", () => { void leave(); });
  retryButton.addEventListener("click", () => {
    if (retryButton.dataset.action === "restart") {
      retained = [];
      transferETag = "";
    }
    retryButton.dataset.action = "resume";
    retryButton.textContent = t("channelRetryReceive");
    void receiveContent();
  });
  window.addEventListener("pagehide", (event) => {
    if (event.persisted) return;
    pageLeave();
    if (archiveURL) URL.revokeObjectURL(archiveURL);
  });
  window.addEventListener("pageshow", () => { if (seatToken) void refresh(); });
}
