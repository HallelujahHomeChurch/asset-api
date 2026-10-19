# Recording Event reactivation checkpoint — 2026-10-19

## Scope and acceptance boundary

The user approved formal Event activation, safe rollback, global media concurrency
2 and complete validation. No OBS wire, key, capture identity, cutoff, inventory,
digest, idempotency or media-validation rule changes. First activate media lanes
at 4 CPU / 8 GiB; maintenance is 0.5 CPU / 1 GiB hourly at UTC minute 17.
Processing 2 CPU / 4 GiB and preview 1 CPU / 2 GiB remain separate, unaccepted
resource candidates. Runtime deployment follows the merged CI/CD path.

## Recovery evidence before reactivation

Initial Event Release 39192112433 succeeded (Asset revision 114), but its private
source smoke failed/retry. That failure remains failed; Job exit is not ready.
The original provider error was not recorded, so it is not a proven unique cause.
PR #125 reproduced and fixed a complete-body output request-cancellation race
using delayed storage. Worker cancellation, claim fencing and complete validation
remain enforced. It also preserves provisioned queue references when Event is off,
so rollback passes the unchanged deployment policy.

PR #125 merge: `5e069da37e2129dace9b533e7a275d00877e53b6`.
Required CI 39195364751 and Production Release 39196342123 succeeded.
Asset `asset-api--0000115` was Healthy/Running, 100% traffic, min replicas 1;
Event false, HLS/source true. Legacy minute schedule restored; Event Jobs Manual.
API image: `sha256:38094513b6987de6f0cde6bde43cb77bc68f7c7b47e2fb7fdc115cf92563980c`.
Worker image: `sha256:9c8e9c205c9d41390647cfc1846ce27802aa1d97bf5e6971cb7c0ccd400c02b1`.

Synthetic draft `adbfaee5-9b86-450a-aa86-14ab8e68ac7e`, source
`657663d1cba730456e2753a53c754604` was retried through its normal bounded UI
retry, without reupload or publication. Attempt 2 started at
2026-10-19T04:08:19.795319Z, reached package validation at
04:08:46.677570502Z and ready at 04:08:55.663798Z. Its original input hash and
completion receipt were retained. Package `a4be105fa1d1147664b82aa290229590`
contains the expected 480p/720p renditions for a 720p source; no upscaling.

The same ready source exposed a separate existing metadata gap: source finish
created no automatic cover row, unlike CLI package finish. This checkpoint adds
the existing auto/pending cover INSERT inside the same fenced ready transaction.
The regression fails before the fix (zero covers) and passes after it, asserting
one pending cover, actor ownership and inherited media expiry. PostgreSQL
Source/Cover/Event race tests passed in 14.050s; vet and diff checks passed.
Independent code review found no blockers.

For only this private synthetic package, the existing cover-backfill operator
command first dry-ran the exact one-package page, then applied that same page.
Executions: maintenance-7e1j2o5 (dry run) and maintenance-km8dcml (apply).
No formal recording, key, selected user cover, publication or Job template was
changed. Browser readback showed the three generated 1280x720 images. Reload was
used after late operator backfill; this does not prove original-page no-reload
recovery. Future source uploads receive the pending cover atomically via this fix.

Published recording `b8471446-b0b3-462a-a6a1-f25b1bfef3b8` was read-only tested
at 04:17:12–04:17:16Z: actual 1920x1080, position 120.010078 to 124.372989,
frames 22 to 155, duration 11120.375823, playing, rate 1, readyState 4, no error.
The private draft has no Admin VOD player; do not bypass publication/grants to
claim direct browser decode of that private source. Full server decode passed.

## Activation gate and required readback

Before merging this runtime change: final-head CI green; current production
healthy; no competing release; fresh full Provider what-if and unchanged policy;
exact 12 queue-scoped permissions; safe trigger pause/drain and quiet-window checks.
Set Event enabled immediately before the authorized merge. Its runtime change
automatically starts exactly one Production Release; do not also dispatch another,
rebuild a published commit or overwrite immutable provenance.

Read back actual revision, immutable API/worker images, health/readiness, 100%
traffic, min replicas 1, Event/HLS/source flags, legacy Manual, three Event lanes
min 0/max 1/poll 10/visible-only, hourly maintenance 0.5/1 and unchanged two global
DB slots. Run a new private source through cold wake, natural ready and automatic
covers; do not publish it. Original test data remains preserved.

## Remaining acceptance

Windows needs one non-meeting 8–10 minute S10 short scene for live cold wake,
cross-minute watermarks, actual member decode/follow, first/five-minute cover,
live timeline, natural Stop/seal/ready/publication and same-URL VOD decode/seek.
No plugin, wire or key changes, and no repetition of accepted long suites.
Keep historical lag/weak-network failures separate. Exact deployed versions and
fixed document hashes are supplied after successful release/readback.

At least 24 hours of all execution seconds (including standby/retries/other Jobs),
Queue/API/DB and cost-meter evidence remains necessary before savings acceptance.
Structural absence of minute-based heavy starts is not an actual billing result.
No background monitoring or resource-downsize acceptance is implied.
