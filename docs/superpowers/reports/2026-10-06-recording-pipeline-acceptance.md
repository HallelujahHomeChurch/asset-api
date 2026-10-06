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

## Same-image Linux short comparison

Both test binaries used the same local recording runtime image
`sha256:5d15e474b9b6b0fbe9412e124cb61b94ae50bcc89b31553940bfd8095e07a631`
(arm64, Debian FFmpeg 5.1.9-0+deb12u1), with 4 CPU / 8 GiB caps, no network,
read-only root and a 1 GiB temporary filesystem. Baseline code: `8d4f5e8`;
candidate: `18e65bb`. Encoding/setup remains excluded.

| Metric | Baseline | Candidate |
| --- | ---: | ---: |
| Seconds, raw | 3.229061 / 2.652022 / 2.639089 | 2.231635 / 2.154760 / 2.237592 |
| Median seconds | 2.652022 | 2.231635 |
| Final GETs / bytes | 10 / 66,316,535 | 5 / 33,157,704 |

Median is approximately 15.9% lower on this short Linux fixture. This still runs
on a local Apple-M4 Docker VM, not Azure hardware, and establishes neither
long-recording nor dual-job capacity/cost acceptance. In-memory test storage is
not R2 network latency. Allocations are not peak RSS or provider billable CPU.

## Release gates still open

Prefer Asset-first release, then CMS, SDK and Admin. Missing worker summaries
remain unknown. The strict legacy Asset decoder rejects `sourceItems`; CMS
handles only its exact bounded `AST_INVALID_REQUEST / invalid request body`
decoder response by retrying the existing package-only lifecycle request.
There is no generic HTTP-400 fallback: association, permission and provider
failures still propagate. The fallback returns no source metadata or grants;
strict legacy decoding and non-bypass behavior have regression tests.

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
