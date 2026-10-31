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
The isolated bulletin extractor can reuse `hhc-web-api-identity` through the
optional exact `ASSET_EXTRACTOR_WORKLOAD_CLIENT_ID` / `OBJECT_ID` pair. It maps
to the existing `hhc-web-api` owner, not a new asset owner. Both identifiers
must be configured together; leaving both empty preserves existing behavior.

## CI test scheduling

`python3 scripts/test-go-lanes.py` runs all packages discovered by `go list ./...`
with `-race -count=1 -p=1` in two processes on the same runner. Media packages
(recording validation, recording processing and derivatives) execute serially in
one lane; all remaining and newly added packages execute serially in the other.
This overlaps database waits with media work without adding another runner. Media
worker and database integration fixtures use distinct per-test schemas.

The runner publishes `.artifacts/data-governance/test-events.jsonl` only after
both lanes succeed and every package has a terminal result. Failure or cancellation
stops both process groups. `python3 scripts/test-go-lanes_test.py` checks package
coverage, event integrity, failure and cancellation. The direct serial command
`go test -race -json ./... -count=1 -p=1` remains useful for comparison; both paths
require the same disposable PostgreSQL and media tools. Governance export still
runs after the complete event stream is available.

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

Capture confirmation performs storage LIST outside its database transaction.
A short preflight preserves accepted receipt replay without accessing storage;
the final transaction rechecks ownership, expiry, state, epoch, deletion and
object declarations before applying evidence. Only newly confirmed media advances
the activity watermark. This removes storage latency from the capture lock and
pool occupancy, but does not eliminate database contention or change idle cutoff.
New broadcast capture creation can opt in with
`recoveryVersion: "common-watermark-v1"`; omission preserves legacy behavior.
An operation cannot change its version on replay. The first successful live
range transaction arms a persisted wall/media baseline using decoded frame rate.
Only contiguous confirmation across all three renditions advances recovery.
After 30 seconds without bounded fresh progress, signal state is interrupted;
at the persisted five-minute deadline, confirmation/close serializes an immutable
cutoff before accepting late supplementation. Old backlog does not receive a
fresh five minutes merely because one more segment arrived. This is bounded
arrival evidence, not proof of a running encoder; pre-activation unknown backlog
cannot be fully distinguished with the existing protocol.

The additive migration leaves existing captures on their legacy clock. Repeated
range writes and process/replica restarts do not rearm. Manual end, revocation,
deletion and terminal state disable recovery. Signal capability and common
watermark fields are optional; consumers must treat missing capability as legacy,
including after server rollback. Neither this policy nor public cutoff controls
native OBS recording; a separately negotiated client policy owns local behavior.

Human CMS administrators can preview and confirm one global retention policy
(1–365 days) through `/priv/recordings/retention-policy`. Active policy uses
immutable accepted upload completion, including browser sources before encoding.
Preview estimates include unfinished accepted uploads without double-counting
materialized sources. Revision, actor-bound five-minute preview, idempotency and
atomic audit protect updates. Migration 034 initially leaves legacy expiry
unchanged; production activation requires a separately approved impact preview.
Terminal cleanup never revives. The owner-only `/priv/recordings/lifecycle`
snapshot lets CMS apply the authoritative expiry before list filtering.

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
exhaust the CMS control-request deadline. Listings are bounded to 13,000 objects;
only declared inventory paths with matching remote sizes are confirmed. Provider
errors fail closed. Status paging and owner checks are unchanged, and size
confirmation is not SHA-256/media validation or evidence of `ready`.
The flag defaults off and must remain off until the reviewed HLS cutover.
Package validation renews its fenced lease before persisting display progress.
A progress-write deadline after successful renewal is logged and retried on the
next heartbeat; lease loss and terminal progress/ready commits still fail closed.
`recording_package_stage_totals` reports cumulative worker milliseconds for
immutable freeze/hash, metadata probe and full decode. Parallel worker times
can overlap and must not be summed as wall-clock latency.

Completion means `freezing`, not ready. In HLS mode the recording Job uses two
global DB slots, renewable fenced leases and bounded per-fragment media validation;
it does not re-encode CLI packages. Retention defaults to thirty days, with a one-hour
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

New timeline preview sprites sample each immutable fragment every ten seconds.
The existing 960×90 JPEG layout and five-second VTT cues remain compatible:
adjacent cues reuse the same sample. Existing live receipts and published sprites
are reused without regeneration; one JPEG is still written per media fragment,
so fewer sampled frames does not imply half as many JPEG objects.
An immutable-image conflict retries that batch with the legacy sampling cadence
to finish any pre-upgrade JPEG left without its receipt; it never overwrites it.

When older ffprobe rejects AAC timing, validation retries against the same
fragment's bounded `trex`/`tfhd`/`trun` sample durations, matched by track ID,
time base and exact packet count. This preserves declared timing across source
audio gaps without widening continuity tolerances. Codec, CFR, IDR and complete
decode checks remain required. Recovered uploads retain their first accepted
completion timestamp so recovery cannot extend an active retention deadline.

Live intake validates the next three closed rendition fragments with at most
two workers inside the existing processing claim. Scratch capacity is checked
for both workers before intake, each decoder retains its two-thread limit, and
failure cancels and joins siblings before cleanup. Immutable copy, exact size,
SHA-256, full decode, approved CFR, cross-rendition continuity and fenced common
publication remain mandatory. This changes no producer API or C1 contract.
`recording_live_rendition` logs immutable freeze/hash and probe/decode elapsed
milliseconds; `recording_live_stage` separates validation, stable publication,
playlist writes and pointer advance. Claim IDs correlate these records; raw
errors, URLs, object keys, titles and credentials are never logged.

Run `go test ./internal/recordingvalidation -run '^$' -bench '^BenchmarkLiveSegmentValidation$' -benchtime=3x -count=1`
for the 30.03-second, 29.97 fps three-rendition moving-media benchmark. On
macOS/Apple M4 with FFmpeg 8.1.2, serial versus two-worker validation averaged 1.891 versus
1.079 seconds per batch; with 50 ms latency on each storage operation, 3.214
versus 1.748 seconds. These synthetic local measurements exclude the Job
schedule, database and stable publication; they do not prove production latency.
Windows acceptance must still measure a five-minute network outage, confirmed
upload backlog reaching zero, recovery to normal 60–120-second live latency
within ten minutes, and eventual stop/seal publication.

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

## Progressive capture intake

Private `/priv/recording-captures` routes share the existing HLS runtime gate and
accept only the owning `hhc-web-api` caller with a human uploader UUID. Create,
declare, confirm, seal and abort use body `operationKey` receipts. GET pages
persisted per-object states; a lost PUT/confirm response can be recovered by
replaying confirm or querying status. Only declared paths can be signed, for at
most fifteen minutes and never beyond the original 24-hour capture deadline.

Confirm checks bounded R2 listing size evidence and durably queues declarations;
`queued` never authorizes playback or claims hash/media validity. Signing queued
objects is fenced. Existing signed PUTs can still overwrite staging, so only the
existing immutable-copy/hash/media validator can produce a ready package. CMS
must accept stop before forwarding seal; stop itself has no Asset mutation.
Seal requires normal end, the exact complete three-rendition inventory and every
object queued/verified. Missing objects leave intake open. Seal atomically creates
one `freezing` package using the capture ID and original expiry, and replays its
receipt. Abort fences processing claims. Recording deletion closes captures.
The recording Job repeatedly cleans unsealed terminal/expired staging keys and
expires unfinished sealed captures; ready VOD remains under normal retention.
Capture receipts allow 20,002 producer operations plus one reserved emergency
abort receipt (20,003 total), and are retained with recovery metadata.

The existing recording Job validates each closed 30-second three-rendition batch
from immutable hash-checked copies, then publishes revisioned EVENT playlists
and conditionally advances an R2 common waterline. Sequence zero remains in every
playlist; only a complete normal seal appends ENDLIST. Queued objects are never
playable. Retries resume durable publication without decoding historical media.
The existing two processing slots bound concurrency; live batches receive the
first bounded 50-second scheduling window of each Job execution.

Private progress reports the published revision. Live grants require three common
segments and expire within five minutes, capture expiry and the requested CMS
replay/retention deadline; exchange credentials expire within 60 seconds. The
Worker authorizes each request before cache and restricts segments to the current
waterline. CMS owns membership, registration and post-stop scope renewal.

Remote failed captures retain at least seven days. Background claim/cleanup work
persists authoritative failed/expired package state without a status poll; a
previously unobserved failure starts its recovery hold at first observation.
Protected capture packages are excluded before cleanup batching, so eligible
ordinary packages continue to be swept. Capture media cleanup waits
for capture expiry plus the existing worker's six-hour writer grace, grants and
leases; it never enumerates ready VOD final objects. Candidate copies and old
playlist revisions are discovered from durable metadata and removed in bounded
batches. This conservative grace temporarily retains duplicate bytes; production
cost and cadence measurements remain an integration gate.

Local synthetic HLS/DB/Worker tests are not provider or Windows OBS acceptance.
No production configuration is activated by this change.

## Live covers

CMS-only `/priv/recording-live-covers/{scope}` routes store immutable pre-live
custom uploads, shared references and immutable automatic pictures per capture.
`defaults` is the CMS settings scope; capture scopes require the exact recording
binding. Only normalized, digest-checked private JPEG output can be read.
Migration 038 adds separate jobs/references; existing ready-package constraints
remain in place. The recording worker reads only published verified 480p init
and first public fragment, examines at most five opening frames, and shares the same
two processing slots with a 30-second deadline. Optional cover failure never
rolls back the live pointer. Defaults and active captures keep independent
references; unselected images are reclaimed after 24 hours with fenced retries.
Promotion queues an immutable VOD cover: imported auto candidates preserve the
live JPEG as candidate one and use measured capture boundaries for candidates
two and three. The internal `live-auto` kind projects as the existing `auto`
contract. This adds no OBS/C1 inventory, digest or validation exception.

The additive CMS-only `POST /priv/recording-live-covers/{scope}/refresh-auto`
uses the same recording binding and response as `/auto`, with one immutable job
per five-minute server bucket. It pins the latest published public fragment,
rejects new refresh jobs after End, and rechecks public bounds before reading.
The original `/auto` remains idempotent. CMS keeps its previous selected image
until a refreshed job is ready; custom uploads and references are unchanged.
Optional covers yield to runnable validation and source jobs.

Migration 046 adds incremental live preview progress. Capture sessions expose
`previews/index.vtt` and `previews/seg-NNNNNN.jpg` through the existing media grant.
The same five-second sprites are reused at VOD promotion; only missing images
and the final index are generated. See the preview contract for bounded work,
public range projection, retention, and failure behavior.

## Broadcast media authority

The private CMS capture-create request may supply `broadcastEpoch` to opt into
B1 media authority. The same transaction initializes a deny-all range at revision
1, with `memberState: blocked`, before capture bytes can receive member grants.
Omitting the field retains the existing C1 create, inventory, seal, and playback
behavior. B1 epoch recovery retains terminal, never-public prior epochs; it does
not replace or truncate raw C1 media.

CMS uses `/priv/recording-captures/{captureID}/broadcast-range` and
`broadcast-projection` to reconcile immutable `[startSequence,endSequenceExclusive)`
boundaries. A successful PUT has committed the DB policy and synchronously
published its conditional R2 authority object. The projection GET independently
checks the matching R2 policy and measures only the published raw timeline.
Future boundaries remain pending; the media Worker serves the verified
intersection as EVENT until the frozen end is verified. Emergency close sets
`revoked` while preserving the immutable boundaries, including a null end.

The optional private `memberState` field defaults to `blocked`. CMS sets `live`
for public capture playback, `blocked` for an unpublished archive or withdrawal,
and `vod` for a published archive. Package grants require `vod`; capture grants
and compatible capture URLs allow `live` or `vod`. Every media request reads
uncached current authority before any cache, HEAD, manifest, segment, or sprite
response, and checks the signed epoch and range revision. Failed authority
publication remains retryable and is never acknowledged as an applied policy.
Staff preview uses a separate purpose and `/captures/{captureID}/previews/{scope}`
path. Its private grant cannot be exchanged or played on member session paths.

Public HLS retains the original fMP4 segments and measured durations; no cloud
transcode is added. Normalized player time starts at zero and source bookmarks
use the projection's measured `mediaStartSeconds` origin. Auto covers select the
first public fragment; cropped preview cues and sprite checks exclude rehearsal
and post-service sequences. Raw inventory hashes, first sequence zero, and full
validation remain mandatory. Verification scheduling and processing concurrency
are unchanged.

Deploy producers and Worker authority support before enabling B1 CMS commands.
Migrations 039 and 040 retain historical epochs and admission policy. Once more
than one epoch exists for a recording, restoring the former global uniqueness
constraint requires data reconciliation; an automatic destructive down migration
is intentionally absent. Rolling back to a Worker without B1 authority checks is
unsafe while B1 media exists. Local decode, DB, and Worker tests do not establish
provider publication, deployment, or Windows OBS acceptance.

### Opt-in short-segment evaluation

`obs-hls-10s-v1` is a capture-pinned 300-frame / 10.01-second profile at
30000/1001 fps; it requires the CMS S10 opt-in supplement. Legacy captures and
CLI packages retain 900-frame validation. No existing capture changes profile.
The live and final validators count actual video packets as well as checking
CFR, continuity, IDR, SHA-256 and full decode. Normal S10 segments require 300
frames; the existing legal final-tail checks remain.

The benchmark above now reports both `frames_900` and `frames_300`. A bounded
local run on 2026-10-17 (Apple M4, three samples per case, no concurrent full
test suite) measured:

| Profile | Provider delay per operation | Three-rendition batch seconds | Media seconds / wall second |
| --- | ---: | ---: | ---: |
| 900 frames | 0 ms | 3.306 | 9.084 |
| 300 frames | 0 ms | 1.695 | 5.906 |
| 900 frames | 50 ms | 4.106 | 7.314 |
| 300 frames | 50 ms | 1.891 | 5.293 |

A smaller batch finishes sooner, but there are three times as many batches per
media hour. These samples show increased overhead per media second, not a
production price estimate. They exclude queue scheduling, API confirmation
listing, final publication, real NVENC output and viewer startup. An earlier
run competing with the full UI test suite is discarded for comparison. The
new profile still needs Windows/cloud A/B measurements before its latency and
cost acceptance can be claimed.

## Recording Event Jobs (candidate; production activation is separate)

`ASSET_RECORDING_EVENTS_ENABLED` defaults to false. With it off, the existing
scheduled recording worker and producer contracts retain their behavior. The
additive `recording_event_wakes` migration adds three operational dispatch rows;
it carries no media bytes or user identifiers. Existing durable work tables are
the outbox: producers never lock dispatch rows or emit one event per fragment.

When enabled, the API's leased dispatcher probes the **same readiness predicates**
as the existing media claims and sends at most two outstanding lane admissions.
Queue messages only wake the database-owned work. Repeated/stale messages cannot
start the same owner twice; Queue pop receipts and DB ownership are renewed during
long processing. The existing two global processing slots remain authoritative
across every lane. `maxExecutions` is not a global container concurrency guarantee.

The candidate worker modes are `live`, `processing`, `preview`, and `maintenance`;
`legacy` remains the default rollback mode and refuses to run when event mode is
enabled. Media Event Jobs use `minExecutions=0`, visible-message scaling, and one
execution target per lane. Live supports a target 2 CPU/4 GiB through
`RECORDING_LIVE_CPU=2`; its staged default remains 4 CPU/8 GiB until the reviewed
rollout selects the reduced tier. Processing is a candidate 2 CPU/4 GiB;
previews are a candidate 1 CPU/2 GiB. Each reduced tier can be reset
to 4 CPU/8 GiB independently of trigger mode. No validation or image-quality
parameter changes with the tier. The hourly maintenance candidate is 0.5 CPU/1 GiB,
bounded to five minutes, and invokes no FFmpeg processing.

Live owns capture thumbnails and live covers, including custom/default covers
before input starts. It drains eligible auxiliary work after each media batch
and before idle exit; existing SQL priority and the two global slots still gate
claims. Preview owns VOD covers and ready-package thumbnail finalization.
Stop/seal keeps capture thumbnails eligible through freezing/validating. Once
the package is ready, VOD waits for an admitted live preview lease to finish or
expire, then resumes durable progress and immutable per-segment receipts.
Stale live claims cannot publish. Ten-second image sampling does not set event
frequency. See [the handoff and resource verification](docs/reviews/recording-live-handoff-2026-10-25.md).

Normal retry/lease recovery is handled by the running API dispatcher and Queue
visibility, not hourly maintenance. Keep API min replicas >=1 while this dispatcher
is active. If no media is ready the API performs bounded readiness probes; it does
not start a heavy Job. Cloud execution starts, Queue requests and database load
still belong in cost accounting.

Worker cancellation covers initialization as well as claimed work. Recording
maintenance uses its existing five-minute application budget from before DB
setup; other recording modes have a 355-minute cooperative invocation budget
inside the unchanged 360-minute platform limit. Media attempts retain their
330-minute deadline. Queue receive/admission and completion/acknowledgement each
share a ten-second control budget; that context is never used for media work.
Timed-out unfinished work remains unacknowledged for the existing fenced retry.
Retention handles SIGINT/SIGTERM and uses a fourteen-minute cooperative budget
inside its existing fifteen-minute Job. Both commands defer DB close on return.
These bounds do not certify a global count of starting, idle or draining PG
pools. See [the lifetime evidence and remaining gates](docs/reviews/asset-worker-lifetime-2026-10-20.md).

Provision and activate using the reviewed Bicep/release path only. See
[the implementation plan](docs/superpowers/plans/2026-10-18-recording-event-jobs.md)
and [the candidate handoff](docs/reviews/recording-events-candidate-2026-10-18.md).

### Bounded API diagnostics

The HTTP server emits local `database/sql` pool statistics every 15 seconds
(`db_pool_summary`) and one fixed-schema `media_stage_summary` every 120 seconds
through the existing console logger. `LOG_LEVEL=warn/error` suppresses these
Info summaries; missing records must not be interpreted as zero errors.
No collector, provider query, per-request log or exported ID is added. Requests
only update an in-memory fixed-size aggregate; the logger runs separately.
Shutdown cancels the loop and waits at most one second for a blocked console
sink. Normal cancellation exits the loop; there is no final flush guarantee.

Stage `schema=1` encodes `rows_json` as a JSON matrix with these ordered rows (indices are stable):

| Index | Stage |
| --- | --- |
| 0 | Grant broadcast-epoch SQL |
| 1 | Grant remote authority read (outside transaction) |
| 2 | Capture BeginTx (pool acquisition plus transaction setup) |
| 3 | Capture owner advisory-lock SQL |
| 4 | Capture FOR UPDATE SQL |
| 5 | Capture callback, including grant policy/signing where applicable |
| 6 | Capture commit |
| 7 | Broadcast projection SQL |
| 8 | Broadcast projection remote authority read |
| 9 | Broadcast projection reuses prefetched authority (count, not cache latency) |
| 10 | Capture live-progress FOR UPDATE SQL |

Each row is `[count,error,cancel,sum_ms,max_ms,le10,le100,le1000]`.
Buckets are cumulative; cancellations/deadlines are also included in errors.
`count-le1000` counts durations above one second. Counts cover completed stages,
not all admitted operations or in-flight work; projection validation following
its reads can still fail. Capture stages are shared with non-grant callers and
callback time can overlap projection time. Do not sum nested durations or label
SQL elapsed as exact PostgreSQL lock wait. No request-level correlation is
claimed. Counters reset at each summary; pool wait counters are interval deltas
with a fresh baseline on process start. Use revision/replica log metadata for
attribution. DBStats wait means connection-pool wait, not row-lock wait.

Tests bound each stage line to 2,048 bytes and each pool line to 512 bytes.
At these intervals the incremental raw-log budget is at most 4,423,680 bytes/day
per API replica (35,389,440 at eight replicas), excluding platform envelopes,
indexing and existing logs. No detailed slow/error samples are emitted. Console
backpressure can delay summaries; actual intervals are logged, and absent data
is a telemetry gap. This does not change Application Insights caps, retention,
alert delivery or release configuration. Worker binaries do not start this loop.

### Recording authority transition evidence

After successful PostgreSQL commits, `recording_authority_committed` records only
first recovery activation, first idle cutoff, and explicit range revocation. A
replayed transition, callback rollback or failed commit emits none. These events
are independent of trace sampling, not durable audit. No capture/recording/user ID,
URL, credential, object path, SQL argument or error text is included.

`capture_ref` is SHA256 of `hhc.recording.capture.v1` + NUL + the canonical
32-lowercase-hex capture ID (no UUID hyphen conversion). It is stable, linkable
operational metadata, **not anonymous**; existing log access and retention apply.
Another service must deliberately use the identical algorithm to correlate it.
Validated version00 `traceparent` supplies trace/parent-span IDs even with sampled=0;
missing/invalid context logs null. This does not create a span or prove a complete
CMS/OBS trace. The header is correlation data, never authorization.

The existing diagnostics reporter drains at most16 events per15-second DB tick
from a fixed32-slot queue; no new goroutine, collector or request-time sink write.
Each text/JSON event is tested below1KiB (nominal ceiling64KiB/minute/replica from this
channel, usually three records per capture; ticker scheduling can bunch a pending tick). Existing two-minute cadence emits
`recording_authority_delivery` with written, queue_dropped, write_failed and
filtered_or_shutdown counters. Info-level filtering can suppress both events and
summary; `written` means the local handler accepted it, not collector ingestion.
A stalled sink cannot block request enqueues; it can delay all reporter output.
Existing bounded server shutdown remains, with no guaranteed queue flush.
Commit-to-enqueue crashes, saturation, shutdown, collector failure and ingestion
caps can lose evidence. Durable PostgreSQL authority remains the source of truth.

Migration advisory-lock acquisition and cleanup failures discard the physical
connection. Unlock uses a detached five-second context, reports cleanup errors
and completes before the CLI exits; application facts and published migrations
are unchanged. Local fault tests cover acquisition, unlock errors and a stalled
unlock, using only disposable PostgreSQL.

Active recording grants read a narrow capture projection and only extend the locked live read-grant deadline. They keep owner/revocation/deletion fences without loading producer receipt/object history or rewriting the capture row; terminal/package/recovery paths retain full lifecycle handling.

## Private bulletin illustrations

The CMS-only `GET /priv/assets/{assetID}/bulletin-illustration` requires the exact
`issueId` and `checksumSha256`. Every request checks the authenticated owner,
private `cms.weekly.image` namespace, source-illustration purpose/locale, deletion,
completed upload, clean scan, size and processing state before an ETag-bound Blob
read. It performs no grant writes and never permits PDF/general owner downloads.
Responses remain `private, no-store`. Release this additive route before the
coordinated CMS client; no public routing, media lifecycle or runtime activation
is changed here. Tracking: https://github.com/HallelujahHomeChurch/hhc-web-api/issues/339.

## Application probes and dependency monitoring

Readiness checks local application initialization only; it never pings PostgreSQL
or Dapr. A transient dependency outage therefore does not remove an otherwise
responsive API replica. Existing startup configuration/schema checks and all
request authorization, persistence and payment safety remain authoritative.
Readiness is not a database, media, payment or end-to-end acceptance test.

One process-owned monitor samples `db` and `dapr_outbound` every 30 seconds.
It emits `dependency_health` on initial observation, outcome transitions and
five-minute heartbeats, including healthy baseline/recovery. Each dependency has its
own two-second deadline; checks are sequential and cannot overlap. DB records
separate `acquire` from `ping` failures and include pool counts. Dapr uses only
loopback `/v1.0/healthz/outbound` when `DAPR_HTTP_PORT` is set, with
`DAPR_API_TOKEN` if present; redirects are rejected. Missing dependencies are
reported as `not_configured`, not healthy. Outcomes are bounded to `ok`,
`deadline`, `cancelled`, `unavailable`, and `not_configured`; raw errors, DSNs,
tokens, response bodies and request data are never logged.

The monitor is cancelled and joined on shutdown before closing the DB.
Keep `LOG_LEVEL=info` to retain healthy heartbeats; an absent heartbeat for an
expected active replica for 6 minutes means monitoring is missing/stale, not
healthy. Correlate platform replica lifecycle before alerting on a stopped
replica. No alert resource or production configuration is changed here.
For release acceptance, check these dependency events plus a representative
protected request; a green readiness probe alone no longer proves DB health.

These periodic checks can miss shorter outages and do not attribute a particular
request failure. Pool wait counts are lifetime totals, not per-request latency.

Dependency probes still sample every 30 seconds. `dependency_health` logs the
initial result, each outcome transition, and a five-minute heartbeat per dependency;
unchanged samples do not emit another event. Missing/stale heartbeats are unknown,
not healthy. Allow six minutes for heartbeat detection on expected active replicas.

HTTP diagnostic correlation accepts one bounded request ID and one valid W3C
version-00 traceparent (flags 00/01); malformed values are replaced (ID) or omitted (trace).
Response IDs and named-client diagnostic context agree. Caller audit headers
remain independently validated; generated diagnostic IDs never establish audit
actor provenance. Background audit dispatch does not reconstruct a request trace.

## Background resource release recovery

The owner release updates the scan App and existing scan, signature-refresh,
derivative, warmer and optionally retention Job definitions as well as the API.
Before migrations or runtime deployment, `worker-release-rollback.py capture`
saves their prior immutable images and Job template/trigger configuration, plus
the scan App's ready revision. It rejects missing resources and unreviewed literal
configuration rather than creating an incomplete recovery artifact. This path
supports the existing resource inventory, not first-time provisioning.

On runtime failure, an independent recovery step attempts every captured Job and
the scan App even if the API/recording rollback failed. Job definitions are read
back; the scan App must reach the exact recovery revision and previous image.
Recovery never stops or replays Job executions: existing executions can retain
new code until they finish. The separate recording topology recovery remains
responsible for its five Jobs and safe drain. A failed resource recovery keeps
the release failed and retains the non-secret snapshot for seven days.

This is not an atomic infrastructure or data rollback. Migrations and completed
business writes are not reversed; App-scope identity, secret references, Dapr,
registry and RBAC changes still require a reviewed inverse configuration through
the owning writer. A removed prior resource is an error, never implicitly
recreated or deleted. Run `python3 scripts/test-worker-release-rollback.py` and
`bash scripts/test-release-workflow.sh` for the focused recovery checks; CI keeps
the full existing verification suite.

### Shared infrastructure release ownership

Normal Asset releases reference the central `azure-infra` Key Vaults and six
managed identities as existing resources: runtime/migration vaults, ACR pull,
runtime, migration, scan, signature-refresh and derivative identities. They do
not redeploy those shared resources. Existing identities, secret references,
role assignments and workload permissions are unchanged.

The scan-warmer identity, six recording event/poison queues, and isolated
recording-source storage/lifecycle/Defender/RBAC are not currently managed by
central Terraform. Asset remains their owner; the separate recording-source
workflow retains its explicit preview/apply approval. They must not be changed
to assumed central resources before reviewed import and no-op ownership proof.
The ordinary release continues to disable shared-infrastructure and permission
provisioning flags. This source change does not detach state or delete resources.

## Application traces

The API exports OpenTelemetry server spans and fixed-endpoint internal Audit
client spans when the existing managed `OTEL_EXPORTER_OTLP_ENDPOINT` is present.
No new credentials are needed. Without an endpoint, existing HTTP correlation
continues and application span export stays disabled. `OTEL_TRACES_SAMPLER_ARG`
sets root sampling between 0 and 0.1, defaulting to 0.01. Remote sampled parents also pass through this
local ratio; remote unsampled parents remain unsampled and local sampled children
keep their decision. Incoming remote spans may therefore be dropped by sampling
while their trace IDs continue to propagate. This is not a hard export quota. Health/readiness
probes do not create spans.

Spans contain only fixed operation names, bounded HTTP methods/status codes and
fixed transport error categories. URLs, headers, object IDs, payloads, peer
addresses and raw errors are excluded. Export buffering is bounded and failures
do not fail application requests. Streaming interfaces and HTTP cancellation are
preserved. Audit transport spans cover response headers, not response-body
consumption; durable outbox attempts start independent traces when no live parent
exists. Audit provenance is unchanged.

Scan, derivative and recording workers/Jobs, storage SDK calls, queue continuity
and the scan-warmer Operations client are not instrumented by this API change.
No queue payload or schema is changed. A local collector test proves export;
production ingestion and cross-service parent/child acceptance require a released
revision and independently queried telemetry.

The Go SDK makes the same deterministic decision for the same trace ID and ratio
at each API, avoiding a separate 1% probability at every Go hop. An independently
sampled 1% Sentry frontend combined with this 1% Go ratio has a theoretical
complete-chain intersection around 0.01% under independent uniform sampling,
not a guarantee. Cache hits, unsampled parents and async boundaries still affect
which network spans exist. This change does not alter frontend sampling.
