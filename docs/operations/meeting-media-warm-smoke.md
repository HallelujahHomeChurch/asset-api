# Meeting media warm acceptance

This runbook accepts the Phase 1 LINE-to-Asset-to-Presenter path. Run it only
with a test Meeting, a test LINE group, and an authenticated test Presenter.
Do not use member content or record LINE user, group, file, or device names.

## Eligibility and evidence

An eligible sample is received by the LINE webhook during a media-enabled
Meeting window, is at most 25 MiB, and is connected to a known online Presenter
configured as `always-offline`. The SLO clock starts at server-side
`media_sync.webhook_received` and stops at Asset API receipt of Presenter
`available-offline`.

Before the run, record:

- deployed revisions and immutable image digests for `hhc-web-api`, `asset-api`,
  `api-gateway`, and `hhc-line-function-bot`;
- Presenter source commit and packaged application SHA-256 separately from any
  release or installed-device evidence;
- test Meeting ID, UTC window, opaque correlation/asset/item IDs, file type,
  size bucket, and Presenter app version.

The shared Log Analytics workspace retains 30 days. In the **HHC Production
Observability** workbook, use:

- **Meeting media eligible latency and missing stages** for count,
  p50/p95/p99/max, missing stages, size bucket, and app version;
- **Meeting media scheduler executions and worker warm hours** for execution
  count and accumulated worker runtime.

The stage query joins only opaque `correlationId`, `assetId`,
`collectionItemId`, and `contentVersion`. A missing stage is a failed sample,
not zero latency. Restrict the workbook time picker to the recorded test window
before exporting evidence.

## Controlled run

1. Create or edit a test Meeting so the current time is inside its resolved
   media window. Confirm both one-minute warmer jobs run and the edit is visible
   within 60 seconds.
2. Send representative image, PDF, presentation/office, audio, and video files
   through the test LINE group. Include the `<=1 MiB`, `1-5 MiB`, and `5-25 MiB`
   buckets where the formats permit.
3. Confirm each clean item becomes `available-offline` on the authenticated
   Presenter. Record the opaque IDs and query the workbook after ingestion.
4. Confirm the eligible p95 is at most 20 seconds, every sample has all six
   stages, and both queue-scaled workers return to zero after their queues
   drain.
5. Record warmer executions and worker warm hours for the same window. Accept
   cost only from the measured window; do not extrapolate from deployment or
   test traffic.

## Security and fallback matrix

Run one controlled case per row. Never weaken or bypass the scanner.

| Case | Required result |
| --- | --- |
| Clean file | Published only after the clean scan result; authenticated Presenter receipt is recorded. |
| EICAR/infected | Remains unavailable, is never published, and has no Presenter receipt. |
| Transient scanner failure | Retries within the existing bound; download remains denied until a later clean result. |
| Meeting API unavailable | Warmer execution fails closed; ordinary queue-triggered cold processing still completes. |
| Warm queue unavailable | Warm pulse fails without losing durable upload/scan work; cold processing still completes. |
| Schedule edit | Both warmers observe the resolved-window change within 60 seconds. |

For pending, infected, and failed assets, verify the public and restricted
download paths deny access. Restore any temporarily unavailable dependency
immediately after its single case and verify readiness before continuing.

## Acceptance and rollback

Accept only when all eligible samples have attributable stages, p95 is at most
20 seconds, the fallback matrix has no security regression, workers return to
zero, and measured warm cost is acceptable. If latency misses, change only the
dominant consumer-owned interval or concurrency setting and repeat the full
sample; scanning and authorization gates are immutable.

For each repository, preserve PR, merge SHA, release workflow, image digest,
deployed revision, smoke result, and documented rollback target. Keep Presenter
source, package, release, and installed-device evidence as four distinct states.
