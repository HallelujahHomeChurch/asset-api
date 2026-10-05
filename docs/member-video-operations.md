# Member video operations

## Gated HLS package path (not yet activated)

The new Pages route is
`/videos/{recording}/packages/{package}/sessions/{scope}/{object}`. Only
`master.m3u8`, `720p|1080p/index.m3u8`, `init.mp4`, `seg-NNNNNN.m4s`,
`previews/index.vtt` and `previews/seg-NNNNNN.jpg`
are served. Inventory, arbitrary keys, query credentials and encoded paths are
not media routes. Package cookies contain a signer-authorized immutable final
prefix, never an upload/staging prefix. Exchange and renewal preserve the
package/session cookie path; the player does not need a new source URL.

Authorization runs before every R2 read and internal cache lookup. The cache key
uses the immutable final object identity and omits viewer cookies/session IDs;
cached bytes can be shared internally only after each viewer is authenticated.
All viewer responses remain `private, no-store`. Full successful GETs may be
cached internally for one day; Range responses are not inserted. Cache failure
falls back to private R2 without bypassing authentication. HEAD and conditional
or Range hits remain protected. Cache expiry does not extend playback grant or
recording retention. The synthetic cache path itself is not publicly routable.
See [Cloudflare Cache API](https://developers.cloudflare.com/workers/runtime-apis/cache/)
for local-cache and Range behavior. Edge placement/cache-hit ratio and charges
still require production measurements.

Local checks include the signed-cookie/R2/Cache flow in Wrangler's pinned
Cloudflare runtime, but are not deployment or browser/device acceptance.
Keep `ASSET_RECORDING_HLS_ENABLED` off until CMS, Gateway and player cutover
checks pass. Ordinary Blob scanning stays unchanged.

## Seek preview contract

The optional VTT and JPEG routes use the exact playback Cookie, scope, expiry,
CORS, GET/HEAD/Range and cache authorization as HLS, including on cache hits.
MIME types are `text/vtt` and `image/jpeg`; each object is capped at 1 MiB.
The standard WEBVTT cues sample every five seconds. Each 30-second media
fragment has one 960x90 JPEG with six horizontal 160x90 cells, referenced as
`seg-000000.jpg#xywh=0,0,160,90` relative to `previews/index.vtt`. Final cues end
at the recording duration and never reference unpopulated tail cells. The VTT
is published only after every sprite. A 404 means previews are not available;
it does not change video readiness or publication.

The scheduled recording Job selects the lowest-resolution validated rendition,
decodes one bounded init+fragment at a time with FFmpeg file-only protocols,
resets fragment PTS, and emits only preview JPEGs. It never downloads the full
package/original or re-encodes source video. Scratch is at most two bounded
media objects plus one 1 MiB JPEG; existing CPU 4 / memory 8 GiB stay unchanged.
Preview claims share the existing two global slots, renewable fenced leases,
and 5.5-hour deadline. Waiting upload validation and source jobs take priority.
Migration 031's default pending state includes existing ready packages; three
attempts total include crashes, with five-minute retry delay and no work after
media expiry. Failed previews leave `state=ready` untouched.

Storage is server-only under the immutable final prefix:
`previews/{preview-claim}/{index.vtt|seg-NNNNNN.jpg}`. A bounded private
`previews/current.json` pointer selects the completed attempt. Publication holds
the shared slot and package lease locks; all writes use conditional create-only
PUTs. Thus an ambiguous/stale write cannot overwrite an existing winner. The
Worker resolves this pointer after authentication and validates its attempt ID;
neither internal attempt paths nor the pointer are routable. No upload inventory
or client signing path accepts these keys. At most three attempts are retained
until media expiry plus the existing one-hour grant grace, then daily declared
key sweeps remove all derived objects, including late writes. Attempts are kept
through expiry to preserve a successful pointer whose DB response was lost.

After release, verify an older ready package progresses to preview ready, a new
upload remains playable before previews finish, and real authenticated VTT/JPEG
GETs work. Recheck unauthenticated, cross-scope, expired-cookie and cached denial,
final-tail seek behavior, Job slots/resources/duration, and expiry cleanup.
Local tests and migration success do not establish production backfill or
browser/device acceptance. Roll back API/Job and Pages using their existing
release paths; the additive schema and optional objects remain compatible.

## Retired single-file path

Single-file recording routes and multipart helpers are removed. Old media URLs
return 404 even with a valid old grant. No MP4 migration or playback fallback is
provided. The empty legacy database table remains solely for release rollback;
do not drop it while old revisions remain rollback candidates. Original browser
sources and HLS initialization fragments can still be MP4. Format validation is
not malware scanning. Keep the bucket private and disable public `r2.dev` access.

## Activation gate

Do not turn on the recording configuration until the production private R2
bucket, Pages project and custom domain, CORS policy, least-privilege R2 API
token, signing key pair, Key Vault references, and recording-validation
Container App are reviewed and provisioned. The API needs
`ASSET_R2_ACCOUNT_ID`, `ASSET_R2_BUCKET`, `ASSET_R2_ACCESS_KEY_ID`,
`ASSET_R2_SECRET_ACCESS_KEY`, `ASSET_MEDIA_PRIVATE_KEY_PEM`,
`ASSET_MEDIA_KEY_ID`, and `ASSET_MEDIA_ISSUER`. The validation worker needs
`DATABASE_URL` and the four R2 settings. Each Pages media function needs the matching
`MEDIA_ISSUER`, `MEDIA_PUBLIC_KEYS` JSON map, `ALLOWED_ORIGINS`, and private
`MEDIA_BUCKET` binding. Never place signing or R2 secrets in browser config,
logs, Git, or workflow output. Apply
`workers/member-media/r2-cors.production.json` to the production bucket only
after reviewing exact Admin origins. Existing empty test resources are not
activation evidence and remain untouched.

Build the single media source with `npm ci && npm test && npm run typecheck && npm run build`
in `workers/member-media`. The production output is
`pages/production/dist`; the release workflow deploys this verified artifact
from the production project directory so its `wrangler.jsonc` is the
configuration source of truth. Production uses `hhc-member-media`,
`hhc-member-recordings-prod`, `hhc-media-prod`, and
`https://www.alive.org.tw`. Set `MEDIA_PUBLIC_KEYS` as a Pages secret; never
commit it. Do not publish preview branches with production bindings.

Provision and verify the private production R2 bucket and Pages project without
a custom domain. Set the binding and secret, then enable
`MEMBER_MEDIA_DEPLOY_ENABLED=true` while `MEMBER_VIDEO_ENABLED=false` and run
the standard release workflow. Its first-deployment guard requires no custom
domain, and unauthenticated GET/HEAD must return 401 before DNS is attached.
After a fresh DNS preview, add only the new `media.alive.org.tw` CNAME pointing
at the actual production Pages hostname. Do not change NS, apex, existing
records, wildcard, or CAA. Keep member access disabled until authenticated
upload/playback, denial, expiry, HTTPS, and cost checks pass. Rollback by
disabling new grants and reverting the Pages deployment; remove only the newly
added `media` record after verifying its ownership. Keep R2 objects private.

Release order: audit catalog, Account permission catalog, Operations
entitlement, Engagement notification source, Asset API, HHC Web API, gateway,
versioned frontend client, Admin, website—all dark—then private Cloudflare
media resources and staged activation. Each repository uses its own PR, CI,
release, revision verification, and rollback. `MEMBER_MEDIA_DEPLOY_ENABLED`
controls Pages deployment independently; `MEMBER_VIDEO_ENABLED` controls the
Asset R2 runtime and recording-validation Job. Both default off. Review the
infrastructure diff before enabling either. A failed media smoke rolls back
to the previous Pages deployment; the first deployment is allowed only before
a custom domain is attached. Never deploy an unmerged local build. This is a
hard activation gate.

Use a distinct merged commit for each staged production release. The governance
artifacts are immutable per commit: rerunning the same commit is only a retry
when it resolves to the original image digest. Changing a repository variable
and rebuilding the same commit can deploy a different digest, after which the
governance publisher must reject the conflicting provenance. Land the reviewed
activation/configuration change first, then release that new commit.

## Routine and incident handling

- Failed/resumed upload: have the Admin reselect the identical original source;
  the browser reuses its session and checks completed Blob blocks. Different bytes
  require a new recording, never another session on the original recording.
  A failed/cancelled session and its staged bytes are
  cleaned by the recording worker; inspect session status without logging
  presigned URLs.
- Validation failure: inspect structured status and worker errors, not signed
  source URLs or video bytes in logs. CLI packages require H.264/AAC fMP4 HLS.
  Browser source processing can be retried at most three times without extending
  source retention. Each processing attempt is bounded to 5.5 hours;
  the fenced claim lease allows recovery after a crash. Do not
  bypass format or hash checks.
- Files cannot be replaced. Unpublishing stops new grants; existing scoped
  playback cookies can remain valid for up to one hour. Do not promise instant
  revocation. Editing or republishing never extends the original deadline.
- Cleanup: CMS denies new grants at the earlier of unpublish time and readyAt
  + 30 days. The Asset worker owns HLS expiry, one-hour existing-grant grace,
  incomplete 24-hour session cleanup and repeated late-write sweeps. Browser
  sources are removed after success; failed sources expire seven days after the
  first durable completion. The independent Blob lifecycle is a nine-day fallback.
  Metadata receipts retain immutable recording/package identity.
  Investigate cleanup lag over one
  hour; escalate at 24 hours. Never extend retention by reuploading.
- Key rotation: add the new public key to `MEDIA_PUBLIC_KEYS`, release the
  Pages function, then switch the Asset signer/key ID. Keep the old public key for at
  least the longest live playback grant plus clock margin (65 minutes here),
  then remove it. Roll back signer first if exchange errors rise.
- Emergency stop: set the Pages function `DENY_ALL=true` to reject new media reads, and
  disable new grants at the API/gateway. This interrupts viewers. Rotating a
  signing key alone does not retract bytes already delivered to a player.
- Recovery: restore CMS metadata through existing database backup procedures;
  recover the existing object/version only through a reviewed operational repair
  without changing readyAt or its deadline. Ordinary reupload creates a new
  recording. R2 is not the original recording backup. A past-deadline recording
  must stay unavailable.

Monitor playback/exchange 5xx, upload/validation failures, grant issuance,
Pages request latency, abandoned/expired deletion age, R2 storage and request
counts. Configure test notifications and the approved monthly-cost alerts
before enabling members. A CI pass or successful dry-run is not a live smoke
test.

### Recording health signals

The recording Job emits `msg=recording_health` at each execution before cleanup,
with `package_cleanup_overdue`, `source_cleanup_overdue`, `waiting` and
`active_slots`. These are aggregate counts only, with no identities, filenames,
keys or signed URLs. Cleanup counts use the same eligibility predicates as the
reconcilers and mean a due sweep is over one hour late; they are not a claim that
every byte remains present. Repeated successful sweeps move the next check forward.
The query is bounded to five seconds; failure emits `recording_health_unavailable`
without blocking processing. Keep this Job's `LOG_LEVEL=info` for heartbeat alerts.

Alert definitions belong to the shared `azure-infra` observability configuration:
review a create-only Terraform plan before enabling notifications. Monitor a
15-minute missing heartbeat, worker errors, persistent cleanup backlog, and the
isolated `aliverecordingsprod` account's `UsedCapacity`. Do not apply scan-queue
alarms to these recordings: their source pipeline does not run antivirus.

On a health alert, inspect Job execution status first, then private session state.
Do not paste raw worker errors into chat: inspect in the restricted log console.
Compare consecutive health snapshots before replaying cleanup; never edit states
or deletion deadlines manually. Missing heartbeat can indicate scheduling, image,
database or logging failure; absence of errors alone does not prove health.

Azure storage capacity is actual provider usage; HLS metadata sizes are not a
replacement for billing metrics. Track this source account and the recording Job
separately in Cost Management. Track R2 storage/Class A/Class B and Workers paid
usage in Cloudflare, including the base plan as feature cost. Cost observations,
alert delivery tests, and positive member playback remain separate acceptance gates.
# Recording cover operations

Recording covers use private R2 `recordings/covers/{recordingId}/{attempt}/`,
outside immutable HLS inventory. The existing recording Job shares its two DB
slots with validation/source processing; covers defer to waiting video work.
Each image job has a two-minute deadline, three-minute fenced lease, at most
three attempts and a five-minute retry delay. Failure never changes HLS ready.
New ready packages enqueue three candidates at 20/50/80 percent. Custom inputs
are limited to 5 MiB JPEG/PNG, 24 MP and 8192 px per edge; non-normal EXIF,
animation and non-16:9 custom input are rejected. Isolated FFmpeg and JPEG
re-encoding produce metadata-free 1280×720 images no larger than 1 MiB.
No normal Blob asset, scan event or new container is created.

Only CMS can use the private cover endpoints documented in OpenAPI. CMS owns
selection and viewer authorization; image reads never grant video access.
Retain a ready image with a unique selection reference before committing its
CMS pointer. Release obsolete/failed selection references durably; do not
delete a pointer's reference on an ambiguous CMS commit. Cover jobs, attempts
and reference metadata are operational receipts, not a legal erasure policy.

Cleanup deletes accepted custom inputs immediately, and retries input/output
deletion through the existing Job. Unselected custom images expire after
24 hours; references preserve selected output only until the recording expires
or is deleted. Abandoned attempts receive a six-hour stale-writer grace.
Repeated sweeps catch late R2 writes and retry provider failure; failed items
rotate so they cannot starve later cleanup. Expired images fail closed before
provider deletion. R2 failure is not evidence of completed byte removal.

Existing recordings require an explicitly reviewed backfill:

Management lists preserve expired upload metadata until the owning recording
expires, so interrupted CLI operations can distinguish an expired attempt from
an unknown upload. This does not permit reading or retaining expired bytes.
The existing 20-upload daily quota and 30-day recording lifetime bound these
receipts; all consumers accept at most 1,000 cover items.

```sh
go run ./cmd/recording-cover-backfill --limit 20
# After approval of the exact page; DATABASE_URL must target the reviewed DB:
go run ./cmd/recording-cover-backfill --limit 20 --apply
# For another reviewed page, pass its prior nextAfter cursor with --after.
```

Dry-run is the default; each page is bounded to 100. Replays skip existing
auto jobs, and deleted/expired packages are excluded. This only enqueues image
work: it never changes the recording selection, HLS inventory, publication,
notifications or expiry. Do not run production apply without explicit approval.

Acceptance must separately prove an authenticated pre-play cover read, custom
upload/selection, replacement, CLI resume and recording deletion. Local tests
do not establish production R2 permissions or successful cleanup. Observe cover
state/attempt counts and retry timestamps without logging images or credentials.
