# Asset API Azure deployment

The template creates `asset-api` in the existing `alive-env`, enables Dapr
with app id `asset-api`, creates a private Blob container, and assigns its
dedicated pull identity ACR pull, plus its system identity container-scoped
Blob contributor and account-scoped Blob delegator roles. Owner services use
Dapr; the dedicated LINE attachment Job uses authenticated internal ingress
with an Entra application audience and the `Asset.Invoke` app role.
Production scanning runs through the `asset-scan-worker` Container App. It
scales from zero on either `asset-scan` or `asset-scan-warm`, polls once per
second while active, and returns to zero after the 120-second cooldown. The
warm queue is a scale signal only; the worker consumes only `asset-scan`.
Warm messages expire after 120 seconds. The legacy `asset-scan` Job remains
Manual with its queue trigger disabled for one compatibility release.
The `asset-scan-warmer` scheduled Job runs once per minute, reads meeting windows
through hhc-web-api internal ingress using its dedicated managed identity, and
can only enqueue the warm queue. It has no Dapr app-channel secret, database,
Blob, business queue, or Azure management-plane access.
Clean supported images are processed only through the queue-triggered
`asset-derivative` Job. Terraform owns its stable queues, identity, RBAC, and
job configuration; the service release updates the job to the same immutable
runtime image as the API.

`asset-retention` is deployed as a Manual Job with mutations disabled. The
release workflow keeps `RETENTION_SCHEDULE_ENABLED=false` and
`RETENTION_APPLY_ENABLED=false`, so deployment alone cannot schedule cleanup or
delete media. After verifying identity, network access, and a read-only
preflight, explicit production approval is required before enabling the 19:00
UTC (03:00 Asia/Taipei) schedule or mutations. Keep the schedule disabled for
the first approved bounded mutation run, verify its database and Blob purge
results, then enable the recurring schedule separately.

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
