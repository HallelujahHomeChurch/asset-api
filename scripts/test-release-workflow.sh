#!/bin/sh
set -eu

workflow=.github/workflows/release.yml

grep -q 'workflow_dispatch:' "$workflow"
grep -q '^  push:' "$workflow"
grep -q 'branches: \[main\]' "$workflow"
grep -Fq "github.event_name == 'push' && 'deploy-asset-api-production' || inputs.confirmation" "$workflow"
grep -q 'ACTIVATE_QUEUE_SCANNING: "true"' "$workflow"
grep -q 'EMBEDDED_SCAN_ENABLED: "false"' "$workflow"
grep -q 'DEPLOY_RETENTION_JOB: "true"' "$workflow"
grep -q 'RETENTION_SCHEDULE_ENABLED: "false"' "$workflow"
grep -q 'RETENTION_APPLY_ENABLED: "false"' "$workflow"
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
grep -Fq "docker export \"\$(docker create asset-api:verify)\" | tar -tf - | grep -Fxq 'asset-derivative-worker'" "$workflow"
grep -Fq "docker export \"\$(docker create asset-api:verify)\" | tar -tf - | grep -Fxq 'asset-derivative-worker'" .github/workflows/ci.yml
grep -Fq 'docker run --rm --entrypoint clamscan asset-scan:verify --help' "$workflow"
for flag in --max-filesize --max-scansize --max-files --max-recursion --alert-exceeds-max --alert-encrypted; do
  grep -Fq -- "$flag" "$workflow"
done
grep -q "replicaTimeout: 720" infra/main.bicep
grep -q "CLAMAV_SCAN_TIMEOUT', value: '10m'" infra/main.bicep
grep -q "ASSET_SCAN_MAX_FILE_SIZE_BYTES', value: '209715200'" infra/main.bicep
grep -q "CLAMAV_MAX_FILE_SIZE_BYTES', value: '209715200'" infra/main.bicep
grep -q "CLAMAV_MAX_SCAN_SIZE_BYTES', value: '1073741824'" infra/main.bicep
grep -q "CLAMAV_MAX_FILES', value: '10000'" infra/main.bicep
grep -q "CLAMAV_MAX_RECURSION', value: '32'" infra/main.bicep
if grep -q 'activate_queue_scanning' "$workflow"; then
  echo 'production releases must not expose the retired embedded scanner' >&2
  exit 1
fi
if grep -Eq '172\.16\.65\.5|CLAMAV_HOST|AllowACAtoClamAV|DenyOtherVNetToClamAV' infra/main.bicep; then
  echo 'production infrastructure must not depend on the retired office scanner' >&2
  exit 1
fi
test "$(grep -c 'embeddedScanEnabled="$EMBEDDED_SCAN_ENABLED"' "$workflow")" = 2
test "$(grep -c 'deployRetentionJob="$DEPLOY_RETENTION_JOB"' "$workflow")" = 2
test "$(grep -c 'deployDerivativeJob=true' "$workflow")" = 2
test "$(grep -c 'retentionScheduleEnabled="$RETENTION_SCHEDULE_ENABLED"' "$workflow")" = 2
test "$(grep -c 'retentionApplyEnabled="$RETENTION_APPLY_ENABLED"' "$workflow")" = 2
grep -Fq 'for name in RETENTION_SCHEDULE_ENABLED RETENTION_APPLY_ENABLED; do' "$workflow"
grep -Fq 'case "${!name}" in' "$workflow"
grep -Fq 'true|false) ;;' "$workflow"
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
grep -q 'asset-derivative-identity' "$workflow"
grep -q 'asset-derivative-poison' "$workflow"
grep -q "roleDefinitionName=='Storage Queue Data Message Processor'" "$workflow"
grep -q "roleDefinitionName=='Storage Blob Data Contributor'" "$workflow"
grep -q 'ASSET_WORKLOAD_AUDIENCE' "$workflow"
grep -q 'ASSET_WORKLOAD_CLIENT_ID' "$workflow"
grep -Fqx "var workloadAuthIssuer = 'https://sts.windows.net/\${subscription().tenantId}/'" infra/main.bicep
grep -Fqx "            { name: 'ASSET_WORKLOAD_ISSUER', value: workloadAuthEnabled ? workloadAuthIssuer : '' }" infra/main.bicep
grep -Fqx '          openIdIssuer: workloadAuthIssuer' infra/main.bicep
grep -q 'LINE_ATTACHMENT_CLIENT_ID' "$workflow"
grep -q 'LINE_ATTACHMENT_OBJECT_ID' "$workflow"
grep -q 'workloadAuthAudience="$ASSET_WORKLOAD_AUDIENCE"' "$workflow"
grep -q 'workloadAuthClientId="$ASSET_WORKLOAD_CLIENT_ID"' "$workflow"
grep -q 'ASSET_READER_CALLER_APP_ID: api-gateway' "$workflow"
test "$(grep -c 'readerCallerAppId="$ASSET_READER_CALLER_APP_ID"' "$workflow")" = 2
grep -q "param readerCallerAppId string = 'api-gateway'" infra/main.bicep
grep -q "name: 'ASSET_READER_CALLER_APP_ID', value: readerCallerAppId" infra/main.bicep
if grep -q "ASSET_ALLOWED_CALLERS.*api-gateway" infra/main.bicep; then
  echo 'api-gateway must not be admitted to private asset routes' >&2
  exit 1
fi
for path in \
  "'/api/assets/collections'" \
  "'/api/assets/collections/*/changes'" \
  "'/api/assets/collections/*/items/*'" \
  "'/api/assets/collections/*/items/*/content-ticket'" \
  "'/api/assets/collections/*/items/*/content'" \
  "'/api/assets/content'"; do
  grep -Fq "$path" infra/main.bicep
done
if grep -Fq "'/api/assets/collections/*'" infra/main.bicep; then
  echo 'collection reader exclusion must not cover the whole prefix' >&2
  exit 1
fi
grep -q 'az deployment group what-if' "$workflow"
grep -q './scripts/check-what-if.sh what-if.json' "$workflow"
grep -q 'manageSharedInfrastructure=false' "$workflow"
grep -q 'exposedPort: 0' infra/main.bicep
grep -q 'latestRevision: true' infra/main.bicep
grep -q 'isAutoProvisioned: false' infra/main.bicep
grep -q 'az containerapp job start' "$workflow"
grep -q 'DERIVATIVE_JOB_NAME: asset-derivative' "$workflow"
grep -q 'az containerapp job show.*"$DERIVATIVE_JOB_NAME"' "$workflow"
grep -q '"$derivative_image" == "$IMAGE_REF"' "$workflow"
grep -q 'PREVIOUS_IMAGE_REF=' "$workflow"
grep -q 'az containerapp revision copy' "$workflow"
grep -q -- '--image "$PREVIOUS_IMAGE_REF"' "$workflow"
grep -q "runtimeKeyVaultName string = 'alive-asset-runtime-kv'" infra/main.bicep
grep -q "migrationKeyVaultName string = 'alive-asset-migrate-kv'" infra/main.bicep
grep -q "name: 'asset-migrate'" infra/main.bicep
grep -q "command: \\['/asset-migrate'\\]" infra/main.bicep
grep -q 'param deployRetentionJob bool = false' infra/main.bicep
grep -q 'param retentionScheduleEnabled bool = false' infra/main.bicep
grep -q 'param retentionApplyEnabled bool = false' infra/main.bicep
grep -q "name: 'asset-retention'" infra/main.bicep
grep -q "cronExpression: '0 19 \* \* \*'" infra/main.bicep
grep -q 'var retentionTriggerConfiguration = retentionScheduleEnabled ? {' infra/main.bicep
grep -q "triggerType: 'Schedule'" infra/main.bicep
grep -q "triggerType: 'Manual'" infra/main.bicep
grep -q 'configuration: union({' infra/main.bicep
grep -q '}, retentionTriggerConfiguration)' infra/main.bicep
grep -q "command: \\['/asset-retention-worker'\\]" infra/main.bicep
grep -q "name: 'ASSET_RETENTION_APPLY_ENABLED', value: string(retentionApplyEnabled)" infra/main.bicep
grep -q 'enableRbacAuthorization: true' infra/main.bicep
grep -q 'test-migration-policy-test.sh' .github/workflows/ci.yml
grep -q 'test-what-if-policy.sh' .github/workflows/ci.yml
grep -q 'param deployDerivativeJob bool = false' infra/main.bicep
grep -q "name: 'asset-derivative-identity'" infra/main.bicep
grep -q "name: 'asset-derivative'" infra/main.bicep
grep -q "name: 'asset-derivative-poison'" infra/main.bicep
grep -q "command: \['/asset-derivative-worker'\]" infra/main.bicep
grep -q 'minExecutions: 0' infra/main.bicep
grep -q 'maxExecutions: 1' infra/main.bicep
grep -q "queueName: 'asset-derivative'" infra/main.bicep
grep -q "queueLength: '1'" infra/main.bicep
grep -q "name: 'ASSET_DERIVATIVE_QUEUE_URL'" infra/main.bicep

migration_line="$(grep -n -- '- name: Run migrations' "$workflow" | cut -d: -f1)"
deploy_line="$(grep -n -- '- name: Deploy API' "$workflow" | cut -d: -f1)"
test "$migration_line" -lt "$deploy_line"
test -f internal/migrations/sql/014_asset_derivative_outbox.sql

if grep -Eiq 'migrate[[:space:]_-]*down|migration[[:space:]_-]*rollback' "$workflow"; then
  echo 'release workflow must not roll back database migrations automatically' >&2
  exit 1
fi
