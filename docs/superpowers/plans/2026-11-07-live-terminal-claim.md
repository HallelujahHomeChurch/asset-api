# Terminal live claim implementation plan

Goal: an abort racing with live validation must fence publication without failing
the worker or retaining its completed processing lease.

Scope: PostgreSQL claim cleanup and the live worker only. Preserve terminal state,
immutable progress, active-claim conflicts, cancellation and database errors.
Do not recover aborted content, change CMS admission, or deploy this branch.

1. Add a failing real-PostgreSQL worker regression: abort while provider work is
   blocked, join both captures, preserve terminal reason/progress, release the
   finished claim and continue processing another capture.
2. Add conditional terminal-claim cleanup after the worker has joined its work.
   Lock capture then live/slot rows; only release matching claim identities.
   Active captures return false so unrelated conflicts remain failures.
3. Cover stale/reassigned slots, active conflicts, idempotency and cleanup errors.
4. Run focused tests, repository race/vet checks and CI; obtain review and leave
   a draft PR. Windows owns the separate plugin initialization-race fix.

Runtime: isolated local PostgreSQL and existing media test tools; no cloud writes.
