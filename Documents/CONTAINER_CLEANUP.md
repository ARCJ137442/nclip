# Container image and artifact retention

Server-local resources and private rollback records are maintained using
[人工维护](OPERATIONS.zh-CN.md). This document owns only the GHCR cleanup workflow
and the distinction between resource types.

| Resource | Owner / cleanup |
|---|---|
| `Share preview` archive | Actions artifact, seven-day retention; archive adopted builds separately |
| Versioned offline image/binaries | GitHub Release assets with checksums, maintained separately from temporary Actions artifacts |
| GHCR package versions | `.github/workflows/cleanup-container-images.yml` |
| Docker images/containers on a server | Operator; unaffected by GitHub package cleanup |
| Bind-mounted data, accounts, secrets and backups | Operator; never part of image cleanup |

## Workflow behavior

**Cleanup Container Images** runs monthly on the first day at 02:00 UTC or by
manual dispatch. Current scheduled defaults are dry-run: the monthly report alone
does not delete versions. A live manual run requires `dry_run=false`.

| Input | Default | Meaning |
|---|---|---|
| `retention_days` | `30` | Consider versions older than this age |
| `keep_latest_count` | `5` | Preserve this many newest versions regardless of age |
| `dry_run` | `true` | Report candidate deletions without deleting |

The workflow sorts by creation time, preserves the newest N versions and any
version with a `latest` tag, then considers remaining old versions. Other tags
are **not** an automatic keep rule. A rollback image with a different tag may
qualify for deletion, so keep independently verified archives.

## Manual procedure

1. Open the fork's Actions tab and select **Cleanup Container Images**.
2. Run with `dry_run=true` and inspect every candidate against your private
   adopted-build/rollback inventory.
3. Adjust retention if necessary; only run with `dry_run=false` when candidates
   can be removed. Keep source SHA, archive checksum and a recoverable image.
4. Verify the resulting package versions separately from server containers.

The workflow needs `packages: write` and access to the correct owner/package.
403 indicates insufficient package permissions; 404 can mean no package or no
access and should be interpreted with the workflow/API context. The script logs
its analysis and deletion errors. Do not infer that Docker volumes were backed up
or that a server image was removed from a successful GHCR cleanup report.

`release.yml` builds versioned binaries and an offline Linux amd64 Docker archive
into a draft GitHub Release. Review its source/checksums/assets, then publish it.
Already published versions cannot be rebuilt in place; use a new version.
The legacy `container.yml` registry publisher is scoped to the original upstream
owner and does not run in this fork. `Share preview` remains a temporary image
export and does not deploy an application.
