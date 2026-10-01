# Member video operations

## Gated HLS package path (not yet activated)

The new Pages route is
`/videos/{recording}/packages/{package}/sessions/{scope}/{object}`. Only
`master.m3u8`, `720p|1080p/index.m3u8`, `init.mp4` and `seg-NNNNNN.m4s`
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
checks pass. The following legacy MP4 procedures remain applicable only until
that separately reviewed cutover; ordinary Blob scanning stays unchanged.

## Existing MP4 path

The recording path is isolated from Blob uploads and ClamAV. It accepts only
MP4 candidates through private R2 multipart sessions, validates SHA-256 and
media format, and serves ready versions through the Pages media function. Format
validation is not malware scanning. Keep the bucket private and disable public
`r2.dev` access.

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

- Failed/resumed upload: have the Admin reselect the identical original MP4;
  the browser reuses its session and checks completed parts. Different bytes
  require a new recording, never another session on the original recording.
  A failed/cancelled session and its R2 parts are
  cleaned by the recording worker; inspect session status without logging
  presigned URLs.
- Validation failure: inspect structured status and worker errors, not signed
  `ffprobe` URLs or video bytes in logs. Re-encode to H.264/AAC fast-start MP4
  locally and create a new recording. Each attempt is bounded to 15 minutes;
  the claim lease and scheduled Job allow cleanup after that deadline. Do not
  bypass format or hash checks.
- Files cannot be replaced. Unpublishing stops new grants; existing scoped
  playback cookies can remain valid for up to one hour. Do not promise instant
  revocation. Editing or republishing never extends the original deadline.
- Cleanup: CMS denies new grants at uploadedAt + 720 hours and retries deleting
  completed Asset files every 15 minutes. The Asset worker owns incomplete
  24-hour session cleanup, serialized with completion. A committed R2 object in
  completing state is recovered into validation instead of deleted. Completion
  time is persisted once; HHC reconciles missing completion metadata every five
  minutes. Deleted rows retain immutable recording/file identity.
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
  without changing uploadedAt or its deadline. Ordinary reupload creates a new
  recording. R2 is not the original recording backup. A past-deadline recording
  must stay unavailable.

Monitor playback/exchange 5xx, upload/validation failures, grant issuance,
Pages request latency, abandoned/expired deletion age, R2 storage and request
counts. Configure test notifications and the approved monthly-cost alerts
before enabling members. A CI pass or successful dry-run is not a live smoke
test.
