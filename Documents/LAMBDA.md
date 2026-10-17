# Standalone Lambda / S3 deployment

This mode is selected by `AWS_LAMBDA_FUNCTION_NAME` and uses S3 for paste content
and metadata. Set `NCLIP_ACCOUNTS=false`: account mode requires a single persistent
filesystem server and intentionally fails in Lambda. Process-local destructive
reader locks do not provide exclusion across Lambda instances.

## Package and infrastructure

Use an AWS account, a private S3 bucket, an execution role and an HTTP API or
Function URL. Grant the execution role only the required bucket/prefix object
read/write/delete/list operations and logging access. Do not enable anonymous
bucket reads to bypass application access rules.

Build this fork, including runtime assets, from the repository root:

```bash
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o bootstrap .
zip -r lambda-function.zip bootstrap static
```

Use runtime `provided.al2023` and an architecture matching the build (`amd64`
maps to Lambda `x86_64`; `arm64` maps to `arm64`). Create/configure the function
using your own private infrastructure settings, then upload the package. General
application variables and defaults are maintained in the root
[README](../README.md#configuration).

## Repository deployment workflow

`.github/workflows/deploy-lambda.yml` runs manually or on `deploy/lambda` pushes.
It builds the Go executable and `static/`, validates the architecture, assumes an
AWS role using OIDC and updates the configured function. Configure the AWS trust
policy to restrict repository/ref access before enabling deployment.

| Private Actions setting | Use |
|---|---|
| Secret `AWS_REGION` | Target region |
| Secret `AWS_ROLE_TO_ASSUME` | OIDC deployment role |
| Secret `AWS_AUDIENCE` | Configured OIDC audience |
| Secret `LAMBDA_EXECUTION_ROLE` | Function execution role |
| Secret `NCLIP_API_KEYS` | Optional standalone upload keys |
| Variable `LAMBDA_FUNCTION_NAME` | Default function name; manual input may override |
| Variables `S3_BUCKET`, `S3_PREFIX` | Storage location |
| Variables `NCLIP_BUFFER_SIZE`, `NCLIP_UPLOAD_AUTH`, `GIN_MODE` | Function settings |

These are setting **names**, not values to publish. The workflow's deployment
role needs the relevant Lambda/S3/IAM access for your infrastructure. Function
URLs/API authorization, bucket policies, DNS and certificates are separate
configuration; the workflow does not create a complete secured public site.

## Limits and verification

Lambda's buffered synchronous invocation has a 6 MB request and response quota;
event envelopes, base64 and multipart overhead reduce usable content size.
Streaming response quotas do not make this buffered adapter a streaming service.
Choose a smaller application limit after testing the entire integration path.
See [AWS quotas](https://docs.aws.amazon.com/lambda/latest/dg/gettingstarted-limits.html).

Optional global upload keys protect uploads/deletion, not reads. Gate access at
the infrastructure layer if required, or choose the single-server account mode.
Do not cache private content or one-time responses at a CDN. Respect bucket
privacy and explicitly configured lifecycles; no distributed exactly-once receive
guarantee is provided.

Check CloudWatch errors, IAM object access, function health and disposable
upload/raw/receive/delete paths. A browser clipboard also needs a secure context
and browser support. Local filesystem integration tests do not prove AWS/S3
deployment compatibility. Keep a verified function version/configuration and
backups for rollback; actual ARNs, secrets and deployment logs remain private.
