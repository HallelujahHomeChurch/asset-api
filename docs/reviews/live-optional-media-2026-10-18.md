# Live optional media follow-up

Protected recording: `847f8188-7a70-4184-801d-8f30a3983b25`; capture: `785329968b57b255a78989e104650392`. No production rows or media changed for diagnosis.

## Confirmed code defect and narrow correction

Auto cover and live timeline preview share `previewWorkWaiting`. It counted validation work with an active claim as waiting work. Thus a claimed live/package/source validation job could prevent optional work using the free second processing slot. Actual validation claim queries already exclude active claims.

The correction applies that same lease eligibility to the priority predicate. It does not change validation, lease fencing, global slot count (2), wire, cron/event triggers, resource sizes, cutoff, or publication. It is independent of the Event Job refactor; that branch should retain this predicate when rebasing.

Database regression tests cover live/package/source × cover/preview. Waiting validation retains priority; after the validation is claimed, optional work can use the free slot; both occupied slots reject another optional claim. All six cases failed before the correction and pass after it. Existing optional-media fencing, retention and cleanup tests also pass.

## Historical evidence and limits

The test worker image was `sha256:9807259023bb030a4a679f7f2e818c5fcca5ba859109e111fe038f15135b28b5`, Asset revision `asset-api--0000108`. CMS logs include rollback revision and `0000233`; one revision cannot represent the entire test.

CMS first auto-cover request time: 2026-10-18 22:57:13.319654335 Asia/Taipei. The original recording naturally reached server ready at 23:16:55.440655 and published at 23:19:04.322107. Current cover is ready and promoted. Historical Asset claim/attempt rows are not available to this task, so the reproduced scheduling defect is not proof of every missed historical claim.

Required read-only Asset evidence remains: cover `5936d0582dd0a9972bda2220d38b9bfd` created/attempt/finish timestamps, its claim IDs, preview completed sequence and claim times, and corresponding processing-slot/worker execution records. Do not reconstruct these from CMS poll observations.

## Separate 503 evidence

At 22:59:01, `broadcasts.watch` was rejected at Gateway with `admission_unavailable`, before CMS. Account's access-admission repository logged PostgreSQL SQLSTATE 53300 and access-admission 503 responses in the same second. This is a shared database-capacity incident, not a player decoder failure. The separate infrastructure investigation owns PG capacity changes.

At 23:01:38, live playback request `28faff0b838f609b1872fe72dad7b4e4` passed Gateway and failed in CMS in 118 ms with `capture_unavailable`. Existing logs do not identify the failing inner operation. Concurrent database exhaustion is a hypothesis for this request, not a proven exact cause. CMS diagnostics are delivered separately; no authorization bypass or blind retry is introduced.

## Notification evidence

Exactly one CMS outbox event exists for this recording: live-start event `424de02b-e05f-4c87-afe7-5adcba7994b2`, created 22:57:13.055114 and delivered downstream 22:57:14.391015, attempts=1. No VOD publication notification exists for the recording. Downstream delivery is not a device receipt timestamp.

## Minimal remaining acceptance after deployment

Do not repeat completed closeout, long recording, DVR or player suites. One short live session longer than five minutes can verify first auto cover while live, five-minute refresh retaining previous ready image, live timeline sprites, and successful desktop hover with no viewer heartbeat. After natural closeout compare live/VOD JPEG hashes. Record any 503 request ID and exact timezone-bearing time for server correlation. Database-capacity stability remains a separate prerequisite; this scheduling correction does not fix it.

Original recording and all production recordings remain protected. No plugin or wire change is required.
