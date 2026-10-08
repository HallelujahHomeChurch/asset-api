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
Production scanning runs through the `asset-scan-worker` Container App. It
scales from zero on either `asset-scan` or `asset-scan-warm`, polls once per
second while active, and returns to zero after the 120-second cooldown. The
worker deletes warm pulses after the scan queue is empty so the queue scaler
does not retain a stale approximate count. Warm messages also expire after
120 seconds. The legacy `asset-scan` Job remains
Manual with its queue trigger disabled for one compatibility release.
The `asset-scan-warmer` scheduled Job runs once per minute, reads meeting windows
through Operations internal ingress using its dedicated managed identity, and
can only enqueue the warm queue. It has no Dapr app-channel secret, database,
Blob, business queue, or Azure management-plane access.
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
