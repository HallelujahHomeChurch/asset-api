# Asset API Azure deployment

The template creates `asset-api` in the existing `alive-env`, enables Dapr
with app id `asset-api`, creates a private Blob container, and assigns its
dedicated pull identity ACR pull, plus its system identity container-scoped
Blob contributor and account-scoped Blob delegator roles. No Container Apps ingress is created;
gateway and owner services invoke it through Dapr.
It also allows only the ACA subnet (`172.16.66.0/23`) to reach clamd at
`172.16.65.5:3310`.

## First deployment

1. Create the `asset` database and least-privilege login on the existing
   private PostgreSQL server at `172.16.68.4`. The role owns the database and
   its public schema:

   ```sql
   CREATE ROLE asset LOGIN PASSWORD 'REDACTED';
   CREATE DATABASE asset OWNER asset;
   \connect asset
   ALTER SCHEMA public OWNER TO asset;
   GRANT USAGE, CREATE ON SCHEMA public TO asset;
   ```
2. Build the initial image:

   ```sh
   az acr build -r alive -t alive/asset-api:latest .
   ```

3. Prepare the ignored parameter file and deploy:

   ```sh
   export ASSET_DATABASE_URL='postgres://asset:REDACTED@172.16.68.4:5432/asset?sslmode=require'
   cp infra/main.bicepparam.example infra/main.bicepparam
   az bicep build --file infra/main.bicep
   az deployment group what-if -g alive -f infra/main.bicep -p infra/main.bicepparam
   az deployment group create -g alive -f infra/main.bicep -p infra/main.bicepparam
   ```

4. Confirm the revision is ready, Dapr app id is `asset-api`, ingress is
   disabled, and the app identity has all three role assignments.
5. Prove TCP access from the ACA environment to `172.16.65.5:3310` and confirm
   current ClamAV signatures before accepting uploads.

The template explicitly disables Defender for Storage; ClamAV is the only
malware scanner. `ASSET_ALLOW_DEV_CALLER_HEADER` remains false in Azure.
The live PostgreSQL server allows 50 connections. Production uses
`DB_MAX_OPEN_CONNS=4`; at three replicas asset-api consumes at most 12 and
leaves 38 for other services, migrations, and operations.

The template also preserves the live Container App inactive-revision limit,
Consumption workload profile, disabled Blob soft-delete policy, default
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
