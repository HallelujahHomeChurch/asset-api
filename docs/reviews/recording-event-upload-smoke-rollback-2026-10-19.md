# Event cutover source-upload smoke and rollback checkpoint

The user approved formal Event activation, complete validation, the two global
media slots, and rollback on regressions. Production Release 39192112433 for
`e9f057dd0d2e83c187e0ecf774182cdbba937f7a` succeeded, including immutable
provenance publication. API `asset-api--0000114` was Healthy/Running, traffic100%,
min replicas1, Event/HLS/source true. Three Event lanes were 4 CPU/8 GiB,
min executions0/max1, queue visible-only/poll10s. Legacy was Manual;
maintenance was hourly UTC minute17, 0.5 CPU/1 GiB. Global media slots remain2.

## Failed acceptance; preserve evidence

A new private synthetic source draft is the only test data created:

- recording `adbfaee5-9b86-450a-aa86-14ab8e68ac7e`
- source `657663d1cba730456e2753a53c754604`
- source SHA-256 `bfe4e7fb30bcd813e2a8ee48939a8f86603fea34abd837d7244a2eca5eb4762f`
- 7,700,777 bytes; MP4, 1280x720, 30000/1001,40.04s,audio present
- complete accepted `2026-10-19T03:16:31.464301Z`
- Event execution `asset-recording-processing-w49nd` started03:16:48Z
- source attempt1 started03:17:08.580335Z, phaseencoding03:17:08.85386351Z
- last heartbeat03:17:22.608138796Z; final statefailed/failureCode retry
- event worker exited03:17:23.2538193Z,claims1,failedfalse

A successful Azure execution only means the claim/state write completed; it does
not mean source ready. Full local processing of these exact bytes passed through
package_validation with two renditions in10.16s. The first cloud failure is not
explained by that local pass. Current public diagnostics do not expose its exact
underlying encoding/storage cause. Old/new Job environment references match,
except worker mode. No speculative root cause is claimed.

The UTC03:17 hourly maintenance execution Succeeded. Existing published member
VOD was decoded before activation,852x480,375frames,MediaErrornull; this does not
establish new upload acceptance. No synthetic publication/notification occurred.
Preserve the source/draft/attempt; do not resubmit/delete it to erase the failure.

## Reproduced source-output cancellation defect

A storage stub delayed by350ms reproduces `recording_output_upload_failed` with
these same bytes and the existing complete35s/three-rendition encode test.
The spool reads and hashes the complete media body, then passes r.Context to R2.
FFmpeg can close that completed PUT while R2 is committing, canceling a valid
worker's storage operation. A deterministic test cancels the request exactly at
storage admission and reproduces422/no committed object before the fix.

After the body is fully received and validated, use the spool's worker context
for the R2 write. The active worker/claim cancellation still cancels storage;
partial/invalid bodies, bounded scratch, hashes, immutable paths, full package
validation and backpressure remain. Tests retain worker-cancellation rejection
and exercise a complete encode/full validation with delayed storage. No media
state is declared ready from this fix alone.

This is a proven code defect and matching failure class, not proof of the exact
unrecorded provider error in the first cloud attempt. Preserve that failed
attempt. Release initially remains Event=false, then verify the same source via
the normal bounded retry before considering reactivation.

## Approved safe rollback

Use this fresh merged commit as a new immutable release checkpoint. Do not
rebuild the already published e9f057d commit or overwrite its provenance.
Set RECORDING_EVENTS_ENABLED=false; keep provisioned=true,approval=true,
processingCPU4,previewCPU4. This checkpoint also fixes the template queue references, so the approved merge
automatically deploys once. Do not manually dispatch a second release for its SHA.

The original rollback preview exposed six expected queue-reference deletions
from API/legacy environment arrays, correctly blocked by the deletion policy.
Retain these non-secret references whenever queues are provisioned; the Event
enabled flag still gates dispatch. A wiring regression fails before this fix.
Do not relax the deletion policy to bypass this failure.

The existing full Provider preview/what-if policy must pass. Quiet window:
no new uploads or OBS sessions; wait all recording executions to drain without
force-stopping. Restore legacy minute Schedule, Event lanesManual, maintenance
Manual, APIEventfalse through CI/CD. Preserve queues, grants, durable work,
receipts, source media and existing recordings. No migrations are reversed.
Read back actual image/revision/traffic/health and Job topology after rollback.

The healthy fallback still uses the existing complete media implementation and
2 slots. If this source also fails under the unchanged legacy path, investigate
it separately rather than silently claiming an Event-specific regression or
loosening validation. Do not ask Windows to start Event real-OBS acceptance
until source-upload acceptance and formal activation are ready.

## Unchanged scope and open gates

This checkpoint fixes infrastructure reference retention and source-output
cancellation, with no API/wire/digest/public boundary/media
validation, key or recording. Source and Event diagnostics remain open.
Windows Event short-scene acceptance, >=24h all-execution/actual-cost evidence,
and processing2CPU/4GiB/preview1CPU/2GiB tier A/B are not passed.
