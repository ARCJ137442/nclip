import { language } from "./preferences.js";
const messages = {
  tagline: ["把内容，传到另一端。", "Your clipboard, across devices."],
  settings: ["设置", "Settings"],
  language: ["语言", "Language"],
  theme: ["外观", "Appearance"],
  system: ["跟随系统", "System"],
  light: ["浅色", "Light"],
  dark: ["深色", "Dark"],
  compose: ["粘贴，然后分享", "Paste. Send. Share."],
  composeHint: [
    "文字、截图和文件，一起生成一个链接。",
    "Text, screenshots and files. One link.",
  ],
  placeholder: ["在这里输入，或直接粘贴内容…", "Type or paste something here…"],
  paste: ["粘贴", "Paste"],
  attach: ["添加文件", "Add files"],
  send: ["生成链接", "Create link"],
  burn: ["一次性接收", "One-time receive"],
  burnHint: [
    "接收方确认后，销毁服务端副本。",
    "Delete the server copy after the recipient confirms.",
  ],
  recommend: ["剪贴板推荐", "Clipboard suggestions"],
  recommendHint: [
    "启用后，在浏览器允许时预览剪贴板；发送前不会上传。",
    "Preview your clipboard when allowed. Nothing uploads until you send.",
  ],
  candidate: ["剪贴板里有新内容", "Ready from your clipboard"],
  sendCandidate: ["发送并复制链接", "Send & copy link"],
  permission: [
    "点击“粘贴”读取，或直接使用系统粘贴。",
    "Click Paste or use your system paste command.",
  ],
  empty: ["先粘贴内容或添加文件。", "Paste content or add a file first."],
  reading: ["正在读取剪贴板…", "Reading clipboard…"],
  uploading: ["正在上传…", "Uploading…"],
  sent: ["上传完成", "Upload complete"],
  sentCopied: ["链接已复制，可以分享了。", "Link copied. Ready to share."],
  sentManual: [
    "上传完成。点击“复制链接”即可分享。",
    "Uploaded. Click Copy link to share.",
  ],
  copyLink: ["复制链接", "Copy link"],
  copied: ["已复制", "Copied"],
  copy: ["复制到剪贴板", "Copy to clipboard"],
  copyPlain: ["复制纯文本", "Copy plain text"],
  plainFallback: ["已按纯文本复制。", "Copied as plain text."],
  copyFailed: [
    "未能写入剪贴板，请重试或手动选择复制。",
    "Could not access clipboard. Retry or select and copy manually.",
  ],
  view: ["打开接收页", "Open link"],
  newShare: ["再传一份", "Send another"],
  share: ["分享", "Share"],
  remove: ["移除", "Remove"],
  file: ["文件", "File"],
  rich: ["富文本", "Rich text"],
  text: ["文字", "Text"],
  image: ["图片", "Image"],
  receiving: ["正在接收…", "Receiving…"],
  receive: ["接收并销毁", "Receive & delete"],
  oneTimeTitle: ["这是一份一次性内容", "A one-time message"],
  oneTimeHint: [
    "确认后接收全部内容，并销毁服务端副本。完整接收后可在本页重试复制；关闭或刷新后无法恢复。",
    "Confirm to receive everything and delete the server copy. After a complete transfer you can retry copying here. Closing or refreshing loses it.",
  ],
  oneTimeNetwork: [
    "接收中断时，服务端副本可能已销毁；无法保证恢复。",
    "If transfer is interrupted, the server copy may already be deleted.",
  ],
  received: [
    "内容已接收，服务端副本已销毁。请保留此页面直到复制或下载完成。",
    "Received. Server copy deleted. Keep this page open until you finish copying or downloading.",
  ],
  receivedCopied: [
    "已接收并复制。服务端副本已销毁。",
    "Received and copied. Server copy deleted.",
  ],
  contents: ["收到一份内容", "Something for you"],
  download: ["下载", "Download"],
  downloadAll: ["打包下载", "Download bundle"],
  details: ["详细信息", "Details"],
  created: ["创建时间", "Created"],
  expires: ["到期时间", "Expires"],
  delete: ["删除分享", "Delete share"],
  deleteConfirm: [
    "删除后，链接将立即失效。确定删除？",
    "Delete this share? The link will stop working immediately.",
  ],
  deleting: ["正在删除…", "Deleting…"],
  deleted: ["分享已删除。", "Share deleted."],
  notFound: ["内容不存在或已失效", "Content unavailable"],
  notFoundHint: [
    "可能已到期、被删除，或一次性内容已被接收。",
    "It may have expired, been deleted, or already been received.",
  ],
  help: ["命令行与使用帮助", "Command line & help"],
  shortcut: ["Ctrl / ⌘ + Enter 发送", "Ctrl / ⌘ + Enter to send"],
  helpText: [
    "命令行入口保持兼容；此站点需要入口认证，请让 curl 提示输入密码。",
    "CLI endpoints remain compatible. This site requires access authentication; let curl prompt for the password.",
  ],
  drop: ["放下文件，添加到待发送内容", "Drop files to add them"],
  loading: ["正在加载…", "Loading…"],
  limits: [
    "每次分享请求上限 {size} · 默认 {hours} 小时",
    "Up to {size} per request · {hours} hours by default",
  ],
  tooLarge: [
    "请求超过上传限制；文件与 multipart 开销合计不能超过 {size}。",
    "Request too large. Files plus multipart overhead must fit within {size}.",
  ],
  tooMany: ["每次最多 {count} 个条目。", "Up to {count} items per share."],
  authentication: [
    "访问认证失败，请重新登录；已保留待发送内容。",
    "Authentication failed. Sign in again; your draft is retained.",
  ],
  network: [
    "网络请求未完成。若正在上传，结果尚未确认，请勿连续重试。",
    "Network request did not complete. Upload outcome may be unknown; avoid repeated retries.",
  ],
  failed: [
    "操作未完成，请重试。",
    "Could not complete the operation. Please retry.",
  ],
  unsupported: [
    "浏览器未提供可读取的内容，请使用系统粘贴、拖放或添加文件。",
    "No readable clipboard content. Use system paste, drop, or Add files.",
  ],
  pathHint: [
    "检测到路径文字；如需传文件，请点击“添加文件”选择实际文件。",
    "This looks like a path. Use Add files to select the actual file.",
  ],
  richEdit: [
    "编辑文字后按纯文本发送；未编辑时保留富文本。",
    "Editing sends plain text. Unedited rich text keeps its formatting.",
  ],
  retry: ["重试加载", "Retry loading"],
  source: ["开源维护", "Source"],
  enableJS: [
    "请启用 JavaScript 使用粘贴与接收功能。",
    "Enable JavaScript to paste and receive.",
  ],
};
export function t(key, values = {}) {
  let value = messages[key]?.[language() === "en" ? 1 : 0] ?? key;
  for (const [name, replacement] of Object.entries(values))
    value = value.replaceAll(`{${name}}`, replacement);
  return value;
}
export function translate(root = document) {
  root.querySelectorAll("[data-i18n]").forEach((el) => {
    el.textContent = t(el.dataset.i18n);
  });
  root.querySelectorAll("[data-i18n-placeholder]").forEach((el) => {
    el.placeholder = t(el.dataset.i18nPlaceholder);
  });
  document.title = `nclip · ${t("tagline")}`;
}
export function sizeLabel(bytes) {
  return bytes < 1024
    ? `${bytes} B`
    : bytes < 1048576
      ? `${(bytes / 1024).toFixed(1)} KiB`
      : `${(bytes / 1048576).toFixed(1)} MiB`;
}
