# HHC Asset API

`asset-api` owns platform file mechanics: upload sessions, Blob object keys, completion validation, Defender malware scan state, grants, and stable downloads. Consumer services retain business ownership of CMS records, LINE context, or desktop sync metadata.

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

Azure Defender for Storage publishes malware scan results to Event Grid, which forwards them to the configured Service Bus queue. The worker accepts each Event Grid event once, persists its event id, and keeps every non-clean object unavailable. Production readiness requires both Service Bus settings; local development can omit them and exercise scan transitions in tests.
