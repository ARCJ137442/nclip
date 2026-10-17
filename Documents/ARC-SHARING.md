# ARC clipboard sharing

The maintained source is the `ARCJ137442/nclip` fork. The supported filesystem deployment uses one server process; one-time consumption is serialized **within that process**, not across independent Lambda/S3 instances.

## User workflow

- One composer accepts typed text, clipboard text/HTML/PNG, pasted `File` objects, dropped files and multi-file selection. Enter inserts a newline; Ctrl/Cmd+Enter sends, except during IME composition.
- A send creates one URL for the entire message. Upload success and clipboard-write success are separate states. Failed uploads retain the draft; unknown upload outcomes are never automatically retried.
- Clipboard suggestions are opt-in. Automatic reads require an existing browser grant and a focused, visible page. Other browsers retain explicit Paste and native paste. Own share links are suppressed without storing clipboard contents.
- Clipboard HTML is stored as a representation and never inserted as live page HTML. Raw responses have a sandbox CSP and `nosniff` header. Preview images use local Blob URLs.
- Language and appearance independently support system/explicit selection. Unmatched system languages fall back to Chinese. Only preferences use localStorage; credentials, drafts and clipboard contents do not.

## HTTP contract

Account-mode deployments enforce independent permissions for all new and legacy endpoints. Private sharing requires login and download permission; senders may explicitly opt into public sharing. See [ACCOUNTS.md](ACCOUNTS.md). In standalone mode, the legacy upload-key option remains available. When migrating from an externally authenticated deployment, restore its authentication gate before reverting to an image without account enforcement.

| Endpoint | Behavior |
|---|---|
| `GET /api/v1/ui-config` | Non-secret upload/TTL/item limits and formats. Account mode requires login; it is not an anonymous capability endpoint. |
| `POST /api/v1/shares` | Multipart request containing a JSON `manifest` field and named binary parts. Returns 201 with `slug`, `url`, `expires_at`, `burn_after_read`. |
| `GET /api/v1/shares/:slug` | Metadata and manifest, without reading or consuming content. |
| `GET /api/v1/shares/:slug/content` | Multipart message for an ordinary share. Returns 409 for a one-time share. |
| `POST /api/v1/shares/:slug/receive` | Requires `X-Nclip-Confirm: 1`. Validates the entire stored message, consumes a one-time share, then returns its multipart content. |

Creation accepts `X-Burn: true` and the existing `X-TTL` duration contract (1h–168h). Without an override the configured TTL is authoritative. The request body, including multipart framing, cannot exceed `NCLIP_BUFFER_SIZE`; the logical binary total is checked separately. The default item limit is 32 and the manifest limit is 64 KiB. No automatic retry/idempotency guarantee is claimed.

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

Deletion precedes response delivery. An interrupted network response may be unrecoverable. A fully received message stays in page memory for copy/download retries; this does not restore a server copy. A browser clipboard permission refusal must never be reported as a successful copy. Multiple native clipboard objects and app-specific OS formats are not promised.

## Verification and release

```sh
go fmt ./...
go vet ./...
go test -race ./...
golangci-lint run
node --test scripts/clipboard.test.mjs
# Playwright installed separately; use only an isolated test instance:
NCLIP_TEST_URL=http://127.0.0.1:18080 node scripts/browser-test.mjs
```

`NCLIP_PLAYWRIGHT_MODULE` optionally points to an installed Playwright package; `NCLIP_BROWSER_CHANNEL=msedge` or `chrome` runs that installed browser. Screenshots go to `NCLIP_EVIDENCE_DIR`. These tests exercise native clipboard behavior on the host and create/delete only their own test slugs. Mobile viewport/touch emulation does not establish real iPhone Safari compatibility.

The `Share preview` Actions workflow runs vet, formatting, race and clipboard checks, builds a fixed-commit image, and exports a checksum plus Docker archive as a seven-day build artifact. It does not publish a registry image, create a release, or deploy automatically. The repository's pre-existing `v*` workflows publish releases/images; do not use such tags merely to build a preview.

Generic release, backup and rollback procedures live in [人工维护](OPERATIONS.zh-CN.md); private instance inventory stays outside this repository. Include hidden account state in backups and restore an external authentication gate before any rollback to an unguarded legacy image.
