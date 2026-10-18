# Recording Event Job cutover review

This document authorizes no production cutover. The user approved releasing code
with Event Jobs disabled, followed by a reviewed what-if before activation.

## Current release and protected behavior

PR #121 merged as `d4de939bbbcb3b262eb34a741925121ff5ede25f`.
`RECORDING_EVENTS_PROVISIONED`, `RECORDING_EVENTS_ENABLED`, and
`RECORDING_EVENTS_CHANGE_APPROVED` are explicitly false at repository scope.
Until a separately approved cutover, legacy Schedule `*/1 * * * *`, 4 CPU/8 GiB,
existing uploads/live, media validation, and the two global slots continue.
No recording, key, Stop/seal, public boundary, or notification is changed here.
Deployment revisions, images, health and final what-if results must be read back
from Azure after release; the PR is not deployment evidence.

## Confirmed release-permission gap

The production deployment principal was read back as Contributor at resource
group `alive`, with no additional inherited IAM-write grant at the recording
storage account. Contributor cannot submit `roleAssignments/write`. The original
Event module included twelve assignments, so provisioning through that release
identity was not ready even though the flags-off release is safe.

The follow-up moves the exact existing role declarations and deterministic IDs
into `infra/recording-event-permissions.bicep`, a separate one-time bootstrap.
Ordinary provisioning creates four Jobs and six queues, with no IAM writes.
Activation verifies all twelve queue-scoped grants before any release mutation.
Missing permissions and Azure query errors fail closed.

| Scope | Principal | Role |
| --- | --- | --- |
| each of 3 work queues | Asset API system identity | Storage Queue Data Message Sender |
| each of 3 work queues | existing recording runtime identity | Storage Queue Data Message Processor |
| each of 3 work queues | existing recording runtime identity | Storage Queue Data Reader |
| each of 3 poison queues | existing recording runtime identity | Storage Queue Data Message Sender |

No Contributor, Owner, job-start privilege or credential is added. Like the
existing recording-source bootstrap documented in `infra/README.md`, applying
these grants needs a separately approved operator with permission at the exact
scopes; it is not silently substituted for the application release workflow.

## Ordered cutover, after approval

1. Fresh full Provider what-if against the released immutable API/worker images.
   Provision only: `provisioned=true`, `enabled=false`, approved=true. All four
   new Jobs remain Manual; legacy Schedule continues. Expected new resources:
   four Jobs + six queues, no role writes or media mutations.
2. After queues exist, preview the standalone permission template with real API
   and worker principal IDs. Require exactly twelve scoped Create/NoChange grants,
   no unrelated changes/deletes. An explicitly approved authorized operator may
   apply this merged template; do not expand ordinary CI permissions. Read back
   all grants with `verify-recording-event-permissions.sh`.
3. Fresh activation what-if. Set enabled=true through the release workflow only.
   Use an agreed quiet window: no live/OBS output or new uploads, no pending long
   execution. Capture the old non-secret topology. Immediately before runtime
   activation, CI checks idle again, pauses recording triggers and waits for any
   raced old execution to drain. It never kills an execution. A precondition
   failure before pausing leaves the old schedule unchanged.
4. CI deploys the new API flag, legacy Manual, three Event Jobs and hourly
   maintenance (`17 * * * *` UTC). The paused output makes failure rollback run
   even if runtime deployment has not begun. Existing DB work remains durable
   during the bounded transition. No new input should start until smoke completes.
5. Read back revision/image/traffic/health; flags, Job triggers, resources and
   scopes; API min replicas >=1; global media slots remain two. Query no credentials.
6. Complete the minimal validation below. Do not classify deployment as acceptance.

Initial low-risk activation should isolate trigger migration from unaccepted tier
reductions: live/processing/preview 4 CPU/8 GiB, hourly maintenance 0.5 CPU/1 GiB.
The user-confirmed processing 2 CPU/4 GiB and preview 1 CPU/2 GiB remain candidates.
Compare them independently after representative headroom/cost validation; revert
any failing lane to 4/8 without changing media checks or adding slots.

## Minimal acceptance and cost evidence

- One short non-meeting S10 OBS run: cold first-ready, continuous cross-minute
  watermark, stable following at about 2–3 minutes, first/refresh cover, timeline
  and natural Stop/seal/ready/publication/same-URL VOD. Preserve originals. No
  plugin or wire change; do not repeat fixed-key, 16-minute or 2.5h suites.
- Platform: source-upload fixture and HLS package integrity/complete decode,
  optional work beside validation, bounded queue/lease recovery, maintenance
  expiry/reference guards. A published old recording is observation-only.
- Observe >=24h with no new media: count **all** executions, standby, retries,
  claims/polls and startup seconds; heavy Jobs should have no periodic starts.
  Include Queue transactions and API/DB overhead. Compare equivalent work and
  Azure usage/cost meters; CPU×wall and allocation are not CPU utilization or bills.
- Tier A/B: same immutable image and media, >=3 runs, median/max stage durations,
  entire-container/cgroup memory, >=20% timeout and memory headroom, no OOM/new
  retries/backlog. Source and complete validation separately; cover and timeline
  separately. Short local fixtures do not establish long-media or billing acceptance.
- Failed acceptance restores the known healthy mode/tier; do not weaken checks,
  extend cutoff, replace captures, manually publish or clean diagnostic media.

## Rollback

- Before mutation, retain the non-secret old Job image/template/trigger snapshot
  and previous API revision/image. If provisioning alone fails, the legacy
  schedule/API remain the serving path. New Jobs stay Manual.
- Activation failure: stop new scaling, restore the previous API, safely drain
  running executions, restore the captured images/env/trigger topology. Old queue
  messages/work rows remain; DB fences preserve idempotency and slots=2.
- Five-minute drain timeout never force-stops media. Fail the rollback visibly;
  retain `recording-rollback-<run>-<attempt>` for seven days. Review and resume the
  exact restore through CI/CD after safe drain. Do not lose the snapshot when the
  runner exits, and do not activate competing schedulers.
- Resource-only regression: restore that lane to 4/8 in CI/CD after safe drain;
  retain Event mode. No wholesale cron rollback merely for a slow cover tier.
- Do not down-migrate, delete queues/grants/media, reset keys or resend Stop/seal.

Actual reviewed parameter files, what-if hashes and release smoke evidence are
attached to the cutover handoff, and must be regenerated if the live state drifts.
