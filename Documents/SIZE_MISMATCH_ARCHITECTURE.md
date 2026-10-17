# Content integrity and destructive reads

This document owns integrity checks. The bundle format and delivery contract are
specified in [ARC-SHARING.md](ARC-SHARING.md); composition is in
[Architecture](ARCHITECTURE.md).

## Ordinary legacy reads

`handlers/retrieval/handler.go` checks `PasteStore.StatContent` at the `View` and
`Raw` entry points. The filesystem implementation uses file statistics; S3 uses
object metadata. When an existing object's size differs from paste metadata,
CLI/raw responses return HTTP 500 `size_mismatch`; browser views show an error.

The stat guard is an early size check, not a cryptographic integrity proof. Its
existence does not justify removing later checks on newly added read paths.
Ordinary legacy helpers retrieve/preview content after this routing check.

## One-time and multipart reads

Browser one-time HTML views display confirmation without loading content or
incrementing reads. Destructive legacy raw/CLI paths use
`internal/services/paste_service.go:ConsumePaste`. That service acquires a bounded
process-local lock, loads fresh metadata, completely reads and checks content
length, and deletes before returning the bytes to the handler.

Confirmed multipart receiving validates the stored manifest and full bundle
before consuming. `handlers/shares.go` and `models/share.go` distinguish logical
items, representations and archive entry paths. Invalid bundle/size data is
rejected instead of being presented as a successful receive.

Only one destructive reader can succeed in the supported single-process
deployment. This lock does not coordinate separate Lambda instances or multiple
filesystem processes. Deletion precedes network delivery; a truncated response
cannot be recovered from the server. Only a completely received browser copy can
be retried locally.

## Verification

Go service/retrieval/share tests cover size disagreement, corrupt data and
destructive-reader races. The legacy `scripts/integration/test_size_mismatch.sh`
deliberately damages records; it must use isolated data. The Python smoke helper
and browser suites verify complete bundles and retention after one-time receive.
Commands and cleanup boundaries are maintained in [tests](INTEGRATION-TESTS.md).
