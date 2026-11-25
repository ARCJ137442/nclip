export function captureSeatTarget(seat) {
  if (!seat || seat.state !== "paired" || !seat.seat_id) return null;
  return { seat_id: seat.seat_id, identity: seat.identity || "", joined_at: seat.joined_at || "" };
}

export function isSameSeatTarget(seat, target) {
  return !!seat && !!target && seat.seat_id === target.seat_id &&
    (seat.identity || "") === target.identity && (seat.joined_at || "") === target.joined_at;
}
