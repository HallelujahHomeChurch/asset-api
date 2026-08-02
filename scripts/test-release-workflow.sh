#!/bin/sh
set -eu

workflow=.github/workflows/release.yml

grep -q 'workflow_dispatch:' "$workflow"
grep -q '^  push:' "$workflow"
grep -q 'branches: \[main\]' "$workflow"
grep -Fq "github.event_name == 'push' && 'deploy-asset-api-production' || inputs.confirmation" "$workflow"
grep -Fq "github.event_name == 'push' && 'true' || inputs.activate_queue_scanning" "$workflow"
grep -q 'deploy-asset-api-production' "$workflow"
grep -q 'environment: production' "$workflow"
grep -q 'Verify isolated runtime prerequisites' "$workflow"
grep -q 'az resource show --ids' "$workflow"
if grep -q 'az keyvault secret show' "$workflow"; then
  echo 'release preflight must not read secret values' >&2
  exit 1
fi
grep -q 'IMAGE_REF=.*@${digest}' "$workflow"
grep -q 'SCAN_IMAGE_REF=.*@${scan_digest}' "$workflow"
grep -q 'Dockerfile.scan' "$workflow"
grep -q 'activate_queue_scanning' "$workflow"
grep -q 'true) embedded_scan_enabled=false' "$workflow"
grep -q 'false) embedded_scan_enabled=true' "$workflow"
test "$(grep -c 'embeddedScanEnabled="$EMBEDDED_SCAN_ENABLED"' "$workflow")" = 2
if grep -q 'ACTIVATE_QUEUE_SCANNING/true/false' "$workflow"; then
  echo 'queue and embedded scanners must use explicit inverse modes' >&2
  exit 1
fi
if grep -Eq 'scope: (scanQueue|scanPoisonQueue|assetContainer|signatureContainer)!' infra/main.bicep; then
  echo 'conditional storage resources must use explicit nested role-assignment types' >&2
  exit 1
fi
for scope in scanQueueScope scanPoisonQueueScope assetContainerScope signatureContainerScope; do
  grep -q "scope: $scope" infra/main.bicep
done
test "$(grep -c "'scoped-v2'" infra/main.bicep)" = 7
grep -q "'storage-queue-data-reader'" infra/main.bicep
grep -q "roleDefinitionName=='Storage Queue Data Reader'" "$workflow"
grep -q 'ASSET_WORKLOAD_AUDIENCE' "$workflow"
grep -q 'ASSET_WORKLOAD_CLIENT_ID' "$workflow"
grep -q 'LINE_ATTACHMENT_CLIENT_ID' "$workflow"
grep -q 'LINE_ATTACHMENT_OBJECT_ID' "$workflow"
grep -q 'workloadAuthAudience="$ASSET_WORKLOAD_AUDIENCE"' "$workflow"
grep -q 'workloadAuthClientId="$ASSET_WORKLOAD_CLIENT_ID"' "$workflow"
grep -q 'az deployment group what-if' "$workflow"
grep -q './scripts/check-what-if.sh what-if.json' "$workflow"
grep -q 'manageSharedInfrastructure=false' "$workflow"
grep -q 'exposedPort: 0' infra/main.bicep
grep -q 'latestRevision: true' infra/main.bicep
grep -q 'isAutoProvisioned: false' infra/main.bicep
grep -q 'az containerapp job start' "$workflow"
grep -q 'PREVIOUS_IMAGE_REF=' "$workflow"
grep -q 'az containerapp revision copy' "$workflow"
grep -q -- '--image "$PREVIOUS_IMAGE_REF"' "$workflow"
grep -q "runtimeKeyVaultName string = 'alive-asset-runtime-kv'" infra/main.bicep
grep -q "migrationKeyVaultName string = 'alive-asset-migrate-kv'" infra/main.bicep
grep -q "name: 'asset-migrate'" infra/main.bicep
grep -q "command: \\['/asset-migrate'\\]" infra/main.bicep
grep -q 'enableRbacAuthorization: true' infra/main.bicep
grep -q 'test-migration-policy-test.sh' .github/workflows/ci.yml
grep -q 'test-what-if-policy.sh' .github/workflows/ci.yml

if grep -Eiq 'migrate[[:space:]_-]*down|migration[[:space:]_-]*rollback' "$workflow"; then
  echo 'release workflow must not roll back database migrations automatically' >&2
  exit 1
fi
