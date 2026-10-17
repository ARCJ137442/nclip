import { language } from "./preferences.js";
const messages = {
  receiveAnother: ["接收其他分享", "Receive another share"],
  deleteUser: ["删除用户", "Delete user"],
  deleteAction: ["删除", "Delete"],
  receiveClipboardShort: ["接收 {code}", "Receive {code}"],
  userChanged: [
    "用户已被其他管理员更新。当前修改已保留，请取消后重新打开用户再修改。",
    "Another manager updated this account. Your draft is retained; cancel and reopen the account before editing again.",
  ],
  privateSpace: ["你的私有剪贴板", "Your private clipboard"],
  loginTitle: ["登录 nclip", "Sign in to nclip"],
  loginHint: ["登录后，继续传递内容。", "Sign in to keep things moving."],
  login: ["登录", "Sign in"],
  logout: ["退出登录", "Sign out"],
  username: ["用户名", "Username"],
  password: ["密码", "Password"],
  loginAccountHint: [
    "账号由管理员创建，可使用密码管理器自动填充。",
    "Accounts are created by a manager. Your password manager can autofill this form.",
  ],
  credentialError: [
    "账号或密码不正确，请重试。",
    "Incorrect username or password. Please try again.",
  ],
  sessionExpired: [
    "登录已失效，请重新登录。当前输入仍保留在本页。",
    "Your session expired. Sign in again; your draft remains on this page.",
  ],
  sessionChanged: [
    "会话已改变，请重新登录后重试。",
    "Your session changed. Sign in again and retry.",
  ],
  sessionFailed: [
    "登录状态更新失败，请重试。",
    "Could not update the session. Please retry.",
  ],
  loginThrottled: [
    "尝试次数过多，请稍后再试。",
    "Too many sign-in attempts. Please try again later.",
  ],
  permissionDeniedTitle: [
    "当前账号没有这项权限",
    "This account does not have that permission",
  ],
  permissionDeniedHint: [
    "可返回可用页面，或联系管理员调整权限。",
    "Return to an available page or ask a manager to update your permissions.",
  ],
  backHome: ["返回首页", "Back to home"],
  uploadPage: ["发送", "Send"],
  downloadPage: ["接收", "Receive"],
  administration: ["管理", "Manage"],
  receiveTitle: ["接收一份内容", "Receive something"],
  receiveHint: [
    "输入分享链接或分享码，也可以从剪贴板直接打开。",
    "Enter a share link or code, or pick one from your clipboard.",
  ],
  shareCode: ["链接或分享码", "Share link or code"],
  openContent: ["打开内容", "Open share"],
  pasteLink: ["粘贴链接", "Paste link"],
  invalidShareCode: [
    "请输入本站的分享链接，或 5–7 位分享码。",
    "Enter a share link from this site, or a 5–7 character code.",
  ],
  openClipboardCode: ["打开剪贴板中的 {code}", "Open {code} from clipboard"],
  receiveExisting: ["接收已有分享", "Receive an existing share"],
  publicShare: ["公网分享", "Public sharing"],
  publicShareHint: [
    "开启后，任何知道链接的人都可接收，无需登录。默认仅限已登录且有下载权限的用户。",
    "Anyone with this link can receive without signing in. By default, recipients must sign in and have download permission.",
  ],
  authorizedUsers: ["有权限用户", "Authorized users"],
  publicAccess: ["公网", "Public"],
  users: ["用户", "Users"],
  allShares: ["分享内容", "Shares"],
  createUser: ["创建用户", "Create user"],
  editUser: ["修改用户", "Edit user"],
  searchUsers: ["搜索用户", "Search users"],
  searchShares: ["分享码、文件名或类型", "Code, filename or type"],
  search: ["搜索", "Search"],
  previous: ["上一页", "Previous"],
  next: ["下一页", "Next"],
  permissions: ["权限", "Permissions"],
  permissionUpload: ["上传", "Upload"],
  permissionDownload: ["下载", "Download"],
  permissionManage: ["管理", "Manage"],
  managePermissionHint: [
    "管理用户及全部分享；上传和下载分别授权。",
    "Manage users and all shares. Upload and download are separate permissions.",
  ],
  disabledAccount: ["停用账号", "Disable account"],
  disabled: ["已停用", "Disabled"],
  active: ["可登录", "Active"],
  newPassword: ["新密码", "New password"],
  newPasswordHint: [
    "密码为 8–72 字节；修改时留空则保持原密码。保存后，该用户的旧会话会失效。",
    "Use 8–72 bytes. Leave blank when editing to keep the password. Saving revokes the user’s existing sessions.",
  ],
  save: ["保存", "Save"],
  cancel: ["取消", "Cancel"],
  edit: ["修改", "Edit"],
  confirmDelete: ["确认删除", "Confirm deletion"],
  passwordLength: ["密码必须为 8–72 字节。", "Password must be 8–72 bytes."],
  passwordRequired: [
    "创建用户时需要设置密码。",
    "A password is required for a new account.",
  ],
  usernameRules: [
    "用户名为 3–64 位英文字母、数字、点、下划线或连字符，以字母或数字开头。",
    "Use 3–64 letters, digits, dots, underscores or hyphens; start with a letter or digit.",
  ],
  usernameExists: ["这个用户名已经存在。", "That username already exists."],
  lastManager: [
    "必须保留至少一个可登录且具有管理权限的账号。",
    "Keep at least one enabled account with management permission.",
  ],
  userNotFound: [
    "用户不存在，可能已被其他管理员删除。",
    "User not found. Another manager may have removed it.",
  ],
  userLimit: ["用户数量已达到上限。", "The account limit has been reached."],
  accountWriteFailed: [
    "账号保存失败，原设置仍有效，请重试。",
    "Could not save the account. Previous settings remain active.",
  ],
  invalidRequest: [
    "请求内容无效，请检查后重试。",
    "Invalid request. Check the fields and retry.",
  ],
  userSaved: ["用户设置已保存。", "Account saved."],
  userDeleted: [
    "用户已删除，旧会话已失效。",
    "Account deleted; existing sessions revoked.",
  ],
  deleteUserPrompt: [
    "删除用户「{name}」？该用户将立即退出登录，已有分享内容仍保留。",
    "Delete “{name}”? Their sessions will be revoked. Existing shares remain.",
  ],
  deleteSharePrompt: [
    "删除分享 {code}？链接将立即失效。",
    "Delete share {code}? The link will stop working.",
  ],
  userCount: ["{count} 个用户", "{count} users"],
  shareCount: ["{count} 份 · {size}", "{count} shares · {size}"],
  pageCount: ["{page} / {pages}", "{page} / {pages}"],
  noUsers: ["没有匹配的用户。", "No matching users."],
  noShares: ["没有匹配的分享内容。", "No matching shares."],
  noPermissions: ["未分配权限", "No permissions"],
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
