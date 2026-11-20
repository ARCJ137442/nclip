# nclip v0.12.0-arc.1

This ARC fork release adds native account login and independent upload and
download permissions for ordinary users. Management is reserved to the unique,
immutable administrator, who can maintain users and all shares. Users land on a
suitable sending, receiving or management page. Private shares require login and
download permission, with explicit public sharing available per message.

The unified composer supports text, rich clipboard representations, screenshots
and multiple files. Sending creates one link and reports upload and clipboard
success separately. One-time browser links wait for confirmation before consuming
content. Legacy raw/CLI interfaces and stored records remain compatible.

Public documentation is indexed and checked in CI. Private deployment records
are kept outside source and artifacts. Diagnostic environment/configuration dumps
have been removed, and Kubernetes examples use consistent resource references and
a single-writer update strategy.

## Downloads

- `nclip_linux_amd64.tar.gz`, `nclip_linux_arm64.tar.gz`: executable and required
  `static/` assets. Run from the extracted package directory.
- `nclip_image_linux_amd64.tar.gz`: offline Docker image, loaded using
  `docker load -i nclip_image_linux_amd64.tar.gz`; image tag `nclip:v0.12.0-arc.1`.
- `SHA256SUMS`: checksums; verify with `sha256sum -c SHA256SUMS` after downloading
  all listed files. `BUILD-INFO.txt` records the version and exact source commit.

This release does not automatically deploy or publish a registry image. Account
mode needs a persistent single-server data directory and a private bootstrap
password file. Back up the whole data directory, including hidden account state.
See [documentation](README.md) and [manual maintenance](OPERATIONS.zh-CN.md).

Actual password-manager extension and mobile Safari behavior depend on the
browser. Only completely received one-time content can be retried in page memory;
a transfer interrupted after server deletion may be unrecoverable.
