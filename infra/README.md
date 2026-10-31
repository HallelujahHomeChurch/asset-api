# Asset API Azure deployment

## Recording HLS and temporary sources

`RECORDING_HLS_ENABLED` and `RECORDING_SOURCE_ENABLED` repository variables
default to `false`. HLS requires `MEMBER_VIDEO_ENABLED`; browser source
processing additionally requires HLS. Deploy compatible producers before
enabling consumers. Enabling HLS changes only `asset-recording-validation`
to 4 vCPU / 8 GiB, a six-hour platform timeout, and no platform retry. Its
application deadline is 5.5 hours; retries and the two global processing slots
are database-fenced. Scheduled executions that cannot acquire a slot exit
without encoding. This is a limit of two active processors, not a platform
limit of two concurrent containers.

`recording-source.bicep` owns the independent `aliverecordingsprod` storage
account and private `recording-sources` container. It must be provisioned from
reviewed, merged code after an approved what-if, before enabling source
processing. Do not change the account-name parameter independently of the
runtime configuration and gateway CSP. The template touches no ordinary asset
storage, scan queues, DNS, or application resources. API access uses its
existing system-assigned identity; only the recording Job selects the runtime
user-assigned identity. Each receives container-scoped Blob contributor and
account-scoped delegation rights. No account key is enabled or required.

Browser uploads use narrowly scoped user-delegation SAS, with CORS restricted
to `https://admin.alive.org.tw`. Recording sources do not enter ClamAV; Defender
upload scanning is disabled only for this new storage account. Ordinary asset
scanning remains unchanged. Sources and generated HLS never persist in the
ordinary assets container. Application cleanup removes ready sources within
24 hours and failed sources seven days after first completion. A nine-day
Blob lifecycle rule is a fallback; soft delete and versioning are disabled on
this disposable source account. Only validated HLS remains in private R2.

The release preflight rejects source activation if the private container or
either identity's required roles are absent. The normal runtime what-if guard
continues to reject storage mutations; source provisioning is a separate,
reviewed infrastructure operation through the main-only **Recording source
infrastructure** workflow. Run its default preview first, approve the exact
change set, then dispatch with `apply=true` and confirmation
`create-recording-source-storage`. Its guard permits only new resources under
the exact source account, never modifications/deletions. Do not grant extra
pipeline RBAC implicitly if provisioning reports insufficient permission.
The source workflow uses full `Provider` permission validation, not
`ProviderNoRbac`. The currently inspected production deployer has Contributor
only and cannot bootstrap role assignments: this workflow is not deployable
under that identity until a separate authorization decision. A one-time
operator bootstrap from merged code requires its own explicit approval; it
must not be silently substituted for CI or expand pipeline permissions.
Rolling back application images does not
delete sources, published R2 objects, or database state.

```sh
az deployment group what-if -g alive -f infra/recording-source.bicep \
  -p location=eastasia storageAccountName=aliverecordingsprod
```

## Existing assets

### Recording cutover release checklist

The source account bootstrap completed separately from runtime activation.
Before enabling `RECORDING_HLS_ENABLED=true` and
`RECORDING_SOURCE_ENABLED=true`, verify the HLS-only CMS, Gateway, shared client,
Admin uploader, member player and media Worker releases. Keep
`MEMBER_VIDEO_ENABLED=true`; do not change ordinary scan, retention or storage
settings as part of this cutover. Use a newly merged release checkpoint so its
immutable image and governance evidence identify the activation release.

The reviewed activation delta is limited to the API's two recording flags and
the dedicated recording Job's matching flags, runtime managed-identity selector,
4 vCPU / 8 GiB, 21,600-second timeout and zero platform retries. ARM may express
unchanged registry and Key Vault references differently; resolve those references
before accepting the preview. Any other resource change requires investigation.

After CI/CD succeeds, verify the API's ready revision, the exact recording Job
image/configuration and an unauthenticated HLS object denial. Then exercise a
controlled CLI package and browser source through server-confirmed readiness.
Publication/notification and real-device acceptance are separate checks, not
implied by a successful deployment. On failure, use the release rollback path;
never drop historical schema or delete source/R2 data to make a release pass.

The template creates `asset-api` in the existing `alive-env`, enables Dapr
with app id `asset-api`, creates a private Blob container, and assigns its
dedicated pull identity ACR pull, plus its system identity container-scoped
Blob contributor and account-scoped Blob delegator roles. Owner services use
Dapr; the dedicated LINE attachment Job uses authenticated internal ingress
with an Entra application audience and the `Asset.Invoke` app role.
Dapr uses `info` logging so managed-sidecar startup can be correlated with probe
failures during scaling. This does not enable Dapr API request logging or change
application health probes, permissions, resource allocations or scale limits.
Production scanning runs through the `asset-scan-worker` Container App. It
scales from zero on the `asset-scan` queue, polls once per second (the
app-level `pollingInterval`, shared by every scale rule), and returns to zero
after the 120-second cooldown. The legacy `asset-scan` Job remains
Manual with its queue trigger disabled for one compatibility release.

### Meeting warm windows

During meetings the scan worker is held at one replica by five KEDA cron scale
rules on `asset-scan-worker` (`desiredReplicas: '1'`, `timezone: 'Asia/Taipei'`).
Each window runs from 5 minutes before the meeting starts to 10 minutes after it
ends. KEDA uses the highest rule, so the replica count is
max(cron, `asset-scan` queue); uploads outside a window still scale from zero.

| rule | start | end | meetingKey (start) |
|---|---|---|---|
| `tue-fri-0625` | `25 6 * * 2,5` | `40 8 * * 2,5` | `morning-prayer` (06:30) |
| `thu-1155` | `55 11 * * 4` | `10 14 * * 4` | `gospel-meal` (12:00) |
| `fri-1855` | `55 18 * * 5` | `40 21 * * 5` | `discipleship-prayer` (19:00) |
| `sat-0855` | `55 8 * * 6` | `40 11 * * 6` | `kingdom-prayer` (09:00) |
| `sun-0855` | `55 8 * * 0` | `10 12 * * 0` | `sunday` (09:00) |

The windows are hard-coded in `main.bicep`. They do not follow meeting edits in
Operations: a meeting-time change requires editing these rules (and the
expected list in `scripts/meeting-cron-drift.test.py`) and releasing. The
release verifies the deployed rules against the template with
`scripts/verify-scan-worker-scale.py`.

`.github/workflows/meeting-cron-drift.yml` runs every Monday 01:00 UTC and on
demand without Azure credentials. It reads the public
`https://www.alive.org.tw/api/meeting-occurrences` and fails when any
`scheduled` occurrence in the next 28 days is not covered (5 minutes before
start to 10 minutes after end, Asia/Taipei) by one cron rule. A failure opens
or updates the issue "Meeting cron drift: asset-scan-worker" with a suggested
rule; a clean run closes it. Run it locally with
`python3 scripts/meeting-cron-drift.py` (exit 0 covered, 1 gap, 2 invalid input).

The per-minute `asset-scan-warmer` Job is retired. The release step "Retire scan
warmer Job" logs its last image reference and deletes it if it still exists
(incremental Bicep deployments never delete it); rerunning is a no-op. The
`asset-scan-warm` queue, its RBAC, `asset-scan-warmer-identity` and the
worker's pulse draining remain until a follow-up cleanup; nothing enqueues
pulses any more. To restore the warmer, revert the change and release: the
previous template recreates the Job with the then-current runtime image.
Clean supported images are processed only through the queue-triggered
`asset-derivative` Job. Terraform owns its stable queues, identity, RBAC, and
job configuration; the service release updates the job to the same immutable
runtime image as the API.

`asset-retention` runs daily at 19:00 UTC (03:00 Asia/Taipei) with mutations
enabled. The release workflow sets `RETENTION_SCHEDULE_ENABLED=true` and
`RETENTION_APPLY_ENABLED=true`; the first scheduled run also clears accumulated
expired LINE collection items. Review a fresh read-only preflight and verify
identity, network access and the fixed selection scope before enabling the
schedule. The worker excludes retention-exempt items and other namespaces.
Verify job results and database/Blob purge status after runs; investigate any
failed item. To suspend automatic cleanup, release with
`RETENTION_SCHEDULE_ENABLED=false` (Manual trigger).

Upload completion and an `asset.scan.requested.v1` outbox row commit in one
PostgreSQL transaction. The runtime sends that event to the `asset-scan`
Storage Queue with managed identity. Queue delivery is at-least-once; the
message contains only version, event ID, asset ID, and immutable Blob ETag.
The queue-scaled `asset-scan-worker` consumes the queue with managed identity,
validates the immutable Blob, and runs local `clamscan` against a read-only
signature snapshot. PostgreSQL records poison work before it is forwarded to
`asset-scan-poison`. Queue messages use an infinite TTL.

A clean scan result and an `asset.derivative.requested.v1` outbox row commit in
one PostgreSQL transaction. The runtime sends the event to `asset-derivative`.
The Job consumes one message per execution, verifies the asset ID and immutable
Blob ETag, and acknowledges ready, stale, deleted, or unsupported work
idempotently. Retryable failures stay invisible until the PostgreSQL retry time
instead of exhausting on raw queue deliveries. Terminal state and the durable
poison record commit together before forwarding to `asset-derivative-poison`.

`asset-clamav-signature-refresh` validates new databases, writes an immutable
generation to the private `asset-signatures` Blob container, and atomically
replaces `current.json`. The previous generation remains available for rollback.

## One-time production cutover

### Bulletin extractor authorization gate

Reuse the existing `hhc-web-api-identity`; do not create another identity or
broaden application consent. The optional `extractorClientId` and
`extractorObjectId` parameters must match that identity and are added to the
existing EasyAuth application/principal allowlists alongside LINE.

With the corresponding release variables configured, release first runs
`bash scripts/ensure-workload-app-role.sh check`. This verifies the exact
existing `Asset.Invoke` assignment without changing it. A missing assignment
stops release before ARM writes. Only a separately reviewed manual dispatch
with `approve_extractor_app_role=true` allows `apply`, under the existing
Entra deployment service principal. Missing authority, unexpected assignments
or ambiguous roles require platform-owner review, not additional consent.

Review the fresh what-if and exact role delta before that dispatch. After the
approved asset release, verify a real CMS managed-identity token's audience,
client/object IDs and `Asset.Invoke` role, then read a clean private asset owned
by `hhc-web-api` through EasyAuth. Do not enable the extractor Job until this
smoke passes. Local mock tests and Bicep compilation do not satisfy this gate.

The existing `asset` login becomes the DML-only runtime role. Migrations use
`asset_migrate` through a manual Container Apps job. Runtime and migration
database URLs are stored in separate RBAC Key Vaults.

1. Build the API and scan-only images and resolve their digests:

   ```sh
   az acr build -r alive -t alive/asset-api:bootstrap .
   digest="$(az acr repository show -n alive --image alive/asset-api:bootstrap --query digest -o tsv)"
   image="alive.azurecr.io/alive/asset-api@${digest}"
   az acr build -r alive -t alive/asset-scan:bootstrap -f Dockerfile.scan .
   scan_digest="$(az acr repository show -n alive --image alive/asset-scan:bootstrap --query digest -o tsv)"
   scan_image="alive.azurecr.io/alive/asset-scan@${scan_digest}"
   ```

2. Review and create only the vaults, identities, role assignments, and storage
   policy. This does not touch the running app:

   ```sh
   az deployment group what-if -g alive -f infra/main.bicep \
     -p storageAccountName=alivestoragebb99ee6e runtimeImage="$image" migrationImage="$image" scanWorkerImage="$scan_image" \
        deployRuntime=false deployMigrationJob=false provisionPermissions=true
   az deployment group create -g alive -f infra/main.bicep \
     -p storageAccountName=alivestoragebb99ee6e runtimeImage="$image" migrationImage="$image" scanWorkerImage="$scan_image" \
        deployRuntime=false deployMigrationJob=false provisionPermissions=true
   ```

3. Create the migration role, transfer schema ownership, restrict the runtime
   role, and write both vault secrets:

   ```sh
   ./scripts/bootstrap-migration-role.sh
   ```

4. Review and create the migration job without touching the runtime:

   ```sh
   az deployment group what-if -g alive -f infra/main.bicep \
     -p storageAccountName=alivestoragebb99ee6e runtimeImage="$image" migrationImage="$image" \
        deployRuntime=false deployMigrationJob=true provisionPermissions=false
   az deployment group create -g alive -f infra/main.bicep \
     -p storageAccountName=alivestoragebb99ee6e runtimeImage="$image" migrationImage="$image" \
        deployRuntime=false deployMigrationJob=true provisionPermissions=false
   ```

5. Trigger the manual GitHub `Production Release` workflow from `main` with
   confirmation `deploy-asset-api-production`. It runs migrations before
   replacing the runtime and rolls back only the runtime image on failure.

6. Confirm the scan and derivative queues and Jobs exist and all images use
   immutable digests.

7. Start the signature refresh Job, then send isolated clean and EICAR fixtures
   through the queue path. Clean must become `clean`; EICAR must become
   `infected` and remain unpublishable.

8. Deploy the runtime. Scan and derivative queue dispatch are enabled and the
   embedded scanner and derivative poller are disabled in production.

9. Upload a small supported image through the authenticated API, complete it,
   wait for scan state `clean` and all three variants `ready`, then delete the
   asset through the normal API. Confirm both derivative queues return to zero.

The template explicitly disables Defender for Storage; ClamAV is the only
malware scanner. `ASSET_ALLOW_DEV_CALLER_HEADER` remains false in Azure.
The live PostgreSQL server allows 50 connections. Production uses
`DB_MAX_OPEN_CONNS=4`; at three replicas asset-api consumes at most 12 and
leaves 38 for other services, migrations, and operations.

The template also preserves the live Container App inactive-revision limit,
Consumption workload profile, 30-day Blob soft delete, default
encryption scope, and the complete disabled Defender settings. Review the full
`what-if` before deployment; only the intended application and pool changes
should remain.

## Storage hardening gate

The current storage account may have consumers outside asset-api. Audit them
before disabling Shared Key. Once asset-api is the only confirmed owner and a
clean upload/download succeeds with managed identity:

```sh
az storage account update -g alive -n alivestoragebb99ee6e --allow-shared-key-access false
```

Do not change the storage firewall to default-deny until a private endpoint or
equivalent ACA subnet path has been deployed and verified.
The live account currently reports `minimumTlsVersion=TLS1_0`; raise it to
`TLS1_2` only after the shared-account consumer audit confirms compatibility.

## Recording Event Jobs: bootstrap and activation

Ordinary Contributor releases deploy `recording-events.bicep` without IAM writes.
The separate `recording-event-permissions.bicep` contains fifteen queue-scoped
assignments for existing API/worker identities and one custom message-write role;
no Job or application deployment. The additional three assignments give only the
worker `messages/write` on the work queues. Azure uses that DataAction for visibility
renewal; it can also modify/add message content, but grants no queue management,
poison-queue write or API write. Custom-role bootstrap additionally requires
`roleDefinitions/write`; ordinary CI gains no IAM-write privilege.
Its one-time operator bootstrap from merged code requires an exact reviewed
what-if and explicit approval, following the source-storage policy above.
`verify-recording-event-permissions.sh` gates activation on the actual grants and the exact complete custom-role permission
definition. The former twelve-role check was insufficient: a real source smoke
failed its first 30-second UpdateMessage with 403. Reactivation must prove a
successful real renewal and final ack, not just role readback.
See [the ordered cutover and rollback](../docs/reviews/recording-event-cutover-2026-10-19.md).

## Live Event Job resources

The live Event Job tier is selected by `RECORDING_LIVE_CPU` in the normal release:
`2` maps to 2 CPU/4 GiB; `4` maps to 4 CPU/8 GiB. The staged default remains `4`.
The same value reaches Provider what-if, Bicep deployment and Job resource
readback. Processing/preview tiers and trigger configuration are independent.
Changing the repository variable or Azure resources requires a separately
reviewed what-if and rollout; this source change does not select the target in
production. Roll back through the release path with `4`; the existing topology
snapshot also retains the exact previous resources. Do not switch while active
media claims are draining. See [handoff verification and rollout gates](../docs/reviews/recording-live-handoff-2026-10-25.md).

## Shared Job login cap (pending cutover)

Database-backed Jobs use runtime-vault `jobs-database-url`, authenticating as
`asset_jobs` with a shared server limit26. The API and persistent scan App keep
`database-url`; migration keeps its separate migration identity (limit1).
Provision/cap the Job-only login and validate all effective Job identities while
admission is paused before releasing this dependency. This task does not create
credentials or apply configuration. See azure-infra PR144 for the reviewed
operator preview and48-connection shared Job budget.

## API capacity and scaling

The API uses0.25vCPU/0.5GiB with a bounded8-open/4-idle SQL pool. Keep min1/max8,30s custom polling and300s stabilization/cooldown configuration. CPU60 remains paired with native HTTP target10. HTTP10 is a provisional default, not a measured capacity limit; the native metric, route coverage and useful scale-up timing still need validation. CPU-only headroom has not been established. Job logins are separately capped and must pass the shared48-login inventory/drain gate before this capacity release. API and persistent scanner budgets remain separate.

The local shared-PG test used900-history captures,50synthetic subjects and delayed dependency/replica startup: three120req/s-per-service trials passed for the paired CMS/Asset0.5CPU configuration. This is not managed ACA timing or end-to-end viewer acceptance; older failed cases remain in the incident report. Pool ceilings are stress bounds, not simultaneous allocated connections.
