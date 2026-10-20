# Asset worker lifetime: source evidence and remaining acceptance

Scope: recording and retention entrypoints, plus the existing recording Event
runner. No schema, wire, resource tier, Job trigger, retry count, retention rule,
media quality, public boundary, global media slots or infrastructure change.
This is separate from the cleanup-health predicate PR and the live incident.

## Gaps and minimal correction

Before this change, retention used `context.Background()` without signal handling
or an invocation deadline. Recording maintenance started its five-minute deadline
after opening the DB pool and running health/setup. Event receive, Begin, Finish,
Ack and Poison inherited the long worker context, so even a no-work or losing
execution could retain a PG pool while control I/O waited without a short bound.
These source gaps do not establish the cause of historical Azure Job failures.

| Stage | Cooperative bound and cleanup |
| --- | --- |
| Recording bootstrap | Mode and invocation context established before storage/SQL setup; existing health probe remains capped at 5s. |
| Maintenance | Existing 5m budget includes bootstrap, sweep and outbox reconciliation. Platform stays 360s/retry0. |
| Event invocation | 355m from before SQL setup, below the existing 360m platform limit; no increase of any media attempt deadline. |
| Receive/decode/Begin, or rejected/poison message settlement | Shared 10s child context; no media processing starts on an admission error. Queue visibility remains 2m. |
| Media | Uses the original parent invocation context, not the admission context; existing 330m attempt and 5h live window remain unchanged. |
| Renewal | Existing 10s shared Queue/DB renewal budget and 30s renewal interval remain unchanged. Lease loss cancels and waits for the media callback. |
| Finish/Ack | Shared 10s child context. Failed/timed-out settlement does not acknowledge unfinished work. |
| Retention | SIGINT/SIGTERM cancellation and 14m application budget from before SQL setup, inside existing 900s/retry1 Job. Apply flag and transaction semantics unchanged. |
| Return/drain | Existing `defer db.Close()` stays in both commands; live standby unlock keeps its independent 5s cleanup and invalidates a lost connection. No forced media termination or lease/grant relaxation. |

The 355m outer budget leaves room around the 330m media attempt; normal bootstrap
control requests have shorter bounds. Parent cancellation/deadlines always win.
The limits are cooperative contexts, not a claim that a broken dependency which
ignores cancellation will return, or that an external SIGKILL executes Go defers.

## Focused evidence

- Before the fix, the real retention subprocess blocked on a controlled loopback
  PostgreSQL handshake exited via `signal: terminated` on SIGTERM. After the fix,
  the actual entrypoint returns normally and its bootstrap socket closes. This is
  not an authenticated PostgreSQL backend/residual-connection measurement.
- Controlled Event receive, Begin, rejected Ack, Poison, Finish and Ack stalls
  reproduce the long-context wait before the fix. They return at the short control
  deadline afterward, without acknowledging unfinished work.
- Virtual-time tests verify admission cancellation does not cancel media or
  settlement, the full 330m callback remains possible, and shutdown waits for
  callback cleanup without marking it finished/acknowledged. No media fixture is
  encoded and no Azure Job or OBS session is started by these tests.
- Invocation tests verify bootstrap has a deadline, the maintenance budget fits
  the existing 360s platform allowance, long work fits between its 330m media and
  360m platform bounds, and an earlier parent deadline/cancellation is preserved.

Focused commands:

```sh
go test -race ./cmd/recording-worker ./cmd/retention-worker ./internal/recordingqueue -count=1 -p=1
go vet ./cmd/recording-worker ./cmd/retention-worker ./internal/recordingqueue
```

Normal PR CI retains the repository's complete race, DB, contract, media, build,
image-transport and release-policy checks. Local long/source/near-limit cohorts
and passed Windows/product suites are not part of this source change.

## Retry, drain and admission: what is actually proved

`RecordingEventStore.Begin` requires the exact lane/token/generation and a valid
lease; renewal preserves that identity. The message and DB leases remain 2m.
Failed work still uses the existing 15s dispatcher backoff. A control timeout does
not introduce a reset, new capture, new operation key, early Ack or unlimited retry.
If a response is ambiguous, durable state and lease fencing remain authoritative.

The release workflow's existing `asset-api-production` concurrency group has
`cancel-in-progress: false`; it serializes that repository's normal release runs.
The workflow's existing migration Job start remains unchanged. This does not
serialize other repositories, Event arrivals, pending platform starts or an
operator's out-of-band manual starts. No new production admission lock is added.

Per-process recording/retention pools remain 3/2 respectively; global heavy-media
slots remain 2. Neither slots2, the live advisory lock, this finite context, nor
per-poll `maxExecutions` certifies a persistent count of PG-owning processes.
The proposed Jobs69/Jobs96/max8 aggregate budget remains **NOT CERTIFIED**.

## Production gates and rollback, not performed by this PR

After the active-recording/meeting hold and coordinator release admission:

1. Use only the normal merged-main CI/CD path. Record actual API revision and all
   worker image digests, unchanged tiers/flags/slot invariant and healthy traffic.
   Do not deploy this candidate locally or modify the active OBS recording.
2. Observe naturally scheduled maintenance/retention and actual Event executions;
   retain startup, waiting, processing, renewal, exit and complete provider wall
   times, including empty/losing/retry/draining executions. Do not manually trigger
   or duplicate a Job merely to fill an evidence gap.
3. In an explicitly admitted isolated environment, correlate cancellation during
   authenticated DB bootstrap/claim/processing/persistence with backend PIDs and
   verify zero residual connections after cooperative Close. A loopback socket
   test and ACA timeout metadata cannot replace this gate. Use existing approved
   read-only DB access, never a credential/IAM workaround.
4. Prove supported arrival, pending-start burst, retry and draining limits across
   all lanes before claiming a global PG execution budget. If the provider cannot
   prove the proposed persistent bound, return to the coordinator for a separate
   minimal admission design; do not infer one from a quiet snapshot.
5. Complete only remaining admitted amd64 long/source/near-limit/fairness cases.
   Keep passed short/Windows suites and interrupted/noisy-host evidence intact.
6. Review >=24h cost only after 2026-10-20 17:05:56 Asia/Taipei and provider usage
   availability, including all starts/standby/retries/maintenance and Queue/API/DB.
   Requested resource-time is not utilization, billed cost or proven savings.

If short control deadlines cause proven normal-operation failures, use a reviewed
revert of this source change through normal CI/CD in the coordinator's safe slot.
Keep Event mode, slot count, tiers, durable work and media verification unchanged;
allow valid work to drain. No forced kill, Queue purge, key reset, Stop/seal replay,
capture replacement, manual publication or deletion is a rollback step. The prior
healthy immutable image must be read back at release time, not inferred here.
