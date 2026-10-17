---
description: 'Go conventions for this repository'
applyTo: '**/*.go,**/go.mod,**/go.sum'
---

# Go editing

Read [AGENTS.md](../../AGENTS.md) for system/security boundaries. Preserve the
existing `main.go`, `handlers/`, `internal/`, `models/` and `storage/` organization;
do not relocate the entry point merely to follow a generic project layout.

- Prefer clear idiomatic Go, early errors and small interfaces. Use gofmt;
  document exported symbols, invariants and non-obvious compatibility decisions.
- Handle errors, close resources, bound untrusted reads and validate input.
  Do not silently truncate oversized bodies or execute uploaded HTML.
- Keep storage behind existing abstractions. Maintain single-process consumption,
  account revision checks, last-manager protection and legacy route authorization.
- Use standard cryptography and the existing password/session store. Never log
  raw credentials, account databases, full environment or full configuration.
- Preserve original content bytes and filenames according to the share protocol;
  keep browser success, network delivery and clipboard success distinct.
- Test meaningful behavior with isolated data. Required checks and entry points
  live in [INTEGRATION-TESTS.md](../../Documents/INTEGRATION-TESTS.md).

References: [Effective Go](https://go.dev/doc/effective_go) and
[Go Code Review Comments](https://go.dev/wiki/CodeReviewComments).
