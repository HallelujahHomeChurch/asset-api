# K3 publication and ended-live replay follow-up

## Scope and evidence boundaries

Windows field plugin: `686dcfecba1a4e5cfc40995d40f219e32ff83964`.
Follow-up candidate: `33be386aed287098f23d8421acd0697887cf2c05`.
The candidate's local/CI success does not establish true OBS acceptance.

The first recording is `b7a8d3f4-54cc-4a69-88b6-ccda0d8b7136`, capture
`63163a8265f4f2cc31f97bfddec93665`, public `[0,14)`.
The successful comparison is `21edf722-0533-4081-a416-580ab69517f6`, capture
`fd6df71873a5d2a4ac0384a155611ea5`, public `[9,16)`, 210.21 seconds.
Neither event was manually published, recreated, cancelled, deleted or resealed.
No Windows media retransmission is required. Diagnostic access to existing
production media was read-only; generated diagnostic JPEGs were not uploaded.

Contracts remain K3 `k3-2026-10-14.rc1`, B1 `b1-2026-10-10.rc1`, C1
`c1-2026-10-08.2`, inventory 1, 30000/1001 fps, 900 frames / 30.03 seconds.
No wire, digest, idempotency, authorization, public boundaries or complete media
validation changes. C1 `autoPublish=cancelled` remains the existing B1 bridge
value, not proof that Console publication was cancelled.

## A: ready archive did not publish

Fresh readback confirmed first recording draft, archive ready, auto cover ready,
auto publication pending, inherited VOD cover not ready. There were six failed
publication attempts and no permission-check timestamp: the publication attempt
had not reached Account authorization. Both the ordinary automatic VOD derivative
and the inherited live-auto derivative had exhausted three attempts.
Worker logs matched their six claim times and reported `invalid upload`.

The public 14-segment timeline is 420.41999999999996 seconds. Its 50% sample
resolved to segment 6 at offset 30.029999999999973: only
2.842170943040401e-14 seconds remained before the next segment. JSON-rounded
boundaries and floating arithmetic classified the exact midpoint into an
exhausted fragment. No frame satisfied the FFmpeg sample filter, so the cover
failed. A ready livestream image alone is not a ready three-candidate VOD image.
The second event's samples lie inside fragments and therefore succeeded.

Read-only reproduction using the first event's actual ready R2 objects:

- Old implementation produced candidate 1 then failed `invalid upload`.
- Fixed implementation produced all three 1280 x 720 JPEGs.
- Candidate bytes: 64808, 64354, 64700. Diagnostic candidate 1 SHA-256:
  `a277e43df648799bcf6c0a2a9e6d87a4418cf93b4a6a8279098e010d40f51c38`.
- This is platform media evidence, not a Windows encoder/capture acceptance pass.

Fix: snap a cover sample within 1e-9 seconds of a measured boundary to the next
fragment. This tolerance applies only to optional image sampling; media validation
and declared timeline values remain unchanged.

Migration 041 grants one fresh bounded three-attempt budget only to failed auto /
live-auto derivatives with ready, retained, unrevoked B1 media and a matching
rounded sample boundary. It retains derivative IDs, operation keys and immutable
attempt history. Migration replay cannot grant another budget. Read-only
production preview selected exactly two rows, both belonging to the first event:
`2cd0d8346babb5c9391b5a0225b53dfe` and `7da9d43b51c5d9cb6f1db7462012c771`.
No media/capture/event/publication row is modified by this recovery migration.
The existing worker and CMS reconcilers perform the subsequent normal lifecycle.

CMS also clears the pending publication's stale retry deadline exactly when its
VOD cover is inherited as ready. It does not clear blocked/cancelled decisions,
reset authority, bypass the human-edit fence or directly publish. The existing
five-minute publication loop still independently checks media, cover and current
publisher authority and commits publication/audit/notification atomically.

### Exact existing timestamps (2026-10-14 Asia/Taipei UTC+8)

| Stage | First event | Successful second event |
| --- | --- | --- |
| Stop accepted | 10:56:44.095812277 | 11:11:17.152477929 |
| Seal accepted | 10:57:51.836961 | 11:12:21.308683 |
| Media ready persisted | 10:59:42.370251 | 11:13:59.451790 |
| Live cover ready SQL transaction timestamp | 10:52:53.566931 | 11:09:43.041186 |
| Inherited VOD cover ready SQL transaction timestamp | Not ready before repair | 11:16:20.173227 |
| Cover selection audit | None before repair | 11:16:43.180648186 |
| Automatic publication audit | None before repair | 11:17:43.477818008 |

Cover timestamps are reconstructed exactly from the persisted next-attempt timestamp
minus the five-minute interval set by the successful Finish transaction. No separate
cover finish wall-clock or commit timestamp was stored; these values identify the
SQL transaction time, not a more precise database commit instant.
The second event's seal-to-publication is 5m22.169135008s; ready-to-publication
is 3m44.026028008s. Windows's 11:20:16.797 observation is an upper bound,
not its publication time. The first event's post-release times and final state
must be read back from actual production; do not replace them with predictions.

## B: transient ended-live quality error

Windows observed a temporary error while 480p EOS -> Home -> 1080p / Auto
rebuilt media, followed by decode recovery without reload. There is no evidence
of permanent corrupt media, and sparse sampling does not establish precise switch
latency. Gateway history had no failed API request around the 11:16 sample.
Old browser-side native/HLS error event payloads were not retained; the exact
historical emitter cannot be reconstructed from video dimensions alone.

The player had no quality-reconstruction state: a decode error during deliberate
MediaSource replacement immediately froze controls and called the terminal error
callback. Reproduction tests establish that code path. The fix keeps a held frame
and accessible loading indicator until the replacement has decoded media, keeping
position / pause / rate / manual quality / DVR intent. A decode error observed
during reconstruction has a bounded 30-second grace period; persistent failure
still reports. Network and authorization failures remain immediate. Slow buffering
without a decode error does not acquire a new time limit. Rapid quality changes
retain the latest intent and held frame, and source teardown cancels the timer.

## Targeted verification and remaining Windows work

Platform tests cover rounded boundaries, real 29.97 fMP4 decoding of three
candidates with inherited first-image byte identity, migration scope and replay,
ready-cover publication wakeup, native/HLS reconstruction recovery, permanent
failure, immediate fatal network errors and slow buffering without false failure.
Full CI and immutable deployment proofs belong to the completion report for these
PRs; no deployment or Windows acceptance is inferred from source/test success.

1. Read the existing first event; do not resend media/Stop/seal or manually publish.
   Confirm natural selected cover and archived/published status. Compare live and
   selected VOD cover bytes, title, URL and public range. Retain historical failure.
2. Existing second event is VOD; its expired live replay cannot test ended-live
   rebuilding. One new disposable short K3 capture is needed for that path. Before
   actual OBS, check current meeting times and that authorization remains valid.
3. Keep original Console/member pages mounted. End, then within the replay deadline
   and before switching to VOD: 480p natural EOS -> Home -> paused 1080p -> Auto;
   also test nonzero position and 1.5x pause/play. Record decoded dimensions,
   readyState, MediaError/HLS error category, currentTime, duration, pause/rate,
   loading/held-frame/alert transitions and times. Never report badge-only success.
4. Publish should proceed naturally with complete media and a selected ready cover.
   If it publishes before replay reproduction, use a disposable test event with
   autoPublish off at creation; do not change/cancel the retained existing events.
5. Session expiry, revocation and process interruption/local recovery may continue
   independently. Normal meeting + local recording + YouTube recovery, weak-network
   ABR/DVR, 5-minute outage/10-minute catch-up pressure and current 2.5-hour testing
   remain separate acceptance tasks. Old 379/388-second delay and 32.730-second
   weak-network seek results remain failures/limitations, not superseded passes.

Retain both original events until Windows has confirmed byte identity and final
state; this task does not authorize deletion of protected/meeting recordings.
No keys, tokens, cookies, passwords or signed URLs belong in handoff evidence.
