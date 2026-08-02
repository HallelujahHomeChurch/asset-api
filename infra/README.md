# Asset API Azure deployment

The template creates `asset-api` in the existing `alive-env`, enables Dapr
with app id `asset-api`, creates a private Blob container, and assigns its
dedicated pull identity ACR pull, plus its system identity container-scoped
Blob contributor and account-scoped Blob delegator roles. Owner services use
Dapr; the dedicated LINE attachment Job uses authenticated internal ingress
with an Entra application audience and the `Asset.Invoke` app role.
Production scanning runs only through the Azure queue-triggered scan Job.

Upload completion and an `asset.scan.requested.v1` outbox row commit in one
PostgreSQL transaction. The runtime sends that event to the `asset-scan`
Storage Queue with managed identity. Queue delivery is at-least-once; the
message contains only version, event ID, asset ID, and immutable Blob ETag.
The event-triggered `asset-scan` Job consumes the queue with managed identity,
validates the immutable Blob, and runs local `clamscan` against a read-only
signature snapshot. PostgreSQL records poison work before it is forwarded to
`asset-scan-poison`. Queue messages use an infinite TTL.

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

6. Confirm both queues and Jobs exist and all images use immutable digests.

7. Start the signature refresh Job, then send isolated clean and EICAR fixtures
   through the queue path. Clean must become `clean`; EICAR must become
   `infected` and remain unpublishable.

8. Deploy the runtime. Queue dispatch is always enabled and the embedded
   scanner is always disabled in production.

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
