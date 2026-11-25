export function receiverTerminalMessage(status, body = {}) {
  if (status === 410 && body.error === "join_closed" && body.reason === "sender_rejected") return "channelRejected";
  if (status === 410 && body.error === "pending_expired") return "channelPendingExpired";
  return "channelReceiveGone";
}
