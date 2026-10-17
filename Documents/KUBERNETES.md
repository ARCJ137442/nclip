# Kubernetes example deployment

`k8s/` is a reusable starting point, not a maintainer's live cluster configuration.
Use your own namespace, registry, storage class, ingress controller, TLS secret
and private settings. All domains in examples are reserved placeholders.

## Inspect before applying

| Manifest | Purpose |
|---|---|
| `namespace.yaml` | `nclip` namespace |
| `pvc.yaml` | `nclip-pvc` persistent claim |
| `deployment.yaml` | One UID/GID-1001 application writer, persistent `/data` |
| `service.yaml` | Cluster-local `nclip` service, port 80 to container 8080 |
| `ingress.yaml` | Example host routing to that service |
| `kustomization.yaml` | Resource list for rendering/adaptation |

Build this fork and push a fixed image to your chosen registry. Replace the
deployment's image before applying; its local placeholder is not pullable by a
remote cluster. Set `NCLIP_URL` to your HTTPS origin and review all limits using
the root [configuration table](../README.md#configuration). Configure ingress TLS
with a secret and controller appropriate to the cluster.

Render and review with `kubectl kustomize k8s/`; validate against your cluster with
server-side dry-run before applying. No real cluster or storage provisioner is
implicitly created by these files.

## Account mode and storage

Account mode needs a writable persistent directory and a read-only mounted
bootstrap password secret. Configure `NCLIP_ACCOUNTS`, `NCLIP_ADMIN_USERNAME` and
`NCLIP_ADMIN_PASSWORD_FILE`. Do not place the secret value in a tracked manifest;
provision it outside Git. Initialization does not reset an existing database.

Use one replica and `Recreate` to avoid overlapping application writers during
updates. ReadWriteOnce alone does not prove process exclusivity. Configure volume
ownership so UID 1001 can write data and read the secret. Persistent backups must
include hidden `.accounts` state, not merely content files.

Do not add an HPA or multiple writers while claiming single-instance one-time
semantics. Distributed accounts and S3 one-time exclusion are not implemented.
See [Accounts](ACCOUNTS.md) and [Sharing](ARC-SHARING.md).

## Verify and maintain

After replacing placeholders and applying the reviewed manifests, inspect rollout,
pod logs, PVC binding and readiness. `kubectl -n nclip port-forward svc/nclip
18080:80` gives a local test path. Health alone is insufficient: verify login,
permissions, normal old links and disposable one-time/private/public shares.

For failures, inspect events, image pull access, storage ownership, secret mounts
and ingress service/port/TLS mappings. Keep a compatible old image and verified
backup before upgrades. Never delete a PVC as an ordinary rollback step. Generic
release/restore rules live in [人工维护](OPERATIONS.zh-CN.md).
