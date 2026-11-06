# Live validation evidence and archive equivalence

Sealed live recordings may reuse a complete published ended snapshot. The worker
waits at most 60 seconds, including database access, for the final live batch.
Legacy or incompatible evidence uses the existing complete validation path.
A missing or expired capture cannot authorize reuse.

The fenced live snapshot stores a validator version, exact init and segment
path/size/SHA-256, stable object ETags returned by conditional publication, measured
frame rate, frame count, timestamps and codecs. Only
the server validator creates this evidence after immutable copy, hash, probe and
decode succeed. No capture declaration or HTTP input accepts it. Snapshot JSON
is additive; legacy snapshots have no evidence and remain valid for playback.

| Required predicate | Live evidence / archive check |
| --- | --- |
| Approved video/audio formats, dimensions, stereo, complete decode | Versioned successful `probeFragment`; sealed rendition must match the live profile |
| Exact init and fragment bytes | Receipt matches every sealed path, size and SHA-256; conditional copies require the recorded stable object ETag |
| Complete ordered three-rendition history | Exact sequence, rendition and segment counts |
| Frame rate and ten-second frame boundaries | Recorded measured rate plus existing `validateRenditionTimeline` |
| Continuous timestamps and total duration | Same global timeline validator as full freezing |
| Aligned rendition timestamps | Same cross-rendition reference timeline check |
| Master codec declarations | Existing `ValidateRecordingMasterCodecs` |
| Inventory and playlists | Existing validators still required; receipts do not authorize playlists |
| Publication, expiry, cancellation and stale workers | Existing processing claim and fenced finish; receipts confer no authority |

The fast path verifies and freezes playlists normally, checks all evidence and
global media predicates, then copies receipt-bound stable media into the existing
attempt-owned package prefix with six bounded workers. It downloads no media
bodies and launches no decoders. Conditional copy failure is an error, never a
successful archive. All provider calls join before cancellation or completion;
package.json is written last and the existing lease/expiry fence owns ready.

No new media prefix or reader format is introduced. Existing package readers and
cleanup work with both fast and fully validated output. Rolling back either
worker loses the optimization safely: missing version/ETag evidence uses complete
validation. The old worker can consume the resulting package unchanged. Existing
live cleanup keeps active processing sources protected. Long-capture performance
and provider copy throughput remain separate measured acceptance gates.

Tests compare real 30 fps and 30000/1001 fixtures (including short tails) through
both full validation and evidence checks. Negative evidence covers missing and
unknown versions, changed hashes, sizes, paths, rates, non-finite timestamps,
discontinuity and codec mismatch. Production throughput and long captures are
separate acceptance steps, not established by these fixtures.
