# Recording Event Jobs — candidate handoff

Status: candidate implementation and isolated verification. **No merge, production
resource creation, trigger switch, key change, OBS operation, or recording mutation
is authorized by this document.** The source chat continues live feature closeout.

## Resource decision

The user clarified processing to **2 CPU / 4 GiB** in this implementation session.
The earlier 2 CPU / 8 GiB was a verbal error and is not a valid standard Consumption
pair. Live: 4/8; processing: candidate 2/4; previews: candidate 1/2; hourly maintenance:
candidate 0.5/1. Processing/preview fallback is 4/8 independently of event mode.

## Flow and changes

- Existing durable media work tables serve as the outbox; three PostgreSQL rows
  coalesce/fence lane dispatch. The dispatcher probes committed readiness every
  10 seconds (5-second loop). No producer trigger locks shared dispatch rows.
  Transaction rollback is invisible to dispatch; feature-off/old API versions do
  not accumulate an unbounded second outbox. Attempts, receipts and media slots
  remain authoritative.
- API minReplicas=1 was read back before implementation. The dispatcher is enabled
  only by `ASSET_RECORDING_EVENTS_ENABLED`; it caps outstanding admissions at two,
  coalesces per lane, fences generations/tokens and retries failed/lost sends.
- Shared claim readiness SQL avoids launching work before dependencies are ready.
  Readiness probes run in the existing API, including future retry/expired leases;
  they incur DB overhead but do not launch high-resource idle containers.
- Event workers renew DB ownership and Queue visibility, use the current pop receipt
  when acknowledging, and cancel media on lost ownership. Completion persists before
  message deletion. Old/duplicate messages cannot reactivate a running owner.
- Required pending media keeps priority. A job already holding one slot no longer
  makes optional work ignore the other free slot. Both still acquire existing fenced
  media slots; no preemption and no additional slots are introduced.
- Exhausted last-attempt crashes are eligible for the existing claim's terminal
  settlement; they do not wait an hour to leave processing.
- Maintenance calls existing bounded/idempotent cleanup functions with a five-minute
  context, then wakes stale work. It runs hourly at minute 17. Real authorization
  expiry remains independent of physical deletion.

No K3/B1/C1/D1/S10, inventory, digest, idempotency, public range, member access,
validation tolerances, transcoding quality, or CMS publication schedule changes.

## Provisioning preview and permissions

The readonly provider what-if for the standalone module proposes **22 creations**:
4 Manual Jobs, 6 queues (3 work + 3 poison), and 12 queue-scoped role assignments.
No existing resource is modified/deleted by that provisioning preview. It uses the
currently deployed immutable recording image as a resource-shape placeholder; a
fresh reviewed preview with the CI-produced candidate digest is required before
actual provisioning. No production resource was created.

- API system identity: message sender on the three work queues.
- Existing recording runtime identity: message processor + reader on each work
  queue, sender on the poison queues. Existing scoped Key Vault/Blob/ACR access is
  reused. No Contributor or job-start ARM privilege is added.
- Scaler uses `queueLengthStrategy=visibleonly` and `maxExecutions=1` per lane,
  min=0, polling=10s. Duplicate executions can still occur under at-least-once
  delivery/scaler startup; DB fences reject them. Actual startup counts remain an
  activation acceptance metric, not a claimed global hard limit.

Azure role/resource preview was generated using ProviderNoRbac. It does not prove
that the release identity can apply new queue-scoped roles. Verify that permission
before approval; do not broaden CI identity to Contributor to make it pass.

## Fixed configuration and deployment gates

Defaults are off:
- `RECORDING_EVENTS_PROVISIONED=false`
- `RECORDING_EVENTS_ENABLED=false`
- `RECORDING_EVENTS_CHANGE_APPROVED=false`
- `RECORDING_PROCESSING_CPU=2` (memory 4Gi; 4 means 8Gi)
- `RECORDING_PREVIEW_CPU=1` (memory 2Gi; 4 means 8Gi)

Provisioning=true deploys the new module with Manual triggers while enabled=false.
Activation requires provisioned=true, HLS/member video enabled, fresh reviewed
what-if and explicit production change approval. The release changes the legacy
job to Manual and starts the three Event triggers plus hourly maintenance.
There must be no active formal recording/live or legacy long execution at switch;
the pre-deploy snapshot rejects a trigger switch with active executions. Once
Event resources exist, provisioned=true must remain set so CI cannot orphan their
triggers while enabling the legacy Schedule.
The API is deployed through the same CI/CD path with the event flag and queue URLs.
Existing API receipt acceptance must survive delayed provisioning/send failures.

Release scripts verify trigger/image/scaler/CPU/memory settings. The what-if extension only
covers the four named recording jobs, named recording queues and queue-scoped
roles with known principals/role IDs; unrelated changes still pass the old policy.

## Tests, measurements and evidence boundaries

Local full `go test -race -json ./... -count=1 -p=1` passed (1,132 test
cases); final modified PostgreSQL package race run passed separately (115s), as
did queue/config/transport/worker/server race tests. Release/activation/rollback
policy tests, migration policy, `go vet ./...`, and Bicep compilation passed.
The governance export is run separately with the recorded test event stream.
The deliberate low-disk fixture is opt-in and was not run locally; CI and cloud
acceptance status are reported on the PR. Tests cover DB admission, transaction rollback,
multiple API replicas, stale generation/token, duplicate start, late send ack,
worker visibility/DB lease loss, timeout recovery, exhausted-attempt settlement,
readiness priority, strict event/config validation and deployment safety gates.
The full media race suite remains required; no failing check may be bypassed.

`scripts/measure-recording-tiers.py` runs same-image Linux FFmpeg fixtures three
repetitions per resource tier, records container cgroup memory peak/OOM events and
wall time, and keeps outputs. The source encoding fixture is one rendition, not a
full long source-to-HLS pipeline. Short synthetic results are not Azure throughput,
long-film memory acceptance, CPU utilization, billing, or a savings percentage.
Earlier exploratory 2/8 measurements are superseded by the user-confirmed 2/4 run.

Local Linux/arm64 Docker fixture observations (3 runs each, same runtime image;
wall includes container startup and fixture generation; concurrent host tests were
not controlled, so these are smoke measurements, not controlled cost comparisons):

| Fixture | CPU/GiB | median wall s | max wall s | cgroup peak MiB | failures/OOM |
| --- | --- | ---: | ---: | ---: | ---: |
| cover | 4/8 | 8.546 | 14.998 | 48.8 | 0 |
| cover | 1/2 | 25.787 | 63.457 | 48.4 | 0 |
| encoding-fixture | 4/8 | 2.695 | 2.857 | 66.8 | 0 |
| encoding-fixture | 2/4 | 1.17 | 4.379 | 67.1 | 0 |
| timeline | 4/8 | 5.212 | 5.822 | 31.5 | 0 |
| timeline | 1/2 | 4.805 | 4.999 | 30.9 | 0 |
| validation | 4/8 | 7.001 | 7.003 | 250.4 | 0 |
| validation | 2/4 | 6.579 | 10.581 | 250.3 | 0 |

The 1CPU cover result is variable and slower; do not accept that tier from these
samples or extrapolate a savings percentage. Actual Azure and representative long
media remain gating. 2CPU validation and encoding also require their complete
workloads, not just these short fixtures. Candidate tier configuration is not
production tier approval.

Not yet accepted: 24h zero-work cloud execution count, representative 2.5h input
resource tiers, live + background-work fairness on Azure, real cold starts/recovery,
actual billing/Queue/DB cost and apply permissions. Until these pass, reduced
resource adoption and trigger activation remain pending. The known old cost cannot
be assumed entirely removable; bill actual media work and all standby/retries.

## Switch and rollback

1. Green PR CI and a fresh infrastructure what-if reviewed by the user.
2. Additive code/migration first, new resources Manual, dispatch disabled. No old
   work is reprocessed; preserve all originals/diagnostic media.
3. Confirm no active formal/long work, then perform approved CI/CD activation.
4. Read back exact image, jobs/triggers/resources/identity/flags; run a short live,
   source-upload fixture, first/refresh cover, preview and natural closeout.
5. Observe >=24h idle baseline and comparable normal work/billing before claiming
   savings or deleting transitional gates.

Resource-only failure: stop new admissions for the affected lane, let valid leases
finish or expire safely, deploy that lane at 4/8 through CI/CD, preserve Event mode.
No blind Stop/seal, capture replacement, manual publish, or media cleanup.

Event mechanism failure: disable dispatch and Event scaling, stop new admission,
let in-flight fenced work settle, restore the known compatible legacy image and
minute Schedule. Keep additive tables and queue/outbox messages. Do not run both
consumer strategies freely at once; the media slots stay two through rollback.

The release captures only non-secret prior Job trigger/template configuration
before deploying. For a provisioned Event topology, failure pauses all five recording triggers, restores the
previous API, waits up to five minutes for existing executions to drain, then
restores prior Job images, environment references and triggers together. Newly
provisioned Jobs remain Manual if absent from the old topology. A still-active
execution prevents automatic Schedule restoration and fails the rollback step;
operator follow-up must complete the safe drain, not forcibly stop media work.
The snapshot is runner-local, not uploaded; configuration secret values are never
queried. Seven isolated tests cover first activation failure, prior Event mode,
active-execution safety, literal-secret rejection, workflow ordering, active-work cutover rejection and
missing-provisioned-flag rejection. Initial default-off releases retain the
existing API-only rollback path; they do not pause the legacy media job.

Independent review found and corrected: shared producer-trigger lock inversion
(removed triggers; durable readiness is the outbox), incomplete API-only rollback
(coordinated topology snapshot/restore), and VOD preview starvation beside an
already claimed validation (actual readiness and claim regression).

## Windows handoff — NOT ready for production Event acceptance

Continue independent live UI/feature tests against the currently released platform;
this candidate has not changed production. No plugin update or key reset is needed.
After explicit Event deployment approval and exact deployed-version handoff, use
one short non-meeting run to measure cold-start-to-verified, cross-minute progress,
normal following latency, cover/preview supply and natural same-URL VOD closeout.
No repeat of fixed-key, 16-minute recovery or 2.5h suite. Historical weak-network,
seek and 379/388-second backlog verdicts remain unchanged. Physical phone acceptance
stays with the user. Report actual commits/IDs/timezone/decoded positions; no secrets
or signed URLs. Keep original recordings and journals.

## References

- https://learn.microsoft.com/en-us/azure/container-apps/containers#vcpu-and-memory-allocation-requirements
- https://learn.microsoft.com/en-us/azure/container-apps/billing
- https://keda.sh/docs/2.20/scalers/azure-storage-queue/
