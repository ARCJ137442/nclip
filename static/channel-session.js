let state = {
  channelId: "",
  sendTarget: "share",
  visibility: "authenticated",
  channelState: "closed",
  senderOnline: false,
  contentState: "empty",
  seat: null,
  observedAt: 0,
  transportError: "",
};
let armed = false;
const listeners = new Set();

function snapshot() {
  return { ...state, seat: state.seat ? { ...state.seat } : null, armed };
}

function publish() {
  const value = snapshot();
  for (const listener of listeners) listener(value);
}

export function getChannelSession() {
  return snapshot();
}

export function subscribeChannelSession(listener) {
  listeners.add(listener);
  listener(snapshot());
  return () => listeners.delete(listener);
}

export function updateChannelSession(next) {
  state = { ...state, ...next };
  if (!state.channelId) {
    state = {
      channelId: "",
      sendTarget: "share",
      visibility: "authenticated",
      channelState: "closed",
      senderOnline: false,
      contentState: "empty",
      seat: null,
      observedAt: 0,
      transportError: "",
    };
    armed = false;
  }
  publish();
}

export function setChannelArmed(value) {
  armed = Boolean(value);
  publish();
}

export function setChannelSendTarget(value) {
  state = { ...state, sendTarget: value === "channel" && state.channelId ? "channel" : "share" };
  publish();
}
