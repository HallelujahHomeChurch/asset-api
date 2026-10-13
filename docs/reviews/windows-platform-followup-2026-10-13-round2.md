# Windows platform follow-up: October 13, round 2

This is the Mac investigation and targeted repair handoff for the immutable
[Windows report](https://github.com/HallelujahHomeChurch/hhc-obs-plugin/blob/36c4eed26d4106ea10ef2531a22f2c391ec689c7/docs/reviews/windows-platform-followup-2026-10-13.md).
Deployment and new Windows acceptance are separate evidence states.

## Fixed inputs and preservation

- Installed Windows plugin: `9c658aeb76e6fe9dc0b3b3ce37c7088a09e77e82`.
- Candidate ZIP SHA-256: `b598ac6fe45348de493da2ff69b80d82d19ed7108e9829ef017bba370df0a0e8`.
- Long recording: `8a43d04c-76d7-4504-843d-81f7689ca995`;
  capture/package: `688d785ae02be5980b693701856af1d4`.
- Short recording: `ba4b1f22-12e8-4d7f-b2c6-5608992d15a2`;
  capture/package: `5f0cdc71d88e695a6e44398b24482df3`.
- Investigated worker image:
  `sha256:10ebdab641b4e65c8ea264510ca485be709ae27791e36ef3922ba32f86483adc`.

Both records were inspected read-only. No Stop, seal, publish, retry-processing,
cover mutation, job start, or deletion was issued by this investigation.
Protected originals `e496a04e-4e2e-4dad-9269-63a610dd7c06` and
`e9c5ebb4-74d7-46e0-a7b4-e3730579fc8a`, and real meeting recordings, remain untouched.

## D: actual validation and publication timeline

All table times are **2026-10-13 Asia/Taipei (UTC+08:00)**. Application log
timestamps are used instead of Log Analytics ingestion time.

| Event | Time | Evidence |
| --- | --- | --- |
| Seal accepted | 21:02:30.022578 | Windows receipt; CMS uploadedAt |
| Attempt 1 claimed | 21:03:30.615725 | Windows processing projection |
| Attempt 1 log start | 21:03:30.646 | `recording_package_claim`, claim `b56033fbfb6735390ae965e62c54a402` |
| First attempt stopped | 21:04:40.669 | `claim_exit`, elapsed 70023 ms, stage `progress_flush`, reason `deadline` |
| Validation canceled | 21:04:40.669 | `validation_result`, stage `object_head_staging`, reason `canceled` |
| Attempt 2 claimed | 21:06:23.004398 | Windows processing projection |
| Attempt 2 log start | 21:06:23.029 | claim `affa9eb1771d44161809ab0bdfd3e6b7` |
| Finalization progress | 21:34:09.294 | 988 objects, 981 segments verified |
| Ready persisted | 21:34:09.650690 | CMS GET recording `readyAt` |
| Ready finish committed | 21:34:09.681 | `finish`, ready=true, committed=true |
| Automatic publication | 21:36:30.672244 | Central Audit `cms.recording.capture.publish`, success, event `f99b15b1-83ff-468d-a501-c207717fae12` |

Measured intervals:

- Seal to first claim: 60.593147 seconds.
- First claim to replacement claim: 172.388673 seconds.
- Attempt 2 claim to persisted ready: 27 minutes 46.646292 seconds.
- Seal to persisted ready: **31 minutes 39.628112 seconds**.
- Ready to automatic publication: **2 minutes 21.021554 seconds**.
- Seal to automatic publication: **34 minutes 0.649666 seconds**.

Windows observed ready at 21:34:14.157 and published no later than 21:39:47.609.
Those observation bounds remain valid; they are not the server transition times.
The short recording's server ready time is 17:58:27.866579 and its successful
automatic publication audit time is 18:01:29.595289 (event
`5fd90b70-c089-4993-a202-b7d3a7282b99`).

### First-attempt cause and focused repair

The heartbeat callback originally flushed display progress **before** renewing
the fenced processing lease, sharing a ten-second deadline. Its second tick
timed out writing progress and canceled validation. There was no committed
`finish` event; the later attempt is consistent with expired-lease reclamation,
not the normal five-minute `failure=retry` backoff. The canceled staging HEAD
was a downstream effect, not evidence that R2 or the media caused the failure.

The fix renews the fenced lease first. Only a display-progress deadline after
successful renewal is deferred until the next tick. Lease errors, ownership
conflicts, cancellation, and invalid progress remain failures. Final progress
flush and ready commit remain mandatory. Full validation, retry policy,
claim fencing, and immutable attempt output remain intact.

The SQL timeout's underlying cause (pool wait, row lock, database/network delay)
is **not established** by these logs. This repair prevents the demonstrated
display-progress timeout from unnecessarily restarting valid media work; it
does not claim to repair an unproven database bottleneck.

### Second-attempt work attribution

The observed rendition transitions were:

| Phase | Application log interval | Elapsed |
| --- | --- | --- |
| Initial controls | 21:06:23.029 to 21:06:25.776 | 2.747 s |
| 1080p validation | 21:06:25.776 to 21:20:16.983 | 831.207 s |
| 720p validation | 21:20:16.983 to 21:28:24.857 | 487.874 s |
| 480p validation | 21:28:24.857 to 21:34:09.294 | 344.437 s |
| Finalization to committed finish | 21:34:09.294 to 21:34:09.681 | 0.387 s |

Progress advanced throughout attempt 2; the 27-minute interval was active
package validation, not a 27-minute scheduler wait. Existing logs cannot split
its object copy/hash, probe, and decode durations. Bounded cumulative stage
timings are added for the next capture; overlapping worker durations must not
be summed and presented as wall-clock latency. No CPU/memory, parallelism,
media-validation tolerances, or infrastructure were changed.

## A–C: UI and ownership

### A: retained Console custom image

The selected custom image can return 404 while its retained reference is pending,
then become ready without changing the cover revision/ID. The image effect did
not depend on readiness and did not retry the failed content request. A fresh
page hid the issue by starting a new request. The repair observes readiness and
uses bounded transient-content retries, with abort/URL cleanup on scope changes.
Late responses must not replace an operator's current custom selection.

### B: recording-management cover-settings 404

B1 intentionally creates an underlying C1 capture with `Create.LiveEnabled=false`:
the broadcast owns its cover configuration. Recording management incorrectly
used the legacy capture-cover editor after reading the member-facing live
projection. The existing backend rejects that route correctly.

The UI checks the existing broadcast read endpoint while the live-cover panel is
needed. A confirmed B1 broadcast shows its mode/state and links to its Console;
only a confirmed non-broadcast 404 selects the legacy C1 editor. Transient/auth
errors cannot silently fall back to disabled Auto. Ready media uses the existing
VOD cover editor. No new discriminator, schema field, or SDK release is required.

### C: recording-management polling stops after a transient failure

The polling effect stopped after a rejected refresh, preserving stale draft and
progress indefinitely until manual reload. Shared authentication already handles
401 refresh; a remaining transient refresh/network/server failure still needs
page-level recovery. The fix adds bounded backoff and resumes successful polling
while preserving unsaved edits and rejecting stale responses. Terminal access or
resource errors stop retries. Exhausted retries retain a visible retry action.

The original Windows page's exact failed HTTP response is not retained, so its
precise network/auth trigger is unqualified. The permanent-stop path is
reproducible without guessing that trigger.

## Unchanged contracts and acceptance boundary

B1 wire `b1-2026-10-10.rc1`, document `b1-mac-2026-10-10.rc2`, C1
`c1-2026-10-08.2`, inventory schema 1, digest/idempotency, member authorization,
30000/1001 fps, 900-frame / 30.03-second interiors, and public
`[start,endExclusive)` remain unchanged. No Windows or CLI change is required.

The prior 379/388-second lag, 32.730-second uncached weak-network seek, normal
congregation plus YouTube load, reconnect/ABR/DVR/pause/rate preservation and
five-minute outage/ten-minute catch-up remain independent acceptance items.
Neither CI, deployment, nor this earlier field's final publication proves them.

## Targeted Windows follow-up

1. Confirm current test authorization and that no gathering is in progress.
2. Use a new disposable short capture. Keep its original Console and recording
   page open. Save a custom thumbnail before ready; observe the same-page image
   once ready without reload. Check B1 cover mode/Console routing, not C1 errors.
3. Allow one transient read failure during processing; verify bounded recovery
   to ready/published without manual reload. Verify terminal denial does not
   loop, and unsaved title/description/cover edits survive recovery.
4. Use nonzero Start and End before source Stop; verify unchanged public duration,
   thumbnail selection and final VOD actual decode/position on the original page.
5. A representative new long capture is needed to measure retry recurrence and
   stage costs. Keep real-media rules unchanged. Report exact seal/attempt/ready
   receipts and publication observations; Mac correlates safe stage logs and audit.

Once this handoff's diagnosis is complete, the two October 13 disposable short
and long recordings are no longer required by Mac and may be cleaned by Windows
under the existing cleanup authorization. Retain report/journal/hash evidence.
Do not delete the named protected originals or any actual meeting recording.

Release versions and CI/smoke results are supplied in the final handoff alongside
this fixed file's commit and SHA-256. New Windows acceptance is still pending.
