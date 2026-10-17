import { t } from "./i18n.js";
const codes = {
  user_changed: "userChanged",
  invalid_credentials: "credentialError",
  login_required: "sessionExpired",
  permission_denied: "permissionDeniedHint",
  csrf_failed: "sessionChanged",
  login_throttled: "loginThrottled",
  password_length: "passwordLength",
  password_required: "passwordRequired",
  invalid_username: "usernameRules",
  username_exists: "usernameExists",
  last_manager: "lastManager",
  user_not_found: "userNotFound",
  user_limit: "userLimit",
  session_failed: "sessionFailed",
  account_write_failed: "accountWriteFailed",
  invalid_request: "invalidRequest",
};
export function accountMessage(code) {
  return t(codes[code] || "failed");
}
export function loginForm() {
  const form = document.getElementById("login-form"),
    error = document.getElementById("login-error");
  const render = () => {
    if (error.dataset.code)
      error.textContent = accountMessage(error.dataset.code);
  };
  render();
  window.addEventListener("preferenceschange", render);
  form.addEventListener("submit", () => {
    document.getElementById("login-submit").disabled = true;
    form.setAttribute("aria-busy", "true");
  });
}
