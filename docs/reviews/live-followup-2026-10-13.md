# Oct 12 long-field validation investigation

Evidence source (immutable):
https://github.com/HallelujahHomeChurch/hhc-obs-plugin/blob/f7a3b28a77e312cf2f948d2eb5d9162057387a87/docs/reviews/mac-auto-cover-original-page-followup-2026-10-12.md

All field times below are 2026-10-12 Asia/Taipei (UTC+8).
Recording `b0cd1c3b-74db-4847-b62a-86ab8ae4bffa`, capture
`03470f36a8686b42834682bee9a234f8`; the disposable field was subsequently deleted.

## Verified historical evidence

- Seal accepted 22:15:31.532366; source inventory 943 objects, 6,217,876,837 bytes,
  three renditions of 312 segments / 9353.744400 seconds.
- Azure execution `asset-recording-validation-29863576` was scheduled at 22:16.
  System logs show completion 22:38:41 and exit code 0 / ProcessExited.
  Capture-specific live stages completed around 22:16:28. These are not the
  subsequent complete package-validation stages.
- The second package attempt started at 22:44:20.672697 (Windows API evidence).
  It reached ready at 23:12:23.392006. Azure execution
  `asset-recording-validation-29863604` completed at 23:12:31, exit code 0.
- Read-only Log Analytics queries found minute-by-minute scheduled worker
  health logs across 22:15–23:15. They contain live-stage telemetry, but no
  package attempt result, failure reason, or per-stage package timings.
- The deployed Job image was
  `sha256:b4aa0750b6d6547a67196c625aab23905b6088668cb207ad575f92acf9a48345`;
  ACR tags identify `main-ce418df`. The Job had 4 CPU / 8 GiB, a one-minute
  schedule, replica retry limit 0, and a six-hour platform timeout.
- No production jobs, data, resource sizing, or schedules were changed during
  investigation. Direct database diagnostics were unavailable because the
  current operator cannot read the runtime database credential; permissions
  were not changed. No credentials were persisted or printed.

## What the implementation establishes

`RunProcessingClaim` classifies a non-invalid validation failure as `retry`,
commits that outcome through `FinishPackageValidation`, and returns the database
finish result. A successful durable retry handoff therefore exits 0. Azure
Succeeded describes the process, not media readiness. Invalid media remains
terminal; renewable fenced leases guard ready commits.

`FinishPackageValidation` delays the next attempt by five minutes, and the next
attempt uses a new immutable prefix and revalidates the complete package. The
observed approximately 57 minutes consists of an unsuccessful first execution,
roughly five minutes of retry backoff plus scheduling, and a second full attempt
lasting about 28 minutes. This is consistent with the durable retry path; the
historical evidence does NOT identify the original transient error or the exact
phase responsible. In particular, it does not prove a decoder, storage, CPU,
lease, or Windows fault.

## Scope of this delivery

Add bounded, sanitized package claim/stage/progress/outcome diagnostics so the
next reproduction can identify where time is spent and why a retry occurs.
Do not log raw provider errors, URLs, credentials, object keys or source titles.
Keep all validation, fencing, retry limits/backoff, concurrency and resource
budgets unchanged. This diagnostic repair is not a claim that the historical
57-minute latency is fixed, nor a performance guarantee for new fields.

The accompanying CMS/Admin changes fix the same-page Auto cover retrieval,
broadcast-owned auto-publication display and public-range recording durations.
B1 `b1-2026-10-10.rc1`, document `b1-mac-2026-10-10.rc2`, C1
`c1-2026-10-08.2`, inventory schema 1, digests, idempotency, 30000/1001 fps,
900-frame/30.03-second interior segments and media/security checks are unchanged.

## Targeted Windows follow-up

Use a new disposable field; retained originals `e496a04e-4e2e-4dad-9269-63a610dd7c06`
and `e9c5ebb4-74d7-46e0-a7b4-e3730579fc8a` are read-only. Existing test-window and
service-time pause authorization still applies. No file sharing is required.

1. Keep the original Console mounted from pending Auto cover through ready;
   confirm an actual image loads without reload. Verify a later automatic result
   cannot replace the operator's saved custom selection.
2. With broadcast auto-publish enabled, verify recording-management status stays
   consistent through End/processing/ready/publication. Also check explicit
   cancellation remains canceled; no manual publish to manufacture success.
3. Use nonzero Start and an End before source Stop. Compare list/recommendation
   duration with the actual public VOD range, retaining the private source length
   separately. Standalone recordings must retain their full duration.
4. Record seal, package claim, phase counts/timings, any retry outcome and ready
   timestamps. A short field validates telemetry, not long-field performance.
   One representative long field is needed to assess whether the 57-minute
   behavior recurs; if it retries, correlate the new sanitized stage/reason.
5. Preserve prior 379/388-second lag, strict pressure-cutoff failures and weak
   uncached seek evidence. This delivery does not claim those issues resolved.

Report actual plugin commit, deployed platform versions, recording/capture IDs,
timezone-qualified times and decoded positions/durations. Platform tests, CI,
release and smoke remain separate from Windows field acceptance.

## New diagnostic events

Correlate `recording_package_claim`, `recording_package_validation_start`,
`recording_package_progress`, `recording_package_validation_result`,
`recording_package_finish` and `recording_package_claim_exit` by package/claim ID
and attempt. Progress is bounded to phase/rendition changes or every 30 seconds.
A `finish` event with `ready=false`, `failure=retry`, `committed=true` means a
persisted retry even when the following process exit is successful. Use the
validation-result stage/reason to distinguish object transfer, probing/decoding,
timeline checks, and final inventory write. Claim-exit stages additionally
identify heartbeat, progress persistence, and ready/retry commit failures.
