# Integration and browser tests

Use isolated data and ports. The shell suite deliberately creates expired/corrupt records and removes recorded test files; it must never target production. Run only one legacy shell suite at a time because its temporary slug list is shared under `/tmp`.

## Go and lightweight client checks

```sh
go vet ./...
go test -race ./...
golangci-lint run
node --test scripts/clipboard.test.mjs scripts/accounts.test.mjs
```

The `Share preview` workflow also checks formatting, runs Linux race/lint and legacy HTTP integration, then builds and exports a fixed-commit Docker archive. See [human operations](OPERATIONS.zh-CN.md) for obtaining and retaining that artifact.

## Legacy HTTP integration (accounts disabled)

Linux/macOS/WSL Bash; requires Go, curl and the utilities used in `scripts/integration/`:

```bash
set -euo pipefail
go build -o /tmp/nclip-integration .
export NCLIP_DATA_DIR=$(mktemp -d /tmp/nclip-integration.XXXXXX)
export NCLIP_PORT=18111 NCLIP_URL=http://127.0.0.1:18111
export NCLIP_BUFFER_SIZE=5242880 NCLIP_UPLOAD_AUTH=false NCLIP_ACCOUNTS=false
/tmp/nclip-integration > /tmp/nclip-integration.log 2>&1 &
server_pid=$!
trap 'kill "$server_pid" 2>/dev/null || true' EXIT
bash scripts/integration-test.sh
```

The suite checks health, upload/retrieval, raw/meta/delete, text/binary extensions, base64/burn combinations, TTL, expiration, collisions, buffer limits, previews and size mismatches. To test the legacy API-key mode, set `NCLIP_UPLOAD_AUTH=true`, `NCLIP_API_KEYS` and matching `NCLIP_TEST_API_KEY` **before starting the server**. API-key mode is separate from account mode.

The shared library `scripts/integration/lib.sh` removes the slugs recorded by `record_slug()` and its own temporary-file patterns. It does not scan all recent production files. Keep the generated temporary data directory for failure investigation; remove only that exact directory after confirming the test process has stopped. Never use broad `pkill nclip` or delete a shared data path.

`.github/workflows/integration-test.yml` exercises a local filesystem server with upload keys both enabled and disabled. This is not a real AWS/S3 end-to-end test; Go storage tests provide separate coverage. Account behavior is covered by Go HTTP tests and the browser suite below.

## Account and clipboard browsers

Use an isolated account-mode server with a fresh directory and private bootstrap password file. The fixture account below is `admin` / `test-admin-password`, **only for testing**. Configure:

- `NCLIP_ACCOUNTS=true`, `NCLIP_ADMIN_USERNAME=admin`;
- `NCLIP_ADMIN_PASSWORD_FILE` pointing to that fixture file;
- `NCLIP_PORT=18081`, `NCLIP_URL=http://127.0.0.1:18081`;
- `NCLIP_BUFFER_SIZE=10485760`, `NCLIP_TTL=72h`, and a separate `NCLIP_DATA_DIR`.

For container testing, the data directory must be writable by UID/GID 1001 and the read-only secret readable by UID 1001. Reuse the production security/resource settings; the near-limit case must pass at 128 MiB with no extra swap. Never mount production data or credentials in the fixture.

Install Playwright separately (`npm install --no-save playwright`, or supply an existing package path). It is a test dependency, not a frontend/runtime dependency. Use an installed Edge/Chrome channel or install Playwright Chromium. Example in PowerShell while the test server is running:

```powershell
$env:NCLIP_TEST_URL='http://127.0.0.1:18081'
$env:NCLIP_TEST_ADMIN='admin'
$env:NCLIP_TEST_PASSWORD='test-admin-password'
$env:NCLIP_TEST_ACCOUNTS='true'
$env:NCLIP_BROWSER_CHANNEL='msedge' # repeat with chrome if installed
$env:NCLIP_EVIDENCE_DIR='.\evidence\accounts'
node scripts/accounts-browser-test.mjs
$env:NCLIP_EVIDENCE_DIR='.\evidence\clipboard'
node scripts/browser-test.mjs
```

`NCLIP_PLAYWRIGHT_MODULE` optionally names an installed Playwright package directory. Both helpers clean up only users/shares they created. The account suite tests native form values without input events, all independent roles, direct-link login return, native code-entry fallback, user CRUD, last-manager protection, revocation, stale edits, CSRF and anonymous public one-time receiving. The clipboard suite tests text/rich/multiple files, one-time ZIP retention, oversized draft retention, read races, clipboard refusal and 320/390px themes/touch emulation.

For a standalone server, omit `NCLIP_TEST_ACCOUNTS` and run only `browser-test.mjs` with the same 10 MiB UI limit. Screenshots are written to the evidence directory. Mobile emulation cannot establish real Safari compatibility; native Bitwarden vault interaction is a separate human acceptance step.

## Container HTTP smoke and size/concurrency checks

Python 3.10+ standard library, no third-party dependencies:

```bash
python3 scripts/share-smoke.py http://127.0.0.1:18081 \
  --username admin --password-file /path/to/fixture-password --large
```

The password is read from a file, not passed as a command argument. When verifying a configured HTTPS origin through loopback, `--origin https://your-test-host` supplies the intended origin. The helper creates/removes only its own slugs; it checks text/raw/CLI, rich multi-part bundles, confirmed one-time receiving, ZIP retention and twenty competing destructive reads with one winner. `--large` adds 32 logical items totaling 9.375 MiB and an 11 MiB rejection. Inspect Docker memory/OOM state and report actual measurements; do not claim a sampled value is a peak.

Use this on an isolated instance, then verify production with disposable manually created shares and known ordinary legacy links. Never consume somebody else's one-time content as a health check.
