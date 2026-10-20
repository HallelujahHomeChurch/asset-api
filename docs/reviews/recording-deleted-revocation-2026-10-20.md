# Deleted broadcast authority convergence

## Problem and change

`SetBroadcastRange` rejected a recording tombstone before considering a pending
revocation. The CMS could retain a legitimate revoked/blocked range intention but
never commit it to Asset and the conditional R2 authority object.

The existing owner advisory lock and tombstone query now have a private helper
returning `(deleted, error)`. Every other `checkRecordingNotDeleted` caller keeps
the original rejection semantics. Only `SetBroadcastRange` admits a tombstoned
recording when `revoked=true` and `memberState=blocked`.

Admission still requires the existing capture-to-recording identity, nonzero and
latest epoch, locked current range, monotonic revision, identical replay and frozen
boundaries. R2 conditional writes and DB commit must complete before success;
storage errors are not converted to conflicts or successful no-ops. Revocation
cannot create captures, renew grants, clear tombstones, unexpire media or restore
access. No schema, wire fields, media verification, cutoff, cleanup, retention,
resource, trigger or concurrency changes are included. The OpenAPI description
documents the existing request shape's narrowed deny-only behavior.

## Evidence and remaining limits

The real PostgreSQL/HTTP regression first failed with `409 AST_CONFLICT` for an
authorized CMS revocation after deletion, then passed with durable DB/provider
revocation and identical replay. An unauthorized caller remains forbidden.

Regressions also exercise rejected nonrevoked/nonblocked policies, wrong recording
or epoch, superseded epoch, stale/equal-conflicting revision, frozen boundaries,
missing captures, no unrevocation and new-capture rejection. Provider failure and
a PostgreSQL deferred-trigger commit failure remain unacknowledged; the latter leaves
the remote deny policy ahead, and an identical retry safely converges the DB.
A concurrent deletion is observed waiting on the in-flight authority transaction's
advisory owner lock through a dedicated PostgreSQL backend before both operations
finish. Removing that advisory lock makes this regression fail; restoring it passes.
Existing deletion, range, R2 CAS and HTTP tests remain part of validation.

These are local PostgreSQL and provider-fixture tests, not production playback,
real cloud storage or residual-connection certification. The three hashed CMS
`range_apply` failures observed on 2026-10-20 have not been independently mapped to
Asset tombstone rows; this source defect is confirmed, but their exact cause remains
unproven. No production DB credential, alternate access path, manual Job, requeue,
recording deletion, Windows operation or media suite is required by this change.

## Delivery and acceptance

This change starts from main `b7c2b2a2339b1fb375a915ebd9c5a531242743e7`
(Asset #132). It is separate from #132's release and #133's worker-lifetime work.
Review and normal required CI must pass on its exact head. Production integration
requires a later coordinator slot and normal immutable main CI/CD, Provider
preview, health/security smoke and fixed observation windows.

After deployment, observe the original pending references without replaying
operations. Confirm real authority/projection convergence before calling the
incident resolved. Remaining conflicts must retain their identity/revision/storage
error meaning; do not suppress CMS errors or assume every 409 is deletion-related.
