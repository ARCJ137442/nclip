---
applyTo: '.github/workflows/*.yml'
description: 'Repository-specific CI, artifact and publication boundaries'
---

# Workflow editing

- Read root `AGENTS.md` and the affected workflow before changing it. Use explicit
  minimal permissions, trusted versioned actions and secrets/OIDC instead of
  literal credentials. Do not interpolate untrusted event text into shell code.
- Use the Go version supported by `go.mod`. Frontend tests use Node; the shipped
  browser application has no Node runtime dependency or frontend bundler.
- `share-preview.yml` verifies and exports a fixed-commit archive/checksum. It
  does not deploy or publish registry packages. Keep artifact inputs explicit;
  never upload a whole workspace, real data, private docs or unredacted logs.
- `test.yml`, `integration-test.yml` and `gosec.yml` validate code/security.
  Integration fixtures must use isolated storage and clean only owned data.
- `release.yml` prepares versioned offline assets and checksums in a draft
  Release. It never deploys. The legacy `container.yml` registry publisher is
  restricted to upstream ownership. `deploy-lambda.yml` performs real AWS
  deployment; ordinary preview builds must not invoke it.
- `cleanup-container-images.yml` defaults to dry-run; preserve the documented
  retention semantics and do not imply it cleans server disks or data volumes.
- Configuration defaults belong to README; test commands belong to
  `Documents/INTEGRATION-TESTS.md`; manual release operations belong to
  `Documents/OPERATIONS.zh-CN.md`. Update the owner document when behavior changes.
- Review names, artifact contents, PR descriptions and logs for deployment data
  before publication. Secret masking does not hide arbitrary domains or paths.
