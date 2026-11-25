export function receiverTerminalMessage(status, body = {}) {
  if (status === 410 && body.error === "join_closed" && body.reason === "sender_rejected") return "channelRejected";
  if (status === 410 && body.error === "pending_expired") return "channelPendingExpired";
  return "channelReceiveGone";
}

export function isReceiverStatusTerminal(status, body = {}) {
  return status === 410 || (status === 404 && body.error === "channel_not_found");
}
