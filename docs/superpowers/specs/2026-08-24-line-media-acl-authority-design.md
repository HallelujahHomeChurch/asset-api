# LINE Media Collection ACL Authority Design

## Status

Approved for local implementation on 2026-08-24. Production data changes,
pull requests, merges, and deployments remain separately gated.

## Goal

Make an authenticated user's active collection ACL the only reader
authorization source for LINE media collections. Retain a separate live
`media-sync:manage` permission for the management plane.

## Final authorization contract

```text
Reader = trusted authenticated Account user
         AND active collection ACL matching either:
           - the immutable Account user UUID; or
           - an immutable Account role UUID from the verified access token.

Manager = trusted authenticated Account user
          AND a live Account `media-sync:manage` decision.
```

The canonical `media_sync_user` role and `media-sync:read` permission do not
exist in the final state. The `media_sync_manager` role remains and contains
only `media-sync:manage`. A manager does not become a desktop reader unless a
collection ACL also matches that manager.

## Trust boundaries

- API Gateway continues to validate Account JWT signature, issuer, audience,
  type, expiry, and not-before claims locally.
- API Gateway clears client-supplied `X-HHC-*`, Dapr caller, and Dapr token
  headers before injecting trusted identity.
- Account access tokens carry both role names for existing RBAC consumers and
  immutable role UUIDs in a new `role_ids` claim.
- API Gateway forwards role UUIDs as `X-HHC-Role-IDs` only after successful JWT
  validation.
- Asset API accepts collection reader identity only from the exact configured
  Gateway Dapr caller and app-channel token.
- Public hosts continue rejecting `/priv/*`.

## Reader behavior

- `GET /api/assets/collections` returns only active ACL matches. A user with no
  ACL receives `200` with an empty `collections` array.
- Collection-specific changes, item metadata, direct content, and reader ticket
  issuance return `404` for both missing and unauthorized resources.
- A reader content ticket stores the user UUID and immutable role UUID snapshot,
  expires no later than five minutes or the source access token, and rechecks
  the active ACL, item, ETag, scan state, and deletion state on every redemption.
- ACL revocation invalidates an already-issued reader ticket immediately.
- Role membership and account-status changes remain bounded by the access-token
  lifetime because Gateway deliberately performs offline JWT validation.
- Manager preview tickets remain a separate `access_mode=manager` capability,
  bounded to five minutes, and do not create reader ACLs.

## ACL subjects and management

- Direct-user ACL subjects use the Account user UUID.
- Role ACL subjects use the Account role UUID, never a mutable role name.
- Account ACL subject search returns the immutable UUID as `id` and the current
  human-readable user or role name as display metadata.
- Before forwarding an ACL grant, the LINE management facade re-resolves the
  exact subject through Account API. Users must still be active and roles must
  still exist. The Admin picker is not a security boundary.
- ACL mutations carry the verified manager user UUID and request ID to Asset
  API. Asset writes a successful append-only ACL audit row in the same database
  transaction as the grant or revocation. Audit records never contain content
  tickets, binding codes, credentials, or raw LINE identifiers.
- New collections have no ACL by default.

## LINE group binding

A LINE group binding controls ingestion into a collection only. It does not
grant Account reader access, manager access, or a collection ACL. Explicit
unbind/rebind product behavior is outside this authorization change.

## Client behavior

- The collection list no longer uses a global `403` entitlement failure. No ACL
  is a normal empty state.
- A known collection or item returning scoped `404` is treated as remotely
  unavailable and follows the existing safe unlink/stop-projection path.
- Existing `403` handling remains during the staged rollout so old Gateway and
  Asset revisions remain compatible.
- LibrePresenter never inspects `media_sync_user` or `media-sync:read`.

## Test-data transition

Existing role-name ACLs are deny-closed after readers switch to role UUIDs.
They are not automatically rebound by matching a current role name. Before any
deployment, a read-only inventory must list active user ACLs, role ACLs,
dangling subjects, and current Account memberships. Because the system is still
in testing, approved test data may be explicitly reset and recreated with UUID
subjects instead of adding a dual-read compatibility layer.

No migration or release workflow may silently delete or reactivate production
ACL data.

## Staged compatibility sequence

The final design has no legacy reader entitlement, but deployment still uses
expand/contract ordering to avoid an outage:

1. Account producer: add `role_ids` JWT claim and return UUID role subjects;
   keep the viewer role temporarily.
2. Gateway producer: verify and forward role IDs while retaining the old viewer
   route gate temporarily.
3. LINE management/Admin: create UUID role ACLs and validate recipients.
4. Asset: enforce ACL-only readers with role UUIDs, scoped `404`, live ticket
   recheck, and mutation audit.
5. LibrePresenter: accept both legacy `403` and new scoped `404` cleanup signals.
6. Gateway contract: remove `media_sync_user` from all reader routes.
7. Account contract: remove the canonical `media_sync_user` role and
   `media-sync:read` permission.

Gateway gate removal is the reader behavior switch. Account cleanup is last so
rollback can restore the Gateway gate until the observation window closes.

## Rollback

- Before Account cleanup, restore the Gateway `media_sync_user` requirement to
  immediately narrow reader access to the prior population.
- Preserve ACL rows, UUID claims, ticket schema, and audit data; do not perform a
  destructive database rollback.
- If Account cleanup has already occurred, use its down migration to recreate
  the canonical viewer bundle before restoring the Gateway gate.

## Acceptance criteria

- Authenticated user with direct user ACL and no viewer role can list, sync,
  inspect, ticket, redeem, and download only that collection.
- Authenticated user with matching role UUID ACL receives the same access.
- Authenticated user without ACL receives an empty list and scoped `404`.
- Manager without reader ACL can manage and preview through manager routes but
  cannot use the reader plane.
- ACL revoke immediately removes the collection and invalidates reader tickets.
- Missing and unauthorized scoped resources are indistinguishable.
- Forged identity, role-ID, Dapr caller, and Dapr token headers are rejected.
- No runtime reader code refers to `media_sync_user` or `media-sync:read` in the
  final Account/Gateway/Asset/LibrePresenter revisions.
- Repository-required tests, lint, builds, contract checks, and release checks
  pass independently for every repository before any merge.
