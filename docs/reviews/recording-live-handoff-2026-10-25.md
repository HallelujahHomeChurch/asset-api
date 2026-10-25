# Live ownership, VOD handoff and the two-core candidate

Scope: continue Asset PR #166 and frontend-platform PR #164. Engineering issues
are Asset #167 and frontend-platform #169, under planning MED-05 (#15), preserving
MED-02/MED-03 and the shared-player scope of MED-04. Planning entry revision:
`73ee1af2a1ff828d72421a73626f45ff4ce04a6f`. Stop before merge, publication or deployment.

## Ownership and compatibility

| Lane | Durable work |
| --- | --- |
| live | Live segment validation, capture thumbnails, live/default/custom covers |
| preview | VOD covers and ready-package preview finalization |
| processing / maintenance | Existing processing and cleanup responsibilities |

Live readiness includes auxiliary-only work before input, missing thumbnail
metadata, lag and exhausted-cover settlement. The owner attempts auxiliary work
after a media batch and before idle exit. Required source/package/live work keeps
its existing SQL priority; all claims still share two global media slots. No new
queue, Redis use, migration, retention rule or media-validation shortcut is added.
Exit telemetry distinguishes auxiliary claims from media batches.

Stop/seal is not VOD readiness. Capture thumbnails remain eligible during
uploading/freezing/validating, until the associated package is ready. A live claim
already admitted may finish with its valid lease; VOD waits for finish or expiry.
The progress row serializes admission and a fresh read checks the competing
owner. An expired claim cannot publish or advance durable progress.

VOD resumes at `completed_sequence + 1`, reuses immutable JPEG/VTT receipts and
publishes the cumulative index last. Tests cover a previous five-second live
sprite followed by a new ten-second partial tail, retry after output write but
before progress commit, and old orphan-JPEG recovery. No existing sprite is
rewritten. Source sampling remains ten seconds with five-second VTT cue bounds;
this changes neither media-fragment cadence nor the Event Job polling interval.
The companion player tests cover cached hover reentry, fractional PTS drift,
real gaps and late metadata from a replaced live session.

## Isolated resource evidence, 2026-10-25

The recording Dockerfile built successfully. Linux ARM64 test binaries from this
PR ran inside its FFmpeg runtime image with enforced cgroup quotas and a
disposable PostgreSQL database. Synthetic object storage is in memory. Image ID:
`sha256:aa22b1e47c2c0fbfc5be7f3d97012c2590c5aa8ea6c729e60cda5e717faa2e16`.

`TestLiveTenSecondPairsWithPreviews` runs three independent paired-arrival
batches. Each pair fully validates 1080p/720p/480p moving 29.97fps media and commits
both capture thumbnails. Fixture preparation is outside the batch timer; SQL,
decode, stable publication and thumbnail work are inside it.

| Enforced tier | Paired batch seconds | Peak cgroup bytes | OOM / exit |
| --- | --- | --- | --- |
| 2 CPU / 4 GiB | 2.178, 1.926, 1.908 | 430784512 | false / 0 |
| 4 CPU / 8 GiB | 1.539, 1.913, 3.726 | 337842176 | false / 0 |

All six batches finished within the next 10.01-second arrival interval, with
zero thumbnail backlog. The same quota runs passed real mixed-sampling/partial-tail
preview generation, cover generation and short-tail validation before/during
seal, including rejection of a sealed short interior fragment.

Reproduce with a disposable test database accessible from the container:

```sh
docker build -f Dockerfile.recording -t recording-live-lab .
mkdir -p .artifacts/live-lab
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go test -c ./internal/recordingvalidation -o .artifacts/live-lab/validation.test
# TEST_DATABASE_URL must point only to the disposable fixture DB.
docker run --rm --cpus 2 --memory 4g --memory-swap 4g \
  -e HHC_REQUIRE_MEDIA_TESTS=1 -e TEST_DATABASE_URL \
  -v "$PWD/.artifacts/live-lab:/bench:ro" --entrypoint /bench/validation.test \
  recording-live-lab -test.v -test.count=1 -test.timeout=5m \
  '-test.run=^Test(LiveTenSecondPairsWithPreviews|PreviewRealNonzeroTimestampAndPartialTail|S10LiveWorkerShortTailBeforeAndDuringSeal|LiveCoverUsesFirstVerifiedFragmentAndSkipsBlack)$'
```

Use the Docker server's architecture when compiling. These short ARM64 lab
batches are directional capacity evidence, not Azure runtime/AMD64, network,
long-session, device or billing acceptance. Shared host scheduling also prevents
interpreting the two rows as a speed or savings comparison. No production media
or cloud resource was used. An increasing backlog in representative Azure
overlap testing fails the later two-core acceptance gate.

## Configuration and later rollout gates

`RECORDING_LIVE_CPU` accepts only `2` or `4`; Bicep maps these to 4 or 8 GiB.
The default remains `4` for staged delivery. Setting `2` requires a separately
approved normal release. The release passes the same value to Provider what-if,
deployment and readback. Tests reject invalid tiers and resource drift, and prove
the existing topology snapshot restores either prior tier. Event min 0/max 1,
visible-only scaling, processing and preview tiers remain unchanged.

This PR changes one CD parameter, not CI selectors, test sharding or build
optimization. Frontend main's independent CI-selection changes are retained.
Required exact-head CI must pass before review; merge is explicitly not authorized.

After a later approved merge/release: review Provider what-if for the exact Job,
drain active claims through the existing rollout path, select `2`, verify the
deployed image/2 CPU/4 GiB and trigger, then prove real overlapping captures,
thumbnail lag, stop-to-VOD, authorization and player/device continuity. Preserve
the previous image/topology for rollback to `4`. Never roll back to a
five-second-only preview writer while a new ten-second JPEG has no receipt;
finish those receipts with a compatible writer first.
