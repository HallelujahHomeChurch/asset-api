# HHC Asset API

`asset-api` owns platform file mechanics: upload sessions, Blob object keys, completion validation, ClamAV malware scan state, grants, and stable downloads. Consumer services retain business ownership of CMS records, LINE context, or desktop sync metadata.

## Local development

1. Create a PostgreSQL database and copy `.env.example` to `.env`.
2. Export the variables; the binary intentionally does not load `.env` itself.
3. Run `go run ./cmd/migrate`, then `go run ./cmd/server`.

Local uploads use a short-lived signed `PUT /dev/uploads/{token}` target and store bytes under `.data/assets`. Production uses Azure Blob Storage with `DefaultAzureCredential` and a single-blob user-delegation SAS. Account keys are not supported.

Set `ASSET_ALLOW_DEV_CALLER_HEADER=true` only for local development without
Dapr. Production leaves it disabled. Container Apps invoke through Dapr with
the matching `APP_API_TOKEN`; the LINE attachment Job uses a dedicated managed
identity against internal ingress. ACA authentication validates the token before
Asset also checks tenant, issuer, audience, client, object id, and `Asset.Invoke`.

## Routes

- `GET /health`
- `GET /ready`
- `GET /api/assets/public/{assetId}`
- `GET /api/assets/public/{assetId}/{small|medium|large}`

The gateway exposes clean public assets canonically as `/assets/{assetId}` and
`/assets/{assetId}/{small|medium|large}`. The `/api/assets/public/*` routes are
the service contract and legacy browser compatibility path.
- `POST /priv/assets/upload-sessions`
- `GET /priv/assets/operations`
- `GET /priv/assets/{assetId}`
- `GET /priv/assets/{assetId}/download`
- `POST /priv/assets/{assetId}/complete`
- `POST /priv/assets/{assetId}/scan/requeue`
- `POST /priv/assets/{assetId}/grants`
- `DELETE /priv/assets/{assetId}/grants/{grantId}`
- `GET /priv/assets/{assetId}/public-url`
- `DELETE /priv/assets/{assetId}`

Private routes first authenticate Dapr's app-channel token, then derive caller
identity from `Dapr-Caller-App-Id`. Restricted downloads additionally require
`X-Asset-Subject-Type` and `X-Asset-Subject-Id` to match an active grant. The
custom caller fallback is accepted only when the development setting is enabled.
The dedicated `hhc-web-bulletin-worker` is delegated to its owning `hhc-web-api`
service for these private asset operations.

## HLS package producer contract

`DELETE /priv/recordings/{recordingID}` is an idempotent owner command available
only to `hhc-web-api` with HLS enabled. A durable recording tombstone rejects
late source/package creation, expires all associated media and fences processing
leases. Existing cleanup jobs remove temporary Blob and R2 HLS/preview objects
with retries; package finals retain the existing one-hour grant safety window.
No public asset deletion permission or extra container is introduced.

`/priv/recording-packages` provides create, paginated status, batched single-object
PUT signing and durable completion handlers. These routes fail closed with 503
unless `ASSET_RECORDING_HLS_ENABLED=true` is set for the API and recording Job.
Upload status uses a strongly consistent, package-scoped R2 object listing
instead of sequential HEAD requests per fragment, so long recordings do not
exhaust the CMS control-request deadline. Listings are bounded to 10,000 objects;
only declared inventory paths with matching remote sizes are confirmed. Provider
errors fail closed. Status paging and owner checks are unchanged, and size
confirmation is not SHA-256/media validation or evidence of `ready`.
The flag defaults off and must remain off until the reviewed HLS cutover.
Completion means `freezing`, not ready. In HLS mode the recording Job uses two
global DB slots, renewable fenced leases and bounded per-fragment media validation;
it does not re-encode CLI packages. Ready retention is thirty days, with a one-hour
existing-grant cleanup grace. Staging and failed attempt deletion are retried and
reconciled daily to sweep late writes; provider acceptance remains required.
Single-file recording upload and playback routes are retired; clients must use
HLS packages or browser source sessions. The empty legacy table is retained only
for release rollback and is not used by this runtime. The ordinary Blob scan
lifecycle is unchanged. The Pages function accepts package-scoped
HLS grants and authorizes every playlist/init/segment, including internal cache
hits. Browser source ingest/encode is independently gated; CMS/player integration
and provider acceptance remain subsequent work. Enabling these producers is not
end-to-end readiness.

Optional seek previews are generated from ready immutable HLS by the same
recording Job, behind waiting validation/source work and sharing its two global
slots. Migration 031 also queues existing unexpired ready packages. No CLI,
upload inventory, CMS publication, ready state, or source/scanning change is
required. The existing authenticated playback base serves `previews/index.vtt`
and `previews/seg-NNNNNN.jpg`; missing previews return 404 while video plays.
See [the preview contract and acceptance checklist](docs/member-video-operations.md#seek-preview-contract).

Browser source storage primitives use separate `recording-sources/{id}/staging`
Blob keys: 16 MiB blocks, up to 2,981 blocks for the independent 50 GB source
budget, exact block IDs/tail sizes and write-only blob-scoped user-delegation
SAS capped at fifteen minutes. Commit checks the returned ETag and size, without
reading/copying/hashing the whole source in the API request. These primitives
back private `/priv/recording-sources` create/status/sign/complete handlers.
Completion commits only validated block metadata and persists one ETag-pinned
`finalizing` receipt; retries never extend its seven-day retention window.
PostgreSQL enforces one active source per actor. Status pages support all 2,981
blocks without a 1,000-block truncation. Source handlers and processing remain
disabled unless `ASSET_RECORDING_SOURCE_ENABLED=true` and HLS is enabled. Set
`ASSET_RECORDING_SOURCE_ACCOUNT_URL` and `ASSET_RECORDING_SOURCE_CONTAINER` to
an explicitly separate private Blob container; no account key is used. Apply
reviewed managed-identity permissions, browser CORS, lifecycle safeguards and
Job resources before activating either runtime. Defaults do not change.
They do not create normal asset records or invoke the existing malware scanner.
A write SAS can still overwrite its staging blob; immutable ETag-fenced
finalization and server-side SHA-256 validation run before encode. The Job pins
all subsequent range reads to that verified ETag, never downloads the full input
to local disk, and encodes each rendition directly from the original. Native
FFmpeg HTTP PUT backpressure spools one bounded fragment at a time into R2.
Control files use measured bitrates and initialization codecs; the existing
immutable-copy/hash/full-decode validator must pass before an atomic ready commit.

Source processing and CLI package validation share two global DB slots and a
5.5-hour processing deadline, with one long claim per Job execution. Source
attempts are limited to three, including crashes; `retry-processing` reuses the
verified input and never extends the original seven-day deadline. Cleanup runs
before processing: ready sources and expired source sessions are removed from
Blob, failed output attempts are reclaimed after a six-hour stale-worker grace,
and daily repeated sweeps catch late writes. Ready HLS retention remains owned
by the package lifecycle. Metadata receipts are retained; their legal retention
policy is not inferred from byte cleanup. Full 20–50 GB runtime/cost acceptance
and production resource activation are still separate delivery gates.

## Scan lifecycle

After upload completion, the runtime dispatches a durable queue message. The
queue-scaled `asset-scan-worker` Container App downloads the immutable private
Blob and scans it with a local ClamAV process and a validated signature
snapshot. It scales from zero on scan work or a short-lived warm pulse, drains
the durable scan queue until empty, then returns to zero. Clean
results enable the existing grant checks; infected, pending, and failed assets
remain unavailable. Transient failures use bounded retries before becoming
`failed`.

Clean image uploads enqueue a versioned derivative event. The event-triggered
Azure derivative Job processes one immutable asset version into stable 480,
960, and 1440 pixel JPEG variants. Variants inherit the original asset grant
and cannot be downloaded before scanning and processing complete. Queue
delivery is at-least-once; the asset ID and Blob ETag make replay and stale
events safe. Upload-session idempotency keys replay the original asset/session
instead of creating duplicate objects.

Failed scans can be requeued by the owning service. Infected scans cannot be
requeued. `GET /priv/assets/operations` exposes scan backlog and purge backlog
for operational checks.

## Deletion

Owner deletion is a soft-delete command and immediately denies download. A
PostgreSQL-leased worker removes expired staging uploads, deleted assets,
derivatives, and retained terminal scan failures. Blob deletion is idempotent
and retried independently of the database transaction.

## Azure deployment

`infra/main.bicep` provisions the internal Dapr-enabled Container App, migration
job, queue-scaled scan worker, derivative event Job, isolated Key Vault access, and scoped
storage RBAC. The manual GitHub Actions release workflow runs DB-backed tests,
builds an immutable image, applies migrations, and then replaces the runtime
and workers. The legacy `asset-scan` Job remains Manual for one compatibility
release and has no queue trigger. A failed release rolls the API back; the schema-compatible derivative
Job stays on the new immutable image so it can drain events already committed
by that revision. Complete the reviewed one-time cutover in `infra/README.md`
first.

## Application logging

`LOG_LEVEL=debug|info|warn|error` defaults to `info`; invalid values stop
startup. This controls application slog and default standard-log output.
Persisted audit records remain independent of this setting.
