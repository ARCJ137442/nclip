# Client and legacy API guide

Examples use a local standalone server. Build/run it using the root
[README](../README.md). Substitute returned slugs; `SLUG` is only a placeholder.
Custom header behavior is maintained in [X-HEADERS.md](X-HEADERS.md).

## Authentication choices

- Account mode: explicit CLI `curl --user example-user` prompts for a password.
  Upload, private download and management require their independent permissions.
  Public-share reads may omit authentication. The browser uses the native login
  form, not a Basic challenge. Details: [Accounts](ACCOUNTS.md).
- Standalone with API keys: provide `X-Api-Key` or `Authorization: Bearer` for
  upload/deletion. Reads remain public. Example key strings are fixtures, not
  credentials to reuse.
- Standalone without keys: these legacy routes are unauthenticated.

## Route reference

| Method and path | Result |
|---|---|
| `POST /` | Upload raw bytes or legacy multipart file; return the share URL |
| `POST /burn/` | Upload one-time content |
| `POST /base64` | Decode a base64 upload before storing |
| `GET /:slug` | Browser view; recognized CLI clients receive content |
| `GET /raw/:slug` | Raw content or portable ZIP for bundles |
| `GET /api/v1/meta/:slug`, `GET /json/:slug` | Metadata without content/consumption |
| `DELETE /:slug` | Delete content and metadata |
| `GET /health` | Public service health |

For raw and CLI one-time reads, a read consumes content. Browser one-time views
wait for confirmation. New multipart message APIs are specified separately in
[ARC-SHARING.md](ARC-SHARING.md).

## curl

```bash
printf 'hello' | curl --fail-with-body --data-binary @- http://localhost:8080/
curl --fail-with-body --data-binary @report.pdf http://localhost:8080/
curl --fail-with-body -F 'file=@report.pdf' http://localhost:8080/
curl --fail-with-body http://localhost:8080/raw/SLUG
curl --fail-with-body http://localhost:8080/api/v1/meta/SLUG
curl --fail-with-body -X DELETE http://localhost:8080/SLUG

# Account mode: curl prompts for the user's password.
curl --user example-user --fail-with-body --data-binary @report.pdf https://clipboard.example.com/

# Standalone key mode only; replace the fixture with your private key.
printf 'hello' | curl --fail-with-body -H 'X-Api-Key: example-key' --data-binary @- http://localhost:8080/

# One-time, two-hour content:
printf 'temporary' | curl --fail-with-body -H 'X-Burn: true' -H 'X-TTL: 2h' --data-binary @- http://localhost:8080/
```

Use `--data-binary` when exact bytes and trailing newlines matter. Legacy multipart
accepts a file; use the versioned sharing endpoint for multiple logical items.
Do not automatically retry a timed-out upload whose result is unknown.

## PowerShell

Use `curl.exe` for identical curl arguments on Windows, or native commands:

```powershell
Invoke-RestMethod -Uri http://localhost:8080/ -Method Post -Body 'Hello from PowerShell'
Invoke-RestMethod -Uri http://localhost:8080/ -Method Post -InFile '.\report.pdf'
Invoke-RestMethod -Uri http://localhost:8080/api/v1/meta/SLUG
Invoke-RestMethod -Uri http://localhost:8080/SLUG -Method Delete
```

In key mode add `-Headers @{'X-Api-Key'='example-key'}`. For account-mode CLI
testing, `curl.exe --user example-user` avoids putting the password in command
arguments. Store real credentials outside shell history and public screenshots.

## Other clients and failures

For wget, use `--method=POST --body-file=report.pdf`; for HTTPie use
`http POST http://localhost:8080/ < report.pdf`. Supply the same authentication and
headers as above rather than creating a different protocol.

400 means invalid input/header/slug; 401 means authentication is missing; 403
means the account lacks permission; 404 means missing/expired/consumed content;
413 means an upload exceeds a relevant limit. Account cookie requests additionally
need CSRF handling; do not copy a browser session token into public examples.
