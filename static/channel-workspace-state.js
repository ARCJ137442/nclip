export function canKickSeat(seat) {
  return !!seat?.seat_id && ["paired", "released"].includes(seat.state);
}

export function recipientHint(seat) {
  if (seat?.state === "released") return "channelReceiverRecovering";
  return seat?.state === "paired" ? "channelSendReady" : "channelSnapshotWaiting";
}

export function captureSeatTarget(seat) {
  if (!canKickSeat(seat)) return null;
  return { seat_id: seat.seat_id, identity: seat.identity || "", joined_at: seat.joined_at || "" };
}

export function isSameSeatTarget(seat, target) {
  return !!seat && !!target && seat.seat_id === target.seat_id &&
    (seat.identity || "") === target.identity && (seat.joined_at || "") === target.joined_at;
}
