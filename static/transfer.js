// Transfer math is kept free of DOM and network so both directions report the
// same honest numbers and the arithmetic stays testable outside a browser.
export function percentOf(received, total) {
  if (!(total > 0)) return null;
  return Math.min(100, Math.max(0, (received / total) * 100));
}

export function timeParts(seconds) {
  if (!Number.isFinite(seconds) || seconds < 0) return null;
  if (seconds < 60)
    return { unit: "seconds", count: Math.max(1, Math.ceil(seconds)) };
  return { unit: "minutes", count: Math.max(1, Math.ceil(seconds / 60)) };
}

// Samples the wire rate at most every 250 ms and smooths it so the label does
// not jump on every progress event. `total <= 0` means the size is unknown.
export function createProgress(now = () => performance.now()) {
  let startedAt = null,
    lastAt = 0,
    lastBytes = 0,
    rate = 0;
  function begin() {
    startedAt = lastAt = now();
    lastBytes = 0;
    rate = 0;
  }
  function update(received, total) {
    const at = now();
    if (startedAt === null) begin();
    const elapsed = at - lastAt;
    if (elapsed >= 250) {
      const sample = ((received - lastBytes) * 1000) / elapsed;
      if (sample >= 0) rate = rate > 0 ? rate * 0.6 + sample * 0.4 : sample;
      lastAt = at;
      lastBytes = received;
    }
    const percent = percentOf(received, total);
    const eta =
      total > received && rate > 0
        ? (total - received) / rate
        : percent === 100
          ? 0
          : null;
    return { received, total, percent, rate, eta, elapsed: at - startedAt };
  }
  return { begin, update };
}
