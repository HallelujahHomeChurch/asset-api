# Live publication performance

Each new fragment still receives immutable hash, probe and complete decode checks.
A matching server validation receipt permits reuse of a stable init, hashing the
actual bytes again before decoding. A missing stable init falls back to staging;
a changed hash or other provider failure fails validation. Only successful reuse
retains the previous init ETag. Four immutable playlists publish concurrently and
join before the common pointer advances. Claim, lease and cleanup fences remain.

## Controlled comparison (2026-11-07)

`BenchmarkLivePublicationBeforeAfter` uses a real ten-second, three-rendition
fixture and controlled storage latency. The baseline performs the previous init
copy/publication and serial playlist operations; the optimized case reuses init
and publishes playlists concurrently. Each value is the median of three runs,
three iterations per run, including validation, stable publication, playlists
and pointer calls. It excludes database claims, queue waits and real-provider
latency. Values are seconds per segment:

| Environment | Provider delay per operation | Baseline | Optimized |
| --- | --- | --- | --- |
| macOS ARM64 / Apple M4 | 50 ms | 1.510 | 1.203 |
| macOS ARM64 / Apple M4 | 250 ms | 5.441 | 3.307 |
| Linux amd64, 1.5 CPU / 4 GiB, FFmpeg 5.1.9 | 250 ms | 7.563 | 5.332 |

The Linux container ran under emulation on ARM64: useful for a quota-constrained
comparison, not an exact production-capacity measurement. Its optimized measured
throughput was about 1.88 media seconds per wall second. This does not establish
live end-to-end latency, sustained backlog recovery or mixed-workload capacity.

Reproduce with `go test ./internal/recordingvalidation -run '^$' -bench
BenchmarkLivePublicationBeforeAfter -benchtime=3x -count=3` and the required media
tools. Long paced input, real storage, background workloads and device playback
remain independent acceptance checks. No production broadcast or load is created
by this benchmark.
