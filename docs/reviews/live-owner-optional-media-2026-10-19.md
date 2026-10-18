# Live owner optional-media closeout

## Protected evidence

Recording `b53a3a41-3a3a-429c-b190-268a86876a16`, capture `f74f8d58009b895c7f61ebbd2f0f65be`, output `b96fa23c-a6fe-4e97-aade-13bbf714318c`. Windows plugin `3b4d364850d50518798264e86fe954f05e863e2c`, OBS 32.2.2, Asset 0000109, CMS 0000235. No original media, rows, Stop/seal, publication or key was changed.

## Proven scheduling gap

Times are 2026-10-19 Asia/Taipei UTC+8, taken from application log timestamps rather than ingestion time:

- Another package validation started 00:51:18.977: 1,036 objects / 7,594,137,493 bytes. Its progress continued throughout the test and through 01:25; it occupied one global processing slot.
- The tested live owner started 01:02:59.948 and ended 01:17:46.433. It processed 90 claims in 886,485 ms: 776,125 ms processing, 110,359 ms other time, 113 polls, failed=false.
- Scheduled executions at 01:03, 01:04, 01:05, 01:07–01:10, 01:13–01:14 and 01:16–01:17 sampled two active slots. 01:15 sampled one. The independent executions did not obtain a live owner lock and could attempt optional media, but only when the common slots and priority predicates allowed.
- The tested package validation started 01:18:19.493 and finished ready/committed at 01:22:10.991, attempt 1, 231,498 ms. Windows first observed cover ready at 01:22:38.568; this is not an exact Asset cover finish time.

The previous lease-predicate correction correctly allowed optional work beside already-claimed validation when a slot was free. It did not make the continuously elected live owner call optional processing. That owner repeatedly released/reclaimed its slot, or slept waiting for the next fragment, while independent minute workers repeatedly sampled full slots. This is a scheduling starvation window, not evidence of corrupt video or failed OBS delivery. Historical cover attempt/preview rows remain unavailable, so no exact historic claim/finish timestamp is inferred from poll observations.

## Narrow correction and bounds

When no live validation was claimed and live input remains active, the owner may process one optional cover or timeline-preview claim before immediately trying live validation again. All existing SQL priority predicates, global slots (2), transaction locks, fenced leases, verified/public media boundaries and worker ownership checks remain in force. A waiting live/package/source validation still wins. Cover processing retains its 30-second work deadline and preview its 20-second deadline, with existing finish deadlines. Optional failures retain existing durable retry behavior and do not abort normal live validation.

The change applies to legacy/live owner processing and leaves independent preview workers available. Concurrent attempts use the same fenced slots, so this does not create another media-concurrency budget. Event Job flags, queues, resource sizing, bootstrap/cutover and cron are untouched; the change is based on the merged Event Job code and is safe to retain when that separate feature is enabled.

Safe logs now expose live cover/preview claim IDs, elapsed processing time and generated/committed outcomes. No credentials, signed URLs, user names or raw error contents are emitted. Optional elapsed time is included in the existing worker processing_ms totals; these timings are not billing measurements.

## Verification and remaining acceptance

A synthetic-clock regression holds a background slot for the entire live interval and verifies images are serviced between successive live fragments; before the correction live=3/images=0, after live=3/images=3. Existing deadline, cancellation and cross-minute owner tests remain required. Real PostgreSQL optional-claim tests retain waiting-work priority, free-second-slot admission and two-slot exclusion.

After CI/release/readback, Windows needs only one short auto-cover live session: first cover ready while live, a five-minute refresh retaining the previous image while pending, live timeline JPEGs, and live-to-VOD corresponding JPEG hash reuse. Record image claim/result and sequence advancement so image work does not conceal a growing live backlog. Successful hover and closeout are already accepted and need no repeat suite. No plugin/wire change, long-field test, notification retest or concurrent YouTube load is required. Preserve all original recordings. Historical 503, weak-network/seek and 379/388-second failures retain their prior status.
