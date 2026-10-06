# Recording pipeline candidate — acceptance in progress

Measurement window: 2026-10-06–07, Asia/Taipei. This is local candidate evidence,
not a production release or a provider-billing acceptance.

## Matched short host fixture

Use the fixture, host/tool versions and command in
`2026-10-06-recording-pipeline-baseline.md`. Candidate samples were run without
concurrent builds/race suites; earlier contended samples are excluded.

| Metric | Baseline | Candidate |
| --- | ---: | ---: |
| Validation seconds, raw | 2.151613 / 2.162208 / 2.235617 | 1.803183 / 1.788681 / 1.802615 |
| Median seconds | 2.162208 | 1.802615 |
| Final GETs | 10 | 5 |
| Final GET bytes | 66,316,545 | 33,157,709 |
| Conditional COPY / HEAD / inventory PUT | 5 / 10 / 1 | 5 / 10 / 1 |

The short-host median is approximately 16.6% lower. Object reads/bytes are halved
at the test-storage boundary, not measured from an R2 invoice. Setup encoding is
excluded; this is not the total upload-to-watch time or a long-recording SLA.
Package size includes generated inventory JSON; GET bytes count declared objects
actually read, not that generated JSON.

The candidate retains hashes, exact lengths, immutable ETag-bound copies, both
ffprobe passes, full decoding and ordered/cross-rendition timeline checks. Two
fragment workers share one budget; the init is cached per rendition/attempt.
Progress remains advisory; only the existing fenced ready transition permits use.
No upload-concurrency, bitrate, scheduling or allocated-resource change is made.

## Release gates still open

- Three paired long recordings with all three qualities on the same Linux
  FFmpeg build, at 4 CPU / 8 GiB.
- Dual-job peak RSS/scratch, legal maximum object and 20–50 GB source capacity,
  including failure/retry/cleanup cost rather than only successful validation.
- Complete ready → requested custom cover → requested publication → playback
  timing. Ready alone does not satisfy a requested cover/publication operation.
- Provider usage/cost, including additional fenced progress writes, failed
  attempts, staging/final copies, cleanup, Blob source, preview/cover artifacts
  and idle cron baseline. The 105% ceiling is not proven by fewer GETs alone.
- Final whole-branch independent review, PR required CI and approved release;
  producer SDK publication before the Admin exact-version lockfile update.
- Desktop/mobile visual confirmation of the banner and Admin progress. The font
  cmap and DOM checks passed, but the local screenshot service timed out.

Upload concurrency stays at three: no matched network/billing evidence justifies
raising it to six. There is no new public option or auto-tuning framework.
No production upload, infrastructure update, merge or release was performed for
these measurements. Keep the performance release gate closed until matched
end-to-end reliability and complete cost evidence meet the approved limits.
