# nclip

[![Test](https://github.com/ARCJ137442/nclip/actions/workflows/test.yml/badge.svg)](https://github.com/ARCJ137442/nclip/actions/workflows/test.yml)

A Go/Gin clipboard and file-sharing service. This is the maintained
[ARCJ137442/nclip fork](https://github.com/ARCJ137442/nclip) of
[johnwmail/nclip](https://github.com/johnwmail/nclip).

- One composer for text, clipboard HTML/PNG, screenshots and multiple files.
- One link per message; ordinary copy/download or confirmed one-time receiving.
- Optional native account login, independent upload/download/manage permissions,
  user management and all-share administration.
- Chinese/English, system/light/dark appearance, keyboard and touch controls.
- Filesystem server or standalone Lambda/S3. Account mode and destructive-reader
  coordination require one persistent filesystem process.

This is a **public repository**. Examples use reserved hosts and fixture accounts.
Private deployment inventory and credentials belong outside the checkout; see
[publication boundaries](SECURITY.md).

## Quick start

Requires Go 1.26+. Run from the repository directory: templates and assets are
loaded from `static/` at runtime, so copy that directory alongside packaged binaries.

```bash
git clone https://github.com/ARCJ137442/nclip.git
cd nclip
go build -o nclip .
./nclip
```

Open `http://localhost:8080`. By default, uploads and reads are unauthenticated;
use standalone mode only in a trusted environment or configure authentication.

```bash
printf 'hello' | curl --fail-with-body --data-binary @- http://localhost:8080/
# Substitute the slug returned by the upload:
curl --fail-with-body http://localhost:8080/raw/SLUG
```

For account mode, create a private password file **outside the checkout**, set
`NCLIP_ACCOUNTS=true`, `NCLIP_ADMIN_USERNAME` and
`NCLIP_ADMIN_PASSWORD_FILE`, then start the server. First startup creates exactly
one all-permissions manager; an existing database is never reseeded. Full behavior
and endpoints: [Accounts](Documents/ACCOUNTS.md).

To build the fork's container locally:

```bash
docker build -t nclip:local .
```

The image runs as UID/GID 1001. Mount writable persistent data and a read-only
bootstrap secret for account mode. The supplied `docker-compose.yml` is a
standalone example with its own volume and port; inspect it before use and do not
apply it over an existing instance. Official upstream binaries/images do not
include this fork's additions. See [manual maintenance](Documents/OPERATIONS.zh-CN.md).

## Configuration

`config/config.go` defines these defaults. Nonempty environment settings override
CLI flags. Use Go duration syntax (`72h`, not `3d`). These are application defaults,
not the settings of any particular deployment.

| Environment variable | CLI flag | Default | Purpose |
|---|---|---|---|
| `NCLIP_PORT` | `--port` | `8080` | HTTP listen port |
| `NCLIP_DATA_DIR` | `--data-dir` | `./data` | Persistent filesystem data |
| `NCLIP_URL` | `--url` | empty | Base URL for generated links; inferred if unset |
| `NCLIP_SLUG_LENGTH` | `--slug-length` | `5` | Generated slug length, 3–32 |
| `NCLIP_BUFFER_SIZE` | `--buffer-size` | `5242880` | Upload limit in bytes |
| `NCLIP_TTL` | `--ttl` | `24h` | Default lifetime |
| `NCLIP_MAX_RENDER_SIZE` | `--max-render-size` | `262144` | Legacy inline preview threshold |
| `NCLIP_UPLOAD_AUTH` | `--upload-auth` | `false` | Standalone upload/delete API keys |
| `NCLIP_API_KEYS` | `--api-keys` | empty | Comma-separated standalone keys |
| `NCLIP_ACCOUNTS` | `--accounts` | `false` | Single-server accounts; replaces global upload keys |
| `NCLIP_ADMIN_USERNAME` | `--admin-username` | `admin` | Bootstrap username, first initialization only |
| `NCLIP_ADMIN_PASSWORD_FILE` | `--admin-password-file` | empty | Private bootstrap password file |
| `NCLIP_S3_BUCKET` | `--s3-bucket` | empty | S3 bucket in Lambda mode |
| `NCLIP_S3_PREFIX` | `--s3-prefix` | empty | S3 object prefix |

`GIN_MODE=release` selects Gin release mode. Account deployments should use HTTPS,
keep the HTTP listener private behind their trusted proxy, and back up hidden
account state with the data. Never put secrets in image layers or public CI logs.

## APIs and authentication

| API family | Authoritative reference |
|---|---|
| Native login, sessions, users and administration | [ACCOUNTS.md](Documents/ACCOUNTS.md) |
| Multipart share creation, metadata and receiving | [ARC-SHARING.md](Documents/ARC-SHARING.md) |
| Legacy upload/raw/delete/meta and CLI examples | [CLIENTS.md](Documents/CLIENTS.md) |
| Custom headers, duration and size semantics | [X-HEADERS.md](Documents/X-HEADERS.md) |

In account mode, upload, private download and management are independent. Private
links require login plus download permission; explicit public shares permit
anonymous receiving. CLI credentials use explicit Basic authorization without
browser challenges. Browser mutations use sessions and CSRF protection.

With accounts disabled, optional global API keys protect uploads and deletion;
reads remain public. Key mode is not a substitute for per-user authorization.
`GET /health` is public and does not demonstrate successful login or content access.

## Development and documentation

Go 1.26+, Node 22 for lightweight tests, Docker for image checks, and separately
installed Playwright for browser tests. The frontend has no build step or runtime
Node dependency.

```bash
go vet ./...
go test -race ./...
golangci-lint run
node --test scripts/clipboard.test.mjs scripts/accounts.test.mjs
node scripts/check-public-docs.mjs
```

Run formatting checks and isolated HTTP/browser regressions as described in
[INTEGRATION-TESTS.md](Documents/INTEGRATION-TESTS.md). Never point their cleanup
at real user data. `Share preview` exports a fixed-commit image and checksum;
it does not deploy automatically.

[Documentation index](Documents/README.md) ·
[Architecture](Documents/ARCHITECTURE.md) ·
[Maintainer instructions](AGENTS.md) ·
[License](LICENSE)
