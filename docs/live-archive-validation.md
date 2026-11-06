# Live validation evidence and archive equivalence

This change records evidence only. Package freezing still performs its complete
hash, probe and decode validation. It does not enable a fast archive path.

The fenced live snapshot stores a validator version, exact init and segment
path/size/SHA-256, measured frame rate, frame count, timestamps and codecs. Only
the server validator creates this evidence after immutable copy, hash, probe and
decode succeed. No capture declaration or HTTP input accepts it. Snapshot JSON
is additive; legacy snapshots have no evidence and remain valid for playback.

| Required predicate | Live evidence / archive check |
| --- | --- |
| Approved video/audio formats, dimensions, stereo, complete decode | Versioned successful `probeFragment`; sealed rendition must match the live profile |
| Exact init and fragment bytes | Receipt matches every sealed path, size and SHA-256; storage identity still needs verification before reuse |
| Complete ordered three-rendition history | Exact sequence, rendition and segment counts |
| Frame rate and ten-second frame boundaries | Recorded measured rate plus existing `validateRenditionTimeline` |
| Continuous timestamps and total duration | Same global timeline validator as full freezing |
| Aligned rendition timestamps | Same cross-rendition reference timeline check |
| Master codec declarations | Existing `ValidateRecordingMasterCodecs` |
| Inventory and playlists | Existing validators still required; receipts do not authorize playlists |
| Publication, expiry, cancellation and stale workers | Existing processing claim and fenced finish; receipts confer no authority |

Before activating reuse, bind a trusted immutable storage source to the receipts,
retain conditional copy and final ownership, and test cancellation, stale leases,
retries and cleanup. Missing or incompatible evidence must use full validation;
invalid storage bytes must never become ready. No new media prefix or reader
format is introduced here, and existing cleanup ownership remains unchanged.

Tests compare real 30 fps and 30000/1001 fixtures (including short tails) through
both full validation and evidence checks. Negative evidence covers missing and
unknown versions, changed hashes, sizes, paths, rates, non-finite timestamps,
discontinuity and codec mismatch. Production throughput and long captures are
separate acceptance steps, not established by these fixtures.
