# ARC clipboard sharing

The maintained source is the `ARCJ137442/nclip` fork. The supported filesystem deployment uses one server process; one-time consumption is serialized **within that process**, not across independent Lambda/S3 instances.

## User workflow

- One composer accepts typed text, clipboard text/HTML/PNG, pasted `File` objects, dropped files and multi-file selection. Enter inserts a newline; Ctrl/Cmd+Enter sends, except during IME composition.
- A send creates one URL for the entire message. Upload success and clipboard-write success are separate states. Failed uploads retain the draft; unknown upload outcomes are never automatically retried.
- Large transfers are observable: uploads show preparation, body transfer, server save and completion; report sent bytes, rate and remaining time; and can be canceled or retried. The failure UI distinguishes a server rejection from a lost response. Downloads show connection, receipt, verification and ready stages with received bytes plus a percentage when the response declares its length. The composer snapshots the sent content, so editing can continue during an upload; a finished link includes the sent size, duration, delivery scope and item names without discarding the newer draft.
- Clipboard suggestions are opt-in. Automatic reads require an existing browser grant and a focused, visible page. Other browsers retain explicit Paste and native paste. Own share links are suppressed without storing clipboard contents.
- Clipboard HTML is stored as a representation and never inserted as live page HTML. Raw responses have a sandbox CSP and `nosniff` header. Preview images use local Blob URLs.
- Language and appearance independently support system/explicit selection. Unmatched system languages fall back to Chinese. Only preferences use localStorage; credentials, drafts and clipboard contents do not.

## HTTP contract

Account-mode deployments enforce independent permissions for all new and legacy endpoints. Private sharing requires login and download permission; senders may explicitly opt into public sharing. See [ACCOUNTS.md](ACCOUNTS.md). In standalone mode, the legacy upload-key option remains available. When migrating from an externally authenticated deployment, restore its authentication gate before reverting to an image without account enforcement.

Account settings may also give each user an optional per-upload content cap through `upload_limit_bytes`. Ordinary new users default to 100 MiB; `null` means no per-user cap. Caps are whole MiB from 1–100; effective uploads also remain bounded by `NCLIP_BUFFER_SIZE`. This cap does not change share visibility or recipient download permissions.

| Endpoint | Behavior |
|---|---|
| `GET /api/v1/ui-config` | Non-secret upload/TTL/item limits and formats. Account mode requires login; it is not an anonymous capability endpoint. |
| `POST /api/v1/shares` | Multipart request containing a JSON `manifest` field and named binary parts. Returns 201 with `slug`, `url`, `expires_at`, `burn_after_read`. |
| `GET /api/v1/shares/:slug` | Metadata and manifest, without reading or consuming content. |
| `GET /api/v1/shares/:slug/content` | Multipart message for an ordinary share. Returns 409 for a one-time share. |
| `POST /api/v1/shares/:slug/receive` | Requires `X-Nclip-Confirm: 1`. Validates the entire stored message, consumes a one-time share, then returns its multipart content. |
| `POST /api/v1/channels/:id/content` | Sender-only chunked upload initialization with a manifest; every request also follows the configured account upload policy or standalone UploadAuth key. |
| `PUT /api/v1/channels/:id/content/:uploadID/parts/:partKey` | Sender-only, fixed 4 MiB chunks (last chunk may be smaller), addressed by `Upload-Offset` and verified with `X-Chunk-SHA256`. |
| `POST /api/v1/channels/:id/content/:uploadID/finalize` | Validates all parts and stores an ephemeral channel paste; repeated finalize with the same idempotency key returns the same held result. |
| `DELETE /api/v1/channels/:id/content/:uploadID` | Aborts an unfinished sender upload and removes its staging files. Closing the channel or releasing its receiver also deletes held ephemeral content. |

Channel pastes carry an internal `ephemeral_channel_id`. Ordinary share metadata/content/receive, legacy slug, raw, JSON and metadata routes return 404 for them; management lists omit them. The internal 168-hour fallback is for orphan cleanup only, is never returned as a user TTL, and cannot delete content while its channel is active. Server startup removes orphan staging files and channel pastes before measuring the channel filesystem budget. Channel state and reservations require one server process; Lambda/serverless channel routes fail closed.

Creation accepts `X-Burn: true` and the existing `X-TTL` duration contract (1h–168h). The browser defaults to 24 hours, offers preset and custom whole-hour values in that range, and sends non-default values as `X-TTL`; the server remains authoritative. The request body, including multipart framing, is bounded separately from the logical binary total. In account mode, managers configure the browser share content quota from 1–100 MiB, bounded by `NCLIP_BUFFER_SIZE`. The request boundary reserves space for the manifest and multipart framing. In account mode, the multipart request boundary reserves space for the manifest and framing, and the logical binary total is limited by the server limit, the global browser-share setting for users with a per-user cap, and that user's cap. Users without a per-user cap are limited by the server value. Other upload routes combine the server limit with the account's optional cap. The default item limit is 32 and the manifest limit is 64 KiB. No automatic retry/idempotency guarantee is claimed.

Each clipboard manifest item may include a server-generated `preview`: up to 24 Unicode code points from its plain-text part, with control characters and line breaks folded to spaces. Likely credentials and long base64-like tokens suppress the preview. This opening hint is visible to anyone holding the share link and also appears with paste metadata in the administrator list. It helps recipients recognize the handoff; it is not a privacy promise. File items do not include previews.

Content responses declare `Content-Length` whenever the body can be measured. The length comes from the same multipart framing that is written, so clients can render determinate download progress without buffering a second copy. Clients must still handle an absent or unknown length by showing received bytes without a percentage.

Request and response payloads use bounded-memory temporary files rather than payload-sized byte slices. Multipart parts are spooled individually, base64 is decoded as a stream, stored shares are built as a streamed ZIP, and browser reception validates the archive before the one-time delete. Memory use therefore does not grow linearly with the upload limit; temporary disk capacity and cleanup are part of the deployment budget.

Example manifest (part sizes/encoding/archive paths are assigned by the server):

```json
{"version":1,"items":[{"kind":"clipboard","name":"text.txt","parts":[{"key":"p0","mime":"text/plain"},{"key":"p1","mime":"text/html"}]},{"kind":"file","name":"report.pdf","parts":[{"key":"p2","mime":"application/pdf"}]}]}
```

Clipboard representations are `text/plain`, `text/html`, `image/png`. A file has one representation with its file MIME. Keys are unique ASCII names; unknown, duplicate and missing parts are rejected. Names are basenames without control characters. Responses use ASCII multipart filenames for browser parser compatibility; display/download names come from the manifest.

## Storage and compatibility

The existing `{slug}` content plus `{slug}.json` metadata layout is retained. An optional `share` manifest is added to metadata; old records are normalized on read without migration.

- `raw`: one logical item and one representation; content remains the original bytes, so old raw and CLI readers still work.
- `zip-v1`: multiple representations/items; a standard server-created ZIP stores entries under `items/` and a `manifest.json`. Raw and CLI readers receive this portable archive. Clients never upload an archive for unpacking.
- Content responses are `multipart/form-data`, parsed by browser `Response.formData()`. No frontend ZIP dependency or base64 expansion is needed.
- A one-time bundle response also includes the original ZIP in an `archive` file part. After server deletion, the browser can still download the ZIP from memory.
- An old application image can ignore the additional JSON field and serve new raw data/ZIP archives. No data migration is required for rollback.

## One-time semantics

Browser `GET /:slug` displays metadata only for one-time content. It does not load a thumbnail/body, increment reads or delete the message. A confirmed receive and direct legacy raw/CLI reads share `PasteService.ConsumePaste`; a bounded set of lock stripes covers fresh metadata lookup, complete read, size verification and deletion. Only one destructive reader succeeds in the current single-instance deployment.

The current one-time receive path consumes the server record before response delivery, so an interrupted response may be unrecoverable. A fully received message stays in page memory for copy/download retries; this does not restore a server copy. Channel mode introduced in P1a will guarantee single-read with an exclusive lock, independent of deletion timing. A browser clipboard permission refusal must never be reported as a successful copy. Multiple native clipboard objects and app-specific OS formats are not promised. The receive UI offers cancellation only for ordinary shares, whose server copy survives; a one-time transfer has no cancel action because aborting it may destroy the only server copy.

## Verification and release

```sh
go fmt ./...
go vet ./...
go test -race ./...
golangci-lint run
node --test scripts/clipboard.test.mjs scripts/accounts.test.mjs scripts/transfer.test.mjs
# Playwright installed separately; use only an isolated test instance:
NCLIP_TEST_URL=http://127.0.0.1:18080 node scripts/browser-test.mjs
```

`NCLIP_PLAYWRIGHT_MODULE` optionally points to an installed Playwright package; `NCLIP_BROWSER_CHANNEL=msedge` or `chrome` runs that installed browser. Screenshots go to `NCLIP_EVIDENCE_DIR`. These tests exercise native clipboard behavior on the host and create/delete only their own test slugs. Mobile viewport/touch emulation does not establish real iPhone Safari compatibility.

The `Share preview` Actions workflow runs vet, formatting, race and clipboard checks, builds a fixed-commit image, and exports a checksum plus Docker archive as a seven-day build artifact. It does not publish a registry image, create a release, or deploy automatically. The repository's pre-existing `v*` workflows publish releases/images; do not use such tags merely to build a preview.

Generic release, backup and rollback procedures live in [人工维护](OPERATIONS.zh-CN.md); private instance inventory stays outside this repository. Include hidden account state in backups and restore an external authentication gate before any rollback to an unguarded legacy image.
