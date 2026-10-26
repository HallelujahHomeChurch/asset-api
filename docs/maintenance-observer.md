# Bounded maintenance ownership inventory

Run `scripts/sql/maintenance-observer.sql` only with the separately approved
observer connection and an explicit maintenance/read-only authorization:

```sh
psql -X -v ON_ERROR_STOP=1 -f scripts/sql/maintenance-observer.sql
```

Supply the connection through the existing approved environment/service profile;
never paste it into logs. Do not create grants, change roles, or use an application
credential to work around access denial. Missing privileges/schema, lock or query
timeout means **no evidence**, never zero. Accept the output only after psql exits
successfully and COMMIT completes; discard partial output on any failure.

The query takes one repeatable read-only snapshot, with a 5-second statement
budget, 250ms lock budget and 5-second idle transaction budget, then closes its
transaction. No row locks, mutations, payload/JSON extraction or identifiers are
returned. Fixed component labels, counts and aggregate timestamps only. Do not
poll continuously or hold a transaction open across observation rounds.

## Interpretation

- `leased` includes dispatch/queue leases; it is ownership requiring settlement,
  not a unique active-operation count. Slots, wakes, work and attempts overlap:
  **never sum components as unique tasks**.
- Expired leases and unfinished/unleased processing effects require owner review;
  expiry does not prove external effects stopped.
- Retained due/delayed candidates may remain through maintenance only with a
  verified no-new-claim fence and compatible target schema, payload, version,
  session/retry expiry and resume behavior. Candidate is not approval.
- Failed candidates may remain once the owner confirms settled effects and no
  escaping replay/cleanup writer. Historical failure is not automatically NoGo.
- Upload/capture sessions need completion-intake fencing and a proven late-byte
  staging/resume contract. Do not automatically wait for every signed URL expiry.
- Actual attempt start is reported only where stored; null is unsupported/absent,
  not zero. Lease expiry or generic updated_at is not a last-claim timestamp.

**Zero output counts never mean Go.** This inventory does not inspect Azure job
executions, PG sessions/transactions/advisory election locks, storage event wiring,
queues/poison replay, legacy retired uploads, completed attempt cleanup or other
services. These remain separate owner/runtime checks. Confirm active completion
and no next claim with the real hold control; if unavailable, naturally drain only
that lane or postpone. Keep existing PG physical-drain checks. No NOLOGIN,
termination, queue deletion or attempt reset to manufacture idle.

## Cost and compatibility

One scan per component (assets and packages have distinct scan/preview semantics).
Outbox delivered-at predicates and existing pending indexes reduce eligible work;
some state/history predicates and aggregates can still scan substantial tables.
No claim of index-only or constant cost: EXPLAIN on representative disposable data
and the hard timeout bound are the safeguards. A timeout is a failed observation,
not permission to raise limits automatically or add indexes during maintenance.
This is operator-triggered, not an application hot path or a new CI job.

Before use against a target revision, run the existing PG tests after its exact
migration chain. The initial implementation is based on main485f755 and requires
no new schema; pending173 must retain a separate composition validation. Neither
successful tests nor this document authorizes production SQL, merge or deployment.
