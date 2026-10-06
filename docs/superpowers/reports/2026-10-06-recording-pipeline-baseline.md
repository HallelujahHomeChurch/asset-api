# Recording validation baseline

Baseline production code: `97d8280`. Measurement date: 2026-10-06.

## Reproducible short benchmark

Run `go test ./internal/recordingvalidation -run '^$' -bench '^BenchmarkPackageValidation$' -benchtime=1x -count=3`.
Fixture: synthetic 35-second 720p/30 fps testsrc2 and 48 kHz stereo AAC; 30-second fMP4 segments; libx264 baseline level 3.1/ultrafast. The benchmark includes immutable copy, SHA-256, both ffprobe passes, full decode and inventory write. Setup/encoding is excluded. Counters use the test storage boundary, not provider billing.

Host: Apple M4/macOS arm64, Go 1.26.1, host FFmpeg 8.1.2. These host numbers are not an Azure CPU/resource-equivalent performance acceptance.

| Run | Validation seconds | COPY | HEAD | GET | GET bytes | Inventory PUT |
| --- | ---: | ---: | ---: | ---: | ---: | ---: |
| 1 | 2.151613 | 5 | 10 | 10 | 66,316,545 | 1 |
| 2 | 2.162208 | 5 | 10 | 10 | 66,316,545 | 1 |
| 3 | 2.235617 | 5 | 10 | 10 | 66,316,545 | 1 |

Median 2.162208 seconds. The fixture contains five inventory objects (master, rendition playlist, init, two fragments). Existing validation reads every final once for SHA, then init+fragment per segment and master again for media validation. This records duplicate reads before optimization; it is not evidence for a long-film speedup.

## Outstanding acceptance

Same-build Linux recording container at 4 CPU/8 GiB, three-quality long recording pairs, two concurrent jobs, maximum-object scratch peaks, 20–50 GB source scenarios, CPU/RSS/thread measurements, upload 3-versus-6 comparison, cover/preview/publication/playback availability and full provider cost remain unmeasured. No new production media was uploaded or published for this baseline. Historical 77-minute job time is not a paired benchmark.

The initial host suite failed `TestDataGovernanceMigratedColumnCoverage` because `TEST_DATABASE_URL` was unset. A dedicated local PostgreSQL container is used for subsequent full-suite runs; production data and other local test containers are untouched.
