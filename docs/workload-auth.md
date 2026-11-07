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
Audit dispatch. No Entra client, tenant, audience, object or app-role settings
are required for Asset caller admission. Managed identities for Blob, queues,
Key Vault and image pulling retain their separate responsibilities.

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
