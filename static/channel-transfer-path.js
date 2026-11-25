export const CHANNEL_BROWSER_DOWNLOAD_THRESHOLD_BYTES = 32 * 1024 * 1024;

export function shouldUseBrowserDownload(contentBytes, supportsInPageReceive = true) {
  const size = Number(contentBytes);
  return !supportsInPageReceive || !Number.isFinite(size) || size >= CHANNEL_BROWSER_DOWNLOAD_THRESHOLD_BYTES;
}

export function validateChannelDownloadURL(value, channelURL, pageURL) {
  const page = new URL(pageURL);
  const channel = new URL(channelURL, page);
  const download = new URL(value, page);
  return download.origin === page.origin && download.origin === channel.origin &&
    download.pathname.startsWith(`${channel.pathname}/content/download/`) &&
    download.search === "" && download.hash === "";
}

// Use the browser's same-origin download path without requiring a WindowProxy.
// Some hosts dispatch window.open but return null; that is not a failed transfer.
// The receiver page stays loaded and remains the owner of seat liveness.
export function dispatchBrowserDownload(url, document) {
  const link = document.createElement("a");
  link.href = url;
  link.download = "";
  link.target = "_blank";
  link.rel = "noopener noreferrer";
  document.body.append(link);
  try { link.click(); } finally { link.remove(); }
}
