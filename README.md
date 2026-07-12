# HHC Asset API

`asset-api` owns platform file mechanics: upload sessions, Blob object keys, completion validation, ClamAV malware scan state, grants, and stable downloads. Consumer services retain business ownership of CMS records, LINE context, or desktop sync metadata.

## Local development

1. Create a PostgreSQL database and copy `.env.example` to `.env`.
2. Export the variables; the binary intentionally does not load `.env` itself.
3. Run `go run ./cmd/server`.

Local uploads use a short-lived signed `PUT /dev/uploads/{token}` target and store bytes under `.data/assets`. Production uses Azure Blob Storage with `DefaultAzureCredential` and a single-blob user-delegation SAS. Account keys are not supported.

## Routes

- `GET /health`
- `GET /ready`
- `GET /api/assets/public/{assetId}`
- `POST /priv/assets/upload-sessions`
- `POST /priv/assets/{assetId}/complete`
- `POST /priv/assets/{assetId}/grants`
- `DELETE /priv/assets/{assetId}/grants/{grantId}`
- `GET /priv/assets/{assetId}/public-url`

Private routes require a trusted `X-Internal-Caller-App-Id` supplied by the internal invocation layer. Public ingress must strip all client-provided `X-Internal-*` headers.

## Scan lifecycle

After upload completion, a database-backed worker claims the pending asset and streams its private Blob directly to `clamd` with the `INSTREAM` protocol. Clean results enable the existing grant checks; infected, pending, and failed assets remain unavailable. Transient Blob or ClamAV failures use bounded exponential backoff and become `failed` after `CLAMAV_MAX_RETRIES`.

`CLAMAV_HOST` must resolve to a private endpoint reachable by every asset-api replica. Port `3310` must not be exposed publicly. Keep clamd's `StreamMaxLength` at least as large as `CLAMAV_MAX_FILE_SIZE_BYTES`, and keep both limits at or above the largest upload accepted by asset-api.
