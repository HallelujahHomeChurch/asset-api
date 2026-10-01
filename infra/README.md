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

`asset-retention` is deployed as a Manual Job with mutations enabled. The
release workflow keeps `RETENTION_SCHEDULE_ENABLED=false` and
`RETENTION_APPLY_ENABLED=true`; deployment cannot schedule cleanup, and an
operator must explicitly start each mutation run. Verify identity, network
access, a fresh read-only preflight, and the fixed selection scope before each
run. Keep the schedule disabled, verify database and Blob purge results, and
stop on an unexpected scope or any failed item. Enabling the 19:00 UTC (03:00
Asia/Taipei) schedule remains a separate production decision.

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
