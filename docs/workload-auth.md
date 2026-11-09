# Internal caller identification and trusted bulletin derivatives

Asset uses internal-only, HTTPS-only ACA ingress. All workloads in the same
environment are trusted; only Gateway is externally reachable. ACA terminates
TLS before the app. `X-HHC-Caller-App-Id` identifies the trusted service and must
contain exactly one allowlisted service string. Duplicate, empty, whitespace,
comma-separated, unknown, and mixed Dapr/header identities fail closed.
`X-MS-CLIENT-PRINCIPAL` never identifies a caller. Easy Auth is explicitly
disabled through `authConfigs/current`; omitting that resource would leave the
existing proxy enabled.

Private `/priv/*` routes retain their configured service allowlist. Gateway
uses `api-gateway` on `/_gateway/api/assets/...`, which dispatches to the existing
reader handlers without granting access to service-owned private routes.
Member identity, scopes, resource ACL, grants and ticket checks remain intact.
The public Gateway must strip caller identity supplied by clients and construct
its own service header.

The bulletin extractor identifies as `hhc-web-api`. LINE API and attachment
jobs identify as `hhc-line-function-bot`. The watermark worker identifies as
`hhc-web-bulletin-worker`; only its ownership is aliased to `hhc-web-api`, while
its original service identity gates derivative completion.

## Temporary Dapr compatibility

A request without the internal header can still use an exact app-channel token
and Dapr caller identity. Local-only `ASSET_ALLOW_DEV_CALLER_HEADER` remains a
development convenience. Without Dapr, production still requires direct HTTPS
Audit dispatch. With the workload verifier off, legacy admission requires no
Entra identity settings. Managed identities for Blob, queues, Key Vault and
image pulling retain their separate responsibilities.

## Audience-bound workload verifier

`ASSET_WORKLOAD_AUTH` is unset by default. It accepts a single JSON object
when explicitly configured. The following values are synthetic examples.

```json
{
  "issuer": "https://issuer.example/workload-issuer",
  "audience": "asset-fixture",
  "jwksUrl": "https://issuer.example/workload-keys",
  "principalClaim": "oid",
  "principals": {
    "cms-fixture-object": "hhc-web-api",
    "extractor-fixture-object": "hhc-web-extractor",
    "watermark-fixture-object": "hhc-web-bulletin-worker",
    "gateway-fixture-object": "api-gateway"
  },
  "disableLegacyCallers": []
}
```

Configure one exact trusted issuer and its trusted HTTPS signing-key endpoint
after verifying the issuer metadata. The verifier never fetches token-supplied
URLs or follows redirects. Choose `oid` or `sub` only after confirming the
issuer's actual app-only principal claim. Each known principal maps to one
existing caller alias. Distinct principal IDs remain separate even if they
share a compatibility alias. Caller, stored owner, end-user actor and audit
provenance retain separate meanings. The watermark worker keeps its original
caller identity and the existing CMS owner delegation.

Workloads send exactly one `X-HHC-Workload-Authorization: Bearer <token>`.
The verified RS256 token must have the exact issuer and single audience,
valid required expiry and not-before, and an admitted principal. Delegated
`scp` tokens and explicit non-app identity types are rejected. The ordinary
end-user `Authorization` header is separate. A matching single legacy HTTPS
alias is optional. Mixed Dapr or development credentials, mismatched aliases
and invalid tokens fail closed, including when the verifier is off. A token
is verified at private ingress or at the protected Gateway reader ingress.
The direct Dapr reader fallback cannot accept a mixed workload token.

Legacy callers continue through the same authorized routes. Add an exact
compatibility alias to `disableLegacyCallers` only after that caller's workload
verification and rollback are proven. This disables its legacy HTTPS and Dapr
admission. The separate Gateway alias remains confined to its reader routes.
A missing workload token during dual acceptance follows legacy admission.
Generic derivation, owner reads and other new privileged capabilities have no
admission through this verifier change.

The verifier loads public keys on demand. Fetches have a two-second deadline,
a 256 KiB response limit, a five-minute cache lifetime and a shared 30-second
refresh interval across misses and failures. A new key can take up to one
refresh interval to be recognized. Still-fresh known keys remain usable during
an outage. Failed refresh with no valid key returns a sanitized 503. Invalid
credentials return 401 and authenticated unknown principals or wrong route
permissions return 403. Responses exclude tokens, keys and upstream details.

Keep the runtime verifier unset until Gateway stripping, authenticated caller
claims, owner mapping and dual-mode rollback have been verified. Local fixtures
use an isolated issuer and keys. They do not certify production identity
readiness. Existing Dapr fallback stays available for the permitted aliases.


## Verified bulletin extractor

A verified `hhc-web-extractor` retains its own service identity and uses CMS
storage ownership for the existing zh-Hant bulletin flows. It can read source
PDF metadata, create its own service read grant for clean source bytes with an
expiry of at most one hour, download through that grant and revoke that exact
grant. Other grant subjects and permissions are rejected. Revocation is scoped
by asset, grant, service subject and read permission in the database.

The extractor can create, complete and inspect its private `cms.weekly.image`
PNG uploads for `bulletin_issue` and `online_bulletin_illustration`. Existing
2 MiB and ordinary scan limits apply. Other CMS asset namespaces and actions
remain outside this delegation. Header-only and Dapr callers cannot assume the
new alias. Keep the current legacy extractor identity until its real token and
both bulletin flows are verified.

## Trusted bulletin derivative completion

Only `hhc-web-bulletin-worker` may call
`POST /priv/assets/bulletin-derivatives/{assetID}/complete`. The original must be
an immutable clean private bulletin with matching SHA-256 and locale. Output
ownership, MIME, size, checksum and immutable Blob commit remain validated.
This records `trusted-bulletin-watermark-v1` provenance without claiming a new
ClamAV scan. Browser uploads and ordinary completion retain the scan lifecycle.

## Ordered rollout and rollback

1. Release the producer compatibility image accepting both the new service
   header and old authenticated principal.
2. Release every HTTPS caller with the new header alongside its existing token.
3. Verify that compatible Asset revision is serving and older replicas drained.
4. Release this removal of principal inference and explicit proxy disablement.
5. Remove obsolete caller token acquisition after the cutover is verified.

The app-level authentication switch affects all revisions immediately. Never
perform step 4 while a pre-header Asset image can serve. After cutover, roll
back only to the header-compatible image; restoring an older image requires a
separate coordinated restoration of its prior authentication configuration.
The release retains storage/queue identities and the existing app/worker
rollback path. No database, media, retention, or permission migration is needed.

## Upload profile pairs

Omitting both `securityProfile` and `processingProfile` selects the existing
namespace policy. Legacy upload fingerprints and response fields stay compatible.
An explicit pair must match the existing mapping for a known namespace and a
scope configured for the verified principal. Unknown or partial pairs, malformed
namespace names, and known namespace mismatches return 400. A valid pair
without the verified principal's approved scope returns 403.

The initial registry preserves the existing MIME, byte, visibility and processing
limits. Only these complete pairs are admitted.

| Existing namespace | Security profile | Processing profile |
| --- | --- | --- |
| `account.avatar` | `avatar-jpeg-v1` | `no-processing-v1` |
| `cms.weekly.pdf` | `bulletin-pdf-v1` | `no-processing-v1` |
| `cms.weekly.image` | `bulletin-png-v1` | `no-processing-v1` |
| `cms.news.cover`, `cms.page.image` | `web-image-v1` | `image-variants-v1` |
| `cms.home.banner` | `home-banner-jpeg-v1` | `no-processing-v1` |
| `line.group.file` | `line-attachment-v1` | `no-processing-v1` |
| `line.group.media-sync` | `line-media-v1` | `no-processing-v1` |
| `presenter.personal` | `personal-media-v1` | `no-processing-v1` |

The optional `uploadProfileScopes` object in `ASSET_WORKLOAD_AUTH` maps an exact
verified principal ID to approved family/pair entries. For example, a synthetic
CMS fixture could allow `cms.page` with `web-image-v1` and `image-variants-v1`.
That matches `cms.page.image` and an approved new purpose such as `cms.page.hero`.
A similarly named `cms.pages.hero` falls outside that scope. Asset checks the
stored service owner separately.

Unmapped namespaces initially belong only to CMS within approved dotted
families. Account cleanup and LINE retention bind their existing exact
namespaces, so their explicit profiles can be scoped to those known namespaces.
Personal-space uploads keep the existing policy path. This change does not
expand the audited upload tuple catalog. A new audited purpose needs its own
reviewed audit contract before rollout.

Migration 050 stores explicit paired IDs without backfill. Completion, public
grant and public metadata resolve the same persisted pair. Roll out the producer
migration and compatible readers before sending explicit fields. Once a new
namespace has live assets, retain a rollback artifact that understands its
stored pair and namespace. Keep all affected Asset writers and scanner or
derivative jobs on the compatible minimum while those scopes are enabled.
Disabling new admission cannot migrate existing assets into the legacy
namespace catalog.

`ASSET_HOME_BANNER_DIMENSIONS_DELEGATED` defaults to false. Legacy unprofiled
Home Banner uploads always keep the existing Asset 1900×700 check. A profiled
Home Banner can defer the check to CMS only when the flag is enabled. Enable
that path only after the CMS release independently validates the immutable
scanned JPEG before public grant, its profiled upload path is approved, and
older CMS writers drain. Asset retains JPEG, size, scan and processing limits.

When the flag is false, a public read grant for a profiled Home Banner rechecks
the immutable bytes. This covers already-completed banners admitted while CMS
owned the dimension check. To roll back CMS to a build without its check, first
restore the flag-off Asset guard in an AS3-aware artifact and verify it serves.
A pre-AS3 Asset artifact cannot apply this grant guard. Roll back further only
after delegated assets have been handled through a separately reviewed recovery.

### Verified owner content

`ownerContentPrincipals` defaults to an empty list. Each configured ID must
resolve to the existing CMS service alias. Only those exact verified principals
can call `GET` or `HEAD /priv/assets/{assetID}/owner-content`. Set scopes only
after the workload identity readiness gate and the CMS caller checks pass.

The caller supplies the exact SHA-256, owner type and owner ID. Asset serves
private, completed, clean and processing-ready original bytes in the approved
`cms.weekly.image` and `cms.weekly.pdf` namespaces for `bulletin_issue` owners.
That scope excludes collection and retention-managed media, download-job
outputs and other owners. CMS checks the issue, locale and purpose association
before requesting bytes.

Asset binds the Blob read to the recorded ETag and rechecks the same immutable
object and current access state before sending the body. HEAD and a matching
conditional request use current metadata. Range uses the same authorization.
Responses use `private, no-store`. Missing, deleted, purged, quarantined and
mismatched objects return 404. Dependency failures return a sanitized error.
The path creates no grants and exposes no Blob credentials. To disable it,
remove the principal scope. The existing narrow illustration adapter remains
available under its original authorization rules.
