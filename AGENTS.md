# Repository conventions

This is ARCJ137442/nclip, a fork of johnwmail/nclip. Check remote URLs before
pushing: a remote named `upstream` may point to the personal fork. Official
`latest` images do not contain this fork's additions.

This repository is PUBLIC. Use example.com/example credentials in public docs.
Keep real domains, hosts, ports, usernames, local paths, deployment snapshots,
backups and credentials outside the checkout. Check commits, PR bodies, logs
and artifacts for disclosure before publishing; `.gitignore` alone is not a scan.

- Keep Go + native HTML/CSS/JS. Users, independent permissions and per-share
  public opt-in are sufficient; do not invent ownership, groups or ACLs.
- Account mode requires one persistent filesystem writer. Protect all legacy
  raw/meta/CLI routes in middleware as well as new APIs. Manage does not grant
  upload/download. Preserve last-manager protection and session revocation.
- `NCLIP_ACCOUNTS`, `NCLIP_ADMIN_USERNAME`, `NCLIP_ADMIN_PASSWORD_FILE` configure
  accounts; bootstrap secrets never reset an existing database. Back up hidden
  `.accounts/accounts.json` privately; never commit credentials or content.
- One-time HTML views do not consume content. Coordinate confirmed/legacy
  destructive reads in-process. Server deletion is not clipboard success.
- Preserve smooth clipboard UX and drafts on failure. Persist only preferences
  in browsers. Treat uploaded HTML as data, never executable page content.
- Test only isolated data/ports. Preserve production settings, exact rollback
  containers and backups. Before legacy rollback, restore proxy authentication
  and drain old auth-off workers; never expose an unguarded old application.

Run vet, formatting, `go test -race ./...`, golangci-lint, and
`node --test scripts/clipboard.test.mjs scripts/accounts.test.mjs`.
Linux CI/image export: `.github/workflows/share-preview.yml`.
References in `Documents/`: `ACCOUNTS.md` (model/API), `ARC-SHARING.md` (protocol,
storage), `INTEGRATION-TESTS.md` (isolated tests), `OPERATIONS.zh-CN.md` (manual
maintenance, deployment, backup and rollback).
