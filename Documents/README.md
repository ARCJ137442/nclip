# Documentation index

The root [README](../README.md) owns quick start and the configuration defaults.
Each document below owns one topic; link to it instead of copying its tables.
Examples describe the software, never a maintainer's live deployment.

| Reader / question | Canonical document |
|---|---|
| How is the system organized? | [Architecture](ARCHITECTURE.md) |
| How do account login and permissions work? | [Accounts and account API](ACCOUNTS.md) |
| What is the multipart sharing/storage protocol? | [Sharing protocol](ARC-SHARING.md) |
| How do curl, PowerShell and other clients use legacy APIs? | [Clients](CLIENTS.md) |
| What do the custom headers mean? | [Headers](X-HEADERS.md) |
| How do I verify a change safely? | [Integration/browser tests](INTEGRATION-TESTS.md) |
| How do I maintain builds, backups and rollbacks manually? | [人工维护](OPERATIONS.zh-CN.md) |
| How do I adapt the Kubernetes examples? | [Kubernetes](KUBERNETES.md) |
| What differs in standalone Lambda/S3? | [Lambda](LAMBDA.md) |
| Which registry images can the cleanup workflow remove? | [Container cleanup](CONTAINER_CLEANUP.md) |
| How are corrupt sizes and destructive reads checked? | [Content integrity](SIZE_MISMATCH_ARCHITECTURE.md) |
| What may be published? | [Publication and security boundaries](../SECURITY.md) |
| What ships in the current fork release? | [Release notes and downloads](RELEASE-NOTES.md) |

Source conventions live in [AGENTS.md](../AGENTS.md). The scoped Copilot instruction
files under `.github/instructions/` describe editing practices, not an alternate
API, installation guide or deployment inventory.

Deployment credentials, actual hosts/ports, local paths, inspect dumps, runtime
image IDs, backups, screenshots with personal information and acceptance evidence
belong in private records outside this repository. Do not add them to PR bodies,
issue comments, Actions logs or downloadable artifacts.
