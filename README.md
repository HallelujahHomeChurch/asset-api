# HHC Asset API

`asset-api` owns platform file mechanics: upload sessions, Blob object keys, completion validation, ClamAV malware scan state, grants, and stable downloads. Consumer services retain business ownership of CMS records, LINE context, or desktop sync metadata.

## Local development

1. Create a PostgreSQL database and copy `.env.example` to `.env`.
2. Export the variables; the binary intentionally does not load `.env` itself.
3. Run `go run ./cmd/server`.

Local uploads use a short-lived signed `PUT /dev/uploads/{token}` target and store bytes under `.data/assets`. Production uses Azure Blob Storage with `DefaultAzureCredential` and a single-blob user-delegation SAS. Account keys are not supported.

Set `ASSET_ALLOW_DEV_CALLER_HEADER=true` only for local development without
Dapr. Production must leave it disabled, invoke the service through Dapr, and
deploy a default-deny Dapr ACL.

## Routes

- `GET /health`
- `GET /ready`
- `GET /api/assets/public/{assetId}`
- `GET /api/assets/public/{assetId}/{small|medium|large}`
- `POST /priv/assets/upload-sessions`
- `GET /priv/assets/operations`
- `GET /priv/assets/{assetId}`
- `POST /priv/assets/{assetId}/complete`
- `POST /priv/assets/{assetId}/scan/requeue`
- `POST /priv/assets/{assetId}/grants`
- `DELETE /priv/assets/{assetId}/grants/{grantId}`
- `GET /priv/assets/{assetId}/public-url`
- `DELETE /priv/assets/{assetId}`

Private routes derive caller identity from Dapr's authenticated
`Dapr-Caller-App-Id`. The custom `X-Internal-Caller-App-Id` fallback is accepted
only when the development setting is enabled. Public ingress must strip all
client-provided internal identity headers.

## Scan lifecycle

After upload completion, a database-backed worker claims the pending asset and streams its private Blob directly to `clamd` with the `INSTREAM` protocol. Clean results enable the existing grant checks; infected, pending, and failed assets remain unavailable. Transient Blob or ClamAV failures use bounded exponential backoff and become `failed` after `CLAMAV_MAX_RETRIES`.

Clean image uploads are processed into stable 480, 960, and 1440 pixel JPEG variants. Variants inherit the original asset grant and cannot be downloaded before scanning and processing complete. Upload-session idempotency keys replay the original asset/session instead of creating duplicate objects.

`CLAMAV_HOST` must resolve to a private endpoint reachable by every asset-api replica. Port `3310` must not be exposed publicly. Keep clamd's `StreamMaxLength` at least as large as `CLAMAV_MAX_FILE_SIZE_BYTES`, and keep both limits at or above the largest upload accepted by asset-api.

Failed scans can be requeued by the owning service. Infected scans cannot be
requeued. `GET /priv/assets/operations` exposes scan backlog and purge backlog
for operational checks.

## Deletion

Owner deletion is a soft-delete command and immediately denies download. A
PostgreSQL-leased worker removes expired staging uploads, deleted assets,
derivatives, and retained terminal scan failures. Blob deletion is idempotent
and retried independently of the database transaction.

## Azure deployment

`infra/main.bicep` provisions the internal Dapr-enabled Container App and Blob
RBAC. The GitHub Actions release workflow tests, builds an immutable ACR tag,
and updates only the existing `asset-api` Container App. Run the one-time
infrastructure deployment in `infra/README.md` before pushing `main`; the
workflow intentionally does not create databases or inject first-deployment
secrets.
