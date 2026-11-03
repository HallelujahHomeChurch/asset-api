# Recording audio derivative operations

Version `aac-hls-v1` copies existing validated AAC-LC packets into MPEG-TS HLS.
Each original segment keeps its sequence and PTS; 10-second OBS and legacy
30-second packages retain their boundaries. Media playlists contain audio only,
and complete output is decoded and checked before publication. Original source
uploads are unnecessary: only the ready immutable package is read.

A claim snapshots source final prefix and inventory digest. Publication and
heartbeats recheck source identity, package readiness/expiry, deletion tombstone
and revocation under the existing recording-owner authority fence before slot/package locks. This serializes publication with SetBroadcastRange and deletion without reversing their lock order. Output lives below the package final
prefix in `audio/aac-hls-v1/{attempt}/`; a create-only `current.json` commits the
first complete attempt. The public session route never accepts attempt IDs or
pointers. Video readiness does not depend on the optional derivative.

The preview event lane reuses two global slots, existing higher-priority media
admission and at most three attempts. No extra service, queue, worker resources
or production configuration is introduced. Local scratch is bounded to one
init+fragment and one output, with free-space admission. Each output is at most
8 MiB. Failed and ambiguous attempts remain discoverable; package maintenance
sweeps declared keys after original expiry and existing grant cleanup grace,
then repeats to catch late writes. Metadata retention remains pending legal
policy, as with existing preview attempt receipts.

## Historical preview and approval

Migration049 creates empty tables. New successful ready transitions enqueue
only their own package. Existing ready packages remain omitted, with no audio
metadata and no background backfill. Deploy schema/runtime/Worker through normal
reviewed native release, and retain the video path during rollback.

After accepted immutable release, the existing migration Job can execute its
released `/asset-migrate --recording-audio-plan` through an execution-only
container template override, preserving image digest, environment secretRefs and
resources. Verify the Job's actual configuration and CLI support before use;
do not use local unreleased binaries, alter permanent configuration or print
credentials. The plan does not run migrations and needs migration049 present.

Output contains aggregate `packages`, `inputBytes`, `segments`, `durationSeconds`.
It excludes expired/deleted/revoked and already scheduled packages. Input bytes
sum the lowest-bitrate rendition init and fragments, not all video renditions.
No package IDs, URLs or object keys are printed. To estimate retry cost, account
for three attempts and repeated init reads per segment; output nominal AAC
payload is about57.6 MB/hour at128 kbps, plus measured TS overhead. A31.2-second synthetic sample produced630552 TS bytes with standard0.7-second mux buffering versus822124 bytes with zero buffering; timestamps remained unchanged. This small artificial sample is not production capacity evidence. Measure actual
CPU/wall time and output bytes before proposing a production backfill budget.
The existing two slots are a global cap, not permission to consume both during
live workloads.

### Approved bounded enqueue

After root has received explicit production approval for the current count/cost
preview, use the same accepted released immutable migration image and existing
Job execution-only override with `/asset-migrate --recording-audio-apply --recording-audio-limit=1` for an approved one-package pilot.
The optional limit defaults to 100 and accepts only integers 1..100; specify the
approved maximum explicitly. It is valid only with apply, never plan or normal
schema migration. The database repository independently enforces this bound.
After measuring actual pilot bytes, CPU/wall time and higher-priority media
impact, obtain a fresh plan and approval for the remaining bounded budget.
Neither audio flag runs schema migrations; specifying both flags, unknown flags
or positional arguments fails closed. Never execute apply merely to obtain a
preview, and never use an unmerged local binary against production.

One invocation inserts at most the requested limit (never above 100) pending
rows in one transaction. Output for a default-limit batch is
`{"enqueued":100,"remaining":{"packages":150,"inputBytes":...,"segments":...,"durationSeconds":...}}`.
These numbers illustrate a synthetic 250-candidate fixture, not production data.
Each candidate must still be ready, unexpired, undeleted, unrevoked, have a valid
immutable source prefix/digest and have no audio state. Existing pending,
processing, ready and failed rows are all untouched. This tool does not retry
failed derivatives or replace completed output.

Explicit batch commands serialize with each other; each candidate acquires the
existing owner-authority fence before its package row. Eligibility and source
identity are rechecked after waiting, preventing a concurrent revoke, delete,
expiry or source change from crossing the preview/insert boundary. An enqueue transaction
error rolls back the whole batch. If commit succeeds but the remaining-plan query
or response fails, existing rows remain excluded and the primary key prevents
duplication, but another apply can enqueue the NEXT batch. Do not blindly retry
a lost response: inspect execution evidence and obtain a fresh plan, then
confirm the remaining approved budget before issuing another apply. The limit
is per invocation, not a cumulative campaign cap or a retry idempotency key.

Run one batch, inspect its aggregate result and existing live/validation backlog,
and obtain a fresh plan before continuing within the approved budget. Do not
loop automatically or create a new schedule. Stop when a fresh plan reports zero;
a zero inserted batch alone may reflect concurrently changed candidates.
Apply only enqueues into the existing preview lane: no media job is directly
started and the existing two-slot cap, video priority and three-attempt policy
remain unchanged. No new infrastructure or permanent Job config is required.

A rollback may keep queued rows while old video playback remains available;
do not delete state or clear failures to force processing. Retain the producer
and Worker contract until consumers no longer depend on audio. No production
apply was performed as part of implementation or local validation.

## Consumer contract and verification

Private status: optional `audio` with version `aac-hls-v1`, state
`pending|processing|ready|failed`, and ready-only literal playlistPath
`audio/aac-hls-v1/index.m3u8`. CMS must validate the literal and retain its existing
membership/content gate. The playlist uses the current package/session base;
no separate credentials, longer grant or renewal endpoint is needed.

Worker grants cover GET/HEAD and Range with exact paths. Broadcast policy still
filters every segment and projects the playlist to current boundaries before
any cache read. Audio resolves its own package derivative pointer rather than
falling through the broadcast raw-capture video rewrite. MPEG-TS responses use
`video/mp2t` even though the transport contains only AAC.

Local synthetic media, PostgreSQL and Worker tests do not prove production
integration, browser background behavior or physical iOS/Android continuity.
Those remain controlled release and consumer/device acceptance gates.

## Pilot measurement

A claim emits `recording_audio_claim` and `recording_audio_claim_exit` with
`claim_id`, `recording_id` and elapsed milliseconds. The exit reports
`finish_committed` and `ready`; only a committed ready result establishes
successful publication. `recording_audio_generate` reports elapsed time,
`success`, `input_bytes`, `output_bytes` and successful `segments` even when
processing partially fails. Generate success alone is not ready publication.

Input bytes are actual `io.Copy` counts into local processing, including each
repeated init read. Output bytes count successful segment/playlist PUT payloads;
failed or ambiguous PUTs and the final pointer are excluded. These are logical
application bytes, not SDK retry traffic, wire bytes or billing receipts. The
logs contain no media URL, object key, raw error or credentials. Sum separate
claim attempts for actual retry costs; do not add overlapping durations to infer
wall time. Per-claim CPU is not recorded. Existing Azure execution CPU/network
metrics may provide coarse corroboration, but include startup/cleanup and other
work and are not per-claim CPU or object billing proof.

For an approved single pilot, retain the one apply execution's numeric result
and correlate subsequent audio claim/generate/exit events by claim ID and their
ContainerGroupName/execution. Verify the committed-ready event and current
member authorization before sharing the matching recording's public canonical
URL. Never share grant credentials, private object paths or session URLs.
If concurrent new-ready work makes the selected historical recording ambiguous,
use existing authorized read-only metadata to resolve it; do not infer identity
from time proximity alone. No production configuration changes are required.
