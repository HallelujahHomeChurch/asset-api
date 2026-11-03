# Recording audio derivative operations

Version `aac-hls-v1` copies existing validated AAC-LC packets into MPEG-TS HLS.
Each original segment keeps its sequence and PTS; 10-second OBS and legacy
30-second packages retain their boundaries. Media playlists contain audio only,
and complete output is decoded and checked before publication. Original source
uploads are unnecessary: only the ready immutable package is read.

A claim snapshots source final prefix and inventory digest. Publication and
heartbeats recheck source identity, package readiness/expiry, deletion tombstone
and revocation under slot/package locks. Output lives below the package final
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
payload is about57.6 MB/hour at128 kbps, plus measured TS overhead. Measure actual
CPU/wall time and output bytes before proposing a production backfill budget.
The existing two slots are a global cap, not permission to consume both during
live workloads.

No historical apply/enqueue command is delivered in this slice. A separately
reviewed bounded idempotent backfill operation and concrete production preview
must receive explicit approval before any historical scheduling. Do not use SQL
mass inserts or table defaults to bypass this gate.

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
