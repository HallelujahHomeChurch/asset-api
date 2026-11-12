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

`POST /priv/assets/upload-policy` is a read-only preflight for an explicit
`namespace`, `securityProfile` and `processingProfile`. Only verified workloads
with the matching configured scope and storage owner can query it. Header-only
or Dapr legacy callers cannot use this new route. It creates no upload, grant or
scan work and does not authorize later operations. The version 1 response reports
the registry's MIME/size/visibility limits and readiness version; ordinary
completion always reports `scanRequired: true`, including `no-processing-v1`.
Privileged trusted-derivative completion remains a separate contract. Unknown or
partial pairs return 400; unapproved scopes return 403. Consumers must reject
unknown contract versions.

The dedicated `hhc-line-harness` verified producer may use the exact
`line-source-unscanned-v1` / `no-processing-v1` pair in `line.group.file` or
`line.group.media-sync` only when its principal has that exact configured scope.
The producer is absent from default allowed callers. Header-only/Dapr identity
is always rejected for it. No legacy caller or other pair inherits this policy.
Its preflight/readiness version is 2 and `scanRequired` is false. Completion
validates actual bytes, MIME, size, checksum and immutable commit; it records
`scanStatus: not_required` and `safetyEvidence.kind: not-scanned`, never clean.

Creation requires `source: {ref, revision, publishedAt}`. Ref/revision are opaque
12–128-character URL-safe identifiers, not LINE IDs, URLs or filenames. The
persisted version 1 `sourcePolicy` adds `expiresAt = publishedAt + 2160 hours`.
Future or expired publications are rejected; completion also checks the hard
deadline. Old pairs reject source fields. The deadline participates in immutable
upload identity and is independent of collection retention. Private owner
metadata remains available for recovery; technical readiness is not source
access authorization. All source-aware content/publication paths must be in
place before enabling producer scopes or new intake. Old readers continue to
deny the new scan state; rollback to an incompatible writer is prohibited.

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

### Verified completion and readiness

`trustedDerivationPrincipals` defaults to an empty list. It accepts only exact
verified principals mapped to `hhc-web-bulletin-worker`. For those principals
the existing private completion route can accept the optional `derivation`
object only when `ASSET_DERIVATION_PROOF_ENABLED=true`. This separate admission
and persistence gate defaults to false (unset); other values besides `true`
and `false` reject startup. The only approved pair is transform `bulletin-watermark` and version
`"1"`. Supply the exact source asset ID, lowercase SHA-256 and recorded ETag.
Asset fences the clean, completed private bulletin issue PDF and the member
download-job output in one transaction. The output is still validated against
its byte length, checksum and MIME. The worker retains responsibility for
binding the source to the authorized bulletin job and locale.

A matching committed proof replays the original output. A different proof or a
completed ordinary upload returns 409. A source deletion or quarantine that
wins before the transaction prevents trusted completion. Source retirement
after commit does not change the existing output lifecycle. Migration 051
adds nullable structured proof storage. With the gate off, the old narrow
completion adapter still performs the same transaction fence using transient
proof, writes only its historical signature/details, and leaves the new column
NULL. It continues necessary work without admitting the new proof endpoint.
With the gate on, both completion paths persist structured proof. The adapter
retains its original response and historical replay format. An ordinary first completion queues scanning.
An ordinary request with identical bytes can replay an already completed
trusted output under the historical idempotency rule. It cannot create or
upgrade trusted evidence, and the replay retains the original safety kind.

Verified workloads receive `processingReadiness` on private asset GET and
completion. Version 1 states are `pending`, `ready` and `rejected`. Readers
must reject unknown versions and states. They may infer readiness from
recognized legacy fields only when the field is absent. `ready` concerns
technical availability. Access, publication and retention still have their
existing checks. Optional `safetyEvidence.kind` is `scanned` or
`trusted-derived`. The legacy `scanStatus: clean` enum continues to represent
accepted safety, including the established trusted watermark path. It is not
proof of an actual malware scan. Consumers that require a scan must also
require scanned evidence. Legacy and end-user projections keep their existing
shapes.

First deploy the migration and compatible producer with the gate unset/false.
The first release can then restore its pre-AS4 image: legacy completion creates
no new proof-format rows. Complete the owning release and retain its immutable
healthy image as the compatible rollback baseline before enabling proof writes.
Then close the reader, identity and CMS gates below and explicitly enable
`ASSET_DERIVATION_PROOF_ENABLED=true` through the owning runtime configuration
workflow. Do not enable it during the first compatibility release. A workload
transport switch can receive the optional fields even while the proof scope
is empty. Close the CMS, Account and LINE readers before enabling their
workload transport. Enable derivation only after the verified identity and CMS
release gates. Set the proof gate false and remove the derivation scope to
disable new proof admission/persistence; the narrow adapter continues work.
Both modes read and preserve previously stored proof, including legacy and
ordinary idempotent replay. Turning the gate off never erases or downgrades
stored evidence. Keep an AS4-compatible writer while
structured proof exists. The previous writer cannot validate source and
session atomicity or replay the structured evidence. After enabling writes,
rolling back the flag is safe only to the retained compatible image; it does
not make a pre-AS4 image safe again. Migrations and completed writes are never
rolled back by the release workflow.
