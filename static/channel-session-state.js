export async function resolveChannelSession(fetcher = fetch) {
  try {
    const response = await fetcher("/api/v1/session", { cache: "no-store", credentials: "same-origin" });
    if (!response.ok) return { status: "error", enabled: false, csrf_token: "", error: response.status === 401 ? "loginRequired" : "channelSessionUnavailable" };
    const session = await response.json();
    return { ...session, status: "ready" };
  } catch {
    return { status: "error", enabled: false, csrf_token: "", error: "channelSessionUnavailable" };
  }
}
