# Accounts and access

Account mode provides an ordinary HTML login form (`username`, `password`, `autocomplete=username/current-password`). Bitwarden and browser password managers can recognize the native form; it submits normally without relying on JavaScript input events.

## Model and behavior

There are only users and sessions. One unique, immutable administrator is identified by `permissions.manage`; ordinary users cannot be granted manage permission. The administrator has upload, download, and manage permission. Management permits account CRUD and listing/deleting all shares; it does not itself grant upload or private content download. The administrator cannot be deleted, renamed, disabled, have permissions changed, or receive an explicit per-user upload cap. Its password can still be reset.

Ordinary users also have an optional `upload_limit_bytes` content cap. New ordinary users start with a 100 MiB cap; `null` means there is no per-user cap. The administrator always has no per-user cap and this setting cannot be changed for that account. The server hard ceiling is `NCLIP_BUFFER_SIZE`, so a user cap cannot raise it. Caps are set in whole MiB from 1–100. The admin interface displays each setting and lets the administrator change ordinary users' caps independently of their upload and download permissions.

- Upload only: composer home; protected share results retain copy actions and omit the inaccessible preview action.
- Download only: link/code entry home with explicit clipboard paste and opt-in clipboard suggestions. Direct share links return to their original path after login.
- Upload and download: composer is primary; receiving is a secondary entry point and a clipboard-link shortcut.
- Administrator: administration home plus upload and download capabilities; after sign-in, the home route opens administration first.
- No permissions: a clear permission page and sign-out access.

Existing and new shares default to authenticated users with download permission. A sender can explicitly enable **Public sharing** for an individual share; anyone with that link can read/receive it without login, including users who do not have private download permission. The only additional share field is `public: true`; there are no per-user share ACLs or ownership records. Missing/expired links return the normal not-found page, including public one-time links after consumption.

## Initialization and persistence

Enable `NCLIP_ACCOUNTS=true`, set `NCLIP_ADMIN_USERNAME`, and mount a private bootstrap secret file named by `NCLIP_ADMIN_PASSWORD_FILE`. The secret is read **only when no account database exists**. Existing databases are never reseeded by a stale environment setting or secret. Choose the initial credentials privately; do not publish them in migration notes.

Accounts and sessions are stored under `.accounts/accounts.json` inside the configured data directory, atomically replaced with file mode 0600 in a 0700 directory. Version 1 databases migrate to version 2 on startup: a sole enabled manager becomes the administrator; when several enabled managers exist, set `NCLIP_OWNER_USERNAME` to one of them before startup. Migration removes manage permission from all other users, assigns them the 100 MiB default per-user cap, clears old sessions, and fails closed if the administrator is ambiguous or invalid. Clipboard listings never traverse this private subdirectory. Passwords use bcrypt cost 12, with 8–72 byte passwords; session authentication stores only SHA-256 token hashes. No raw passwords/session tokens go into browser storage or account JSON. The private account file must be included in data backups.

This mode requires a **single persistent filesystem server**. It intentionally does not add distributed account coordination for Lambda/S3. Leave accounts disabled to retain the upstream standalone behavior and legacy upload-key option; account mode itself uses per-user sessions/permissions rather than global upload keys.

## Session and request boundary

Sessions last seven days, with at most ten sessions per user. Cookies are HttpOnly, SameSite=Lax, Secure on the HTTPS deployment, and use the `__Host-` prefix there. Login creates a fresh opaque token; sign-out revokes its server record. Account edits increment a version and invalidate all prior sessions. Password verification and session issuance bind to the same version, preventing a password-reset race from issuing a stale session.

Mutations with session cookies require the CSRF token from `GET /api/v1/session` and a same-origin request. Login checks origin and uses bounded verification concurrency plus per-client failure throttling. HTTPS/loopback proxy deployments trust only loopback and the Docker private network for client-address resolution. Raw HTML/SVG remains sandboxed.

Explicit CLI `Authorization: Basic` credentials remain supported by the application with the same account permissions. It never emits a Basic challenge. Browser requests ignore cached proxy Basic headers so sign-out remains effective after the migration. Static login assets and health checks are public; content and administrative endpoints enforce permissions in middleware regardless of hidden UI controls.

## HTTP interfaces

| Endpoint | Purpose |
|---|---|
| `GET /login`, `POST /login` | Password-manager-compatible native login and local-only `next` return path |
| `POST /api/v1/login` | JSON login, returning current user/CSRF/return path |
| `GET /api/v1/session` | Current public identity and CSRF; no hash/token disclosure |
| `POST /api/v1/logout` | Revoke the current session |
| `GET /receive` | Authorized link/code entry |
| `GET /admin` | Management interface |
| `GET /api/v1/users`, `POST /api/v1/users` | List/create users |
| `PUT /api/v1/users/:id`, `DELETE /api/v1/users/:id` | Replace settings/delete a user |
| `GET /api/v1/admin/shares` | Search/paginate share metadata, not payloads |
| `DELETE /api/v1/admin/shares/:slug` | Delete any share with manage permission |
| `GET /api/v1/settings/upload`, `PUT /api/v1/settings/upload` | Read/update the browser sharing content quota with manage permission, CSRF and revision checks |

The browser sharing quota setting is configured in whole MiB from 1–100 and applies to aggregate binary content in `POST /api/v1/shares`. Its effective limit is the minimum of that setting, `NCLIP_BUFFER_SIZE`, and the user's per-user cap when one is set. Legacy upload endpoints use the minimum of the user's cap and `NCLIP_BUFFER_SIZE`. `null` disables only the per-user cap; users without one, including the administrator and core accounts, are not reduced by the browser-share setting. The settings file is stored under the data directory and survives restarts.

User creation requires `username`, `password`, and explicit `upload` and `download` permission booleans. `permissions.manage` is true only for the immutable administrator and false for ordinary users. `upload_limit_bytes` is optional and accepts a whole-MiB byte count from 1–100 MiB or `null`; ordinary new accounts default to 100 MiB. Updates also send the returned `version`; a stale version returns 409 `user_changed`. A blank update password preserves the old hash. Usernames are case-insensitive and use 3–64 ASCII letters/digits/dots/underscores/hyphens. No user self-registration or email identities are introduced.

Share creation accepts `X-Nclip-Visibility: public` or `authenticated` (default). The web checkbox applies per send and resets for the next share. All legacy content-reading routes, including raw/meta/CLI and confirmed one-time receiving, enforce the same access policy.

## Verification and rollout

Run Go tests/race/vet/lint, the Node clipboard/account checks, and `scripts/accounts-browser-test.mjs` against isolated data. Browser tests cover native autofill-like form values, permission-specific landing/navigation, direct-link login return, user CRUD, immutable administrator constraints, public one-time receiving, session revocation, CSRF, version conflicts, and 320/390px themes. Real extension-vault interaction is distinct from standards-based form compatibility.

Deploy behind the old proxy password first. Back up and verify existing data/configuration, initialize the administrator account, and test application access on a loopback preview port under the production memory/security settings. Only after those checks should the site's `auth_basic` directives be disabled and OpenResty syntax-checked/reloaded. Preserve TLS, request-size limits, data, and other services.

Rollback must **restore the old proxy authentication before starting an image without account enforcement**. Retain the new private account data, current shares, old image/container, exact proxy configuration, and verified backup. Never expose the old application while proxy authentication is disabled. The old proxy credentials and new account passwords may diverge after later password changes; account initialization is deliberately not a password-reset mechanism.

For human acceptance and generic maintenance steps, see [OPERATIONS.zh-CN.md](OPERATIONS.zh-CN.md). Deployment-specific records belong outside the public repository.
