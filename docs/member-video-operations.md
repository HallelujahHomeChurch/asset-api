# Member video operations

The recording path is isolated from Blob uploads and ClamAV. It accepts only
MP4 candidates through private R2 multipart sessions, validates SHA-256 and
media format, and serves ready versions through the Pages media function. Format
validation is not malware scanning. Keep the bucket private and disable public
`r2.dev` access.

## Activation gate

Do not turn on the recording configuration until the separate test and
production R2 buckets, Pages projects and custom domains, CORS policies, least-privilege R2 API
tokens, signing key pair, Key Vault references, and recording-validation
Container App are reviewed and provisioned. The API needs
`ASSET_R2_ACCOUNT_ID`, `ASSET_R2_BUCKET`, `ASSET_R2_ACCESS_KEY_ID`,
`ASSET_R2_SECRET_ACCESS_KEY`, `ASSET_MEDIA_PRIVATE_KEY_PEM`,
`ASSET_MEDIA_KEY_ID`, and `ASSET_MEDIA_ISSUER`. The validation worker needs
`DATABASE_URL` and the four R2 settings. Each Pages media function needs the matching
`MEDIA_ISSUER`, `MEDIA_PUBLIC_KEYS` JSON map, `ALLOWED_ORIGINS`, and private
`MEDIA_BUCKET` binding. Never place signing or R2 secrets in browser config,
logs, Git, or workflow output. Apply the matching
`workers/member-media/r2-cors.{test,production}.json` to each bucket only
after reviewing exact Admin origins.

Build the single media source with `npm ci && npm test && npm run typecheck && npm run build`
in `workers/member-media`. The build creates separate ignored Pages output at
`pages/test/dist` and `pages/production/dist`; deploy from the corresponding
project directory so its `wrangler.jsonc` is the configuration source of truth.
The test project is `hhc-member-media-test` with `hhc-member-recordings-test`,
`hhc-media-test`, and `https://www-test.alive.org.tw`; production uses
`hhc-member-media`, `hhc-member-recordings-prod`, `hhc-media-prod`, and
`https://www.alive.org.tw`. Set `MEDIA_PUBLIC_KEYS` separately as a Pages secret
in each project; never commit it. Do not publish preview branches with production
bindings or assume a Pages preview alias can serve an external-DNS custom domain.

Provision and verify the private test R2 bucket, then test Pages project, binding,
secret, custom domain, and only the `media-test.alive.org.tw` CNAME in Azure DNS
pointing at the actual test project hostname. Exercise real upload, exchange,
GET/HEAD/Range, expiry, denial, and HTTPS certificate before repeating the same
sequence for production. The production DNS change is only the new `media`
CNAME; do not change NS, apex, existing records, wildcard, or CAA. Resolve
record-set conflicts and inspect the exact DNS diff before either apply.
Keep member access disabled until the production smoke and cost alerts pass.
Rollback by disabling new grants and reverting the Pages deployment; DNS rollback
removes only the newly added `media`/`media-test` record after verifying its
ownership. Keep the R2 objects private throughout rollback.

Release order: audit catalog, Account permission catalog, Operations
entitlement, Asset API and validation worker, Pages media function, HHC Web API,
gateway, versioned frontend client, Admin, website. Each repository uses its
own PR, CI, approved release, revision verification, and rollback. The
Asset API release conditionally deploys the recording-validation Job and Pages
function when `MEMBER_VIDEO_ENABLED=true`. Review its infrastructure diff and
provision the private Cloudflare resources first. A failed media smoke rolls
back to the previous Pages deployment; the first deployment is allowed only
before a custom domain is attached. Never deploy a local unmerged build to
production. This is a hard activation gate.

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
