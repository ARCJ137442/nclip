# HTTP header contract

This file owns the legacy header semantics. [Sharing](ARC-SHARING.md) defines
multipart manifests; [Accounts](ACCOUNTS.md) defines authentication and CSRF.

| Header | Meaning |
|---|---|
| `X-Base64` | Decode a legacy upload before storage |
| `X-Burn` | Make the entire share one-time |
| `X-TTL` | Override the configured lifetime |
| `X-Slug` | Request a custom validated slug |
| `X-Nclip-Visibility` | `authenticated` (default) or explicit `public` |
| `X-Nclip-Confirm` | `1` for the confirmed receive endpoint |
| `X-Api-Key`, `Authorization: Bearer` | Standalone upload-key mode only |
| `Authorization: Basic` | Explicit CLI account credentials in account mode |
| `X-CSRF-Token` | Session-cookie mutation protection in account mode |

## Boolean headers and route precedence

`handlers/upload/handler.go:headerEnabled` treats a present header as enabled
unless its value is `0`, `false` or `no` (case-insensitive). An empty present
`X-Base64` or `X-Burn` is enabled. Omit the header to use the normal route default.

- `POST /` honors the supplied headers.
- `POST /base64` sets `X-Base64: true` before the upload handler, overriding any
  client opt-out. Use `POST /` if the header must control decoding.
- `POST /burn/` always creates one-time content. Use `POST /` to omit/disable burn.
- `POST /api/v1/shares` accepts logical bytes and a manifest; it does not use
  legacy base64 decoding. Burn/TTL and visibility apply to the whole message.

## TTL, slugs and size limits

`X-TTL` uses `time.ParseDuration`: values must be between `1h` and `168h`.
Use `72h`; a `d` suffix is invalid. Without an override the configured default is
authoritative. Invalid or out-of-range request values are rejected.

Slugs are 3–32 characters from `A-HJ-NP-Z2-9` (uppercase, excluding I/O/0/1).
Invalid custom slugs are rejected; generated length follows configuration.

Legacy base64 accepts standard/URL-safe and padded/raw variants. Decoded bytes
must fit the content limit; the legacy handler accounts for encoding overhead
when bounding the encoded body. A reverse proxy may impose a tighter whole-body
limit. Base64 is a transport encoding, not encryption or access control.

The versioned multipart endpoint limits the **whole request**, including framing,
to `NCLIP_BUFFER_SIZE` and checks the logical byte total separately. A file whose
size equals that limit can therefore be rejected once multipart overhead is added.

## Authentication, public visibility and forwarding

With accounts disabled, configured global keys protect all upload routes and
legacy deletion; reads remain public. With accounts enabled, those keys do not
replace per-user permissions. Cookies need CSRF/origin checks for mutations;
explicit CLI credentials remain subject to the same capabilities.

`X-Nclip-Visibility: public` explicitly allows anonymous reads of the new share.
It does not grant upload, account management or deletion. Existing records without
that flag are private in account mode. `X-Nclip-Confirm: 1` indicates intent to
consume, not successful clipboard delivery.

Reverse proxies must preserve the intended host/protocol and required auth
headers. Configure their trusted boundary deliberately; do not accept client
forwarding headers as an alternative to authenticating the request.
