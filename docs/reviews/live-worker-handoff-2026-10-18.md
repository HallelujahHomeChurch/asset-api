# Live worker handoff repair

## Scope and historical evidence

Capture `dce3101b54b15a24624316eeb7e3e5b4` / recording
`2796ca13-51b1-4817-ac49-795848d68437` is read-only evidence, not a recovery target.
On 2026-10-18 UTC+8, sequence 402 was confirmed at 11:58:11.731;
sequence 401 finished at 11:58:11.884. The next worker started sequence 402 at
approximately 11:59:00.531: 48.8 seconds after confirmation.
Historical worker image: `sha256:ad2e99cf1d2a8ad2acf4d554dc7e8b30b74fc585dfd2dcefa60ec9bdae512932`.

## Change

Elect one live standby execution using a session-scoped PostgreSQL advisory lock,
following the existing recording session-lock pattern. It drains across minute
boundaries for at most five hours of claim admission, checking the lock connection
before each claim. A 5.5-hour context leaves a bounded completion margin under the
six-hour Job limit; cancellation retains the existing lease recovery semantics.
Idle input still exits using the existing recent-input predicate (two minutes);
no active input exits immediately. Database-session loss stops further admission;
the existing fenced leases remain authoritative for in-flight media.

Other scheduled executions skip live standby and run the unchanged cleanup,
cover, preview, source and final-validation path. The live owner returns after its
live session so it cannot append a 5.5-hour final job to five hours of standby.
Global processing slots remain **two**. Cron, Job resources, individual claim
leases, full validation, public cutoff, media profiles and wire contracts do not
change. No final-validation or publication-scheduler optimization is included.
A killed owner or five-hour lifetime boundary can still wait for the next cron;
this patch removes routine minute-by-minute handoff, not every possible restart gap.
During rolling release, an old worker can finish its original bounded batch;
existing capture/slot fencing prevents duplicate publication.

## Cost measurement

Use `recording_live_worker_start` and `recording_live_worker_exit`:
`elapsed_ms`, `processing_ms` (RunLiveValidation calls including unsuccessful polls),
`non_processing_ms` (remaining lifetime, mainly standby plus coordination),
`claims`, `polls`, `failed`. These are wall-clock attribution, not CPU utilization.
Join ContainerGroupName to execution start/end and immutable image. Sum all Job
execution seconds, including skipped-live executions doing other work.
At unchanged 4 CPU / 8 GiB, resource allocation is execution-seconds ×4 vCPU-seconds
and ×8 GiB-seconds. Azure billing/free grants are not inferred from application logs.

Deterministic three-minute fake-clock regression: 17 completed eight-second claims,
181-second owner lifetime, 136 seconds processing and 45 seconds remaining;
maximum ready-fragment wait ≤5 seconds. Owner allocation in this scenario is
724 vCPU-seconds / 1448 GiB-seconds, excluding startup and other scheduled jobs.
This is a controlled test, not a measured production bill or real OBS result.

A live owner may add allocated standby time compared with the old 50-second window;
other executions should no longer each spend 50 seconds waiting behind it. Measure
both effects before claiming savings. No live input must not acquire a five-hour
idle lifetime. No extra permanent worker, replica, slot or resource size is added.

## Validation and minimum Windows follow-up

Local/CI: single owner, contender returns without consuming media slots, unlock on
error/cancellation, database connection loss, reacquisition, idle exit, deadline,
continuous arrivals across cron boundaries; existing full media/DB race suite.

After release readback, use one 8–10 minute authorized isolated OBS S10 short run.
No plugin or key changes are required. Record plugin commit, capture/recording IDs,
timezone, producer/common-confirmed/verified every five seconds and HTTP spans if
available. Mac correlates confirm→claim, processing and execution allocation.
Check no routine ~minute handoff stalls, no accumulating watermark lag and normal
End/Stop/seal/ready/natural publish. Do not reload/force-live as an acceptance aid;
preserve intentional DVR/pause/non-1x. Do not repeat the passed 2.5-hour run.
Do not mutate the protected formal recording. Historical latency/weak-network
failures are not reclassified by this patch. Deployment and live acceptance must
be reported separately.
