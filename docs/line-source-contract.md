# Dedicated LINE source contract

This is an additive, disabled-by-default contract for the verified `hhc-line-harness` workload. The legacy caller and its scanned profile remain separate. Do not admit the new caller until Account current-subject, Harness source-check, Gateway routes and the central Audit catalog have been deployed and accepted together.

## Current authorization

Configure all four variables together: `ASSET_SOURCE_GATE_ORIGIN`, `ASSET_SOURCE_GATE_TOKEN`, `ASSET_ACCOUNT_ORIGIN`, `ASSET_ACCOUNT_TOKEN`. Origins must be HTTPS origins; tokens are distinct 64-character lowercase hex secrets. No values are configured by this change. Requests use a two-second timeout, bounded strict JSON, no redirects and no proxy inheritance.

Asset sends `POST /priv/source/check` to Harness with `X-HHC-Harness-Service-Caller: asset-api` and its dedicated `X-HHC-Harness-Service-Token`. The version-1 request carries mode, persisted source policy, collection/item IDs, publication revision, ACL revision and binding generation as applicable. Harness must resolve its own persisted authority without calling Asset recursively. The response is `{version:1,state:"active|paused|revoked|expired",policyRevision:<positive integer>,revocationRevision:<positive integer>}`. Missing, unknown or malformed authority fails closed.

For collection reads Asset sends `POST /priv/account/v1/current-subject` with `{userId}` and dedicated `X-HHC-Account-Service-Caller: asset-api` / `X-HHC-Account-Service-Token`. Account returns `{version:1,active:<boolean>,userId,roleIds:[]}`. Current roles replace Gateway role claims for source collections. Source tickets also require the currently authenticated user to match the original ticket subject; managed-content tickets are unavailable for source collections.

Source errors use `error.code` and `error.scope` (`item`, `collection`, `dependency`): `AST_SOURCE_PAUSED` is 423, revoked/expired are 410, ACL revoked is 403, unavailable is 503. Dependency failure and pause must not advance a consumer cursor or remove a root. Only definitive item revocation/expiry becomes a tombstone. `GET /api/assets/collections/{collectionID}/control` returns minimal control and revision fields after current Account and ACL checks.

## Publication and mutation

`GET /priv/assets/upload-sessions/{operationID}` recovers the dedicated owner’s original asset/session metadata after an uncertain create, including expiry/deletion. It never creates an upload or signs a target and does not depend on source availability. A missing operation returns 404; failures remain retryable. This lets the producer reconcile revocation racing with creation without creating an orphan merely to discover its identity.

Create source collections with a positive `bindingGeneration`. ACL add requires `expectedAclRevision`; ACL revoke requires an exact quoted decimal `If-Match`, such as `"7"`. Every ACL row mutation increments the persisted revision transactionally.

1. `POST /priv/assets/collections/{collectionID}/staged-items` accepts the existing item input plus binding generation and expected ACL revision. It creates a hidden item without advancing the reader cursor.
2. Producer persists returned item ID and approval context.
3. `POST /priv/assets/collections/{collectionID}/items/{itemID}/activate` checks current source authority and matching stored revisions, then advances the collection cursor once.

Both writes require `Idempotency-Key`. `GET /priv/assets/collections/{collectionID}/publications/{operationID}` recovers the staged operation metadata independently of source availability, including after collection deletion; it never grants bytes. Unknown outcomes must be reconciled through this route before any new operation. `PUT /priv/assets/collections/{collectionID}/source-binding` atomically compares expected binding/ACL revisions and accepts only a strictly newer binding generation.

The producer fixture adds `asset.collection_item.stage`, `asset.collection_item.activate`, and `asset.collection.bind`; item deletions accept the dedicated harness service principal. The central Audit catalog must carry the same contract before runtime admission. Management binding retains a verified user actor; background publication uses the service actor.

## Bytes, expiry and compatibility

`GET`/`HEAD /priv/assets/{assetID}/source-content` is restricted to the dedicated verified caller. Every request, including Range and conditional 304, checks current authority and absolute expiry before serving. New items expose `sourceExpiresAt`; consumers must enforce it locally, including offline bytes. Existing reader projections remain unchanged when no source policy exists.

The source reference uniquely identifies one original within the producer; separate messages may contain identical bytes. Source metadata and publication deadline are immutable. The existing lifecycle worker first expires an eligible source through the existing collection-first soft-delete transaction, including sources without collection references. This creates permanent tombstones before normal byte purge. Collection retention/exemption cannot extend source lifetime. This code does not enable a worker, change cloud storage, or perform a backfill.

Local PostgreSQL, HTTP and TLS fixtures test these contracts. They do not establish CI, central-service deployment, LINE acceptance or Presenter offline behavior.

Harness startup must require `sourceUploadRecoveryVersion: 1` from upload-policy. Contract v2 without this capability is insufficient for safe ambiguous upload recovery. Legacy scan-required contracts omit the field.


### Presenter protocol admission

New-source reader requests must send exactly one `X-HHC-Source-Policy-Version: 1` header. The header advertises client compatibility; it grants no identity or ACL rights. Asset resolves current Account roles and checks collection ACL before returning `426 AST_SOURCE_CLIENT_UPGRADE` for absent/unsupported versions. Legacy collections retain their existing contract. Source failures carry `{error:{version:1,code,scope,message}}`; consumers must keep dependency/unknown failures recoverable and distinguish item deletion from collection revocation. Authorized source content, including HEAD/Range/304, carries `X-HHC-Source-Expires-At` with the immutable UTC deadline. Presenter persists this deadline in local metadata and native file policy and stops active playback at expiry. Public anonymous tickets remain unsupported for new sources; browser Presenter uses authenticated content with a bounded ephemeral object URL.

### Independent collection discovery

`GET /api/assets/collections` (or `?sourcePolicyVersion=0`) lists only legacy collections using the existing verified claims and local ACL. It never calls Account or Harness. `?sourcePolicyVersion=1` independently lists source collections using current Account roles and Harness authority; clients must advertise source compatibility. Cursors cannot cross scopes. Unknown, empty or repeated selectors return 400. Source dependency failure or pause returns an error without a page/cursor.

Presenter merges the two complete listings. A retryable source-list failure leaves legacy choices available with an explicit incomplete-list notice and retry; it does not commit a partial source listing. Authentication loss still fails the operation. This selector must ship to Asset before the new Presenter is admitted; no source collections may be admitted with older clients.

### Bounded source authorization

Source discovery and change pages use `POST /priv/source/check-batch` with the same dedicated Asset credential. The envelope is `{version:1,requests:[{id:"0",request:<single-check request>},...]}`; IDs are consecutive decimal indices and each batch contains 1–100 entries. The response is `{version:1,decisions:[{id:"0",decision:<single-check decision>},...]}` in exact request order. Unknown fields, mismatched IDs, missing/extra decisions or invalid states fail closed. Requests are limited to 64 KiB; responses to 16 KiB; the existing two-second timeout, redirect prohibition and cancellation apply per batch.

A 500-entry change page uses one Asset metadata query, at most five Harness batches and two collection-control checks. Harness resolves each batch in one database snapshot, without calling Asset. Local ACL/binding versions and Harness collection policy revision must remain stable; expiry is checked again after all batches. Dependency failure returns no page/cursor, while definitive item expiry/revocation produces tombstones. No cross-request allow cache or single-item fallback exists. Deploy the batch-capable Harness before enabling source reads on Asset; legacy collection, bulletin and recording paths do not call it.

Harness increments a dedicated per-group `revocationRevision` in the same transaction as source invalidation. Asset compares it across active batch results and the final collection control check. Source withdrawal therefore invalidates an in-flight multi-batch page without changing management/Reply generations; direct source reads still reauthorize independently. Apply Harness migration 029 before enabling this response contract. Missing revision fields fail closed.
