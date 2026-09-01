#!/bin/sh
set -eu

workflow=.github/workflows/release.yml

grep -q 'workflow_dispatch:' "$workflow"
grep -q 'fail_openapi_before_pointer:' "$workflow"
grep -q '^  push:' "$workflow"
grep -q 'branches: \[main\]' "$workflow"
expected_paths_ignore='docs/**
.github/workflows/ci.yml'
actual_paths_ignore="$(awk '
  $0 == "    paths-ignore:" { in_paths_ignore = 1; next }
  in_paths_ignore && /^      - / { sub(/^      - /, ""); print; next }
  in_paths_ignore { exit }
' "$workflow")"
if [ "$actual_paths_ignore" != "$expected_paths_ignore" ]; then
  echo 'release docs-only paths-ignore policy mismatch' >&2
  exit 1
fi
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
trivy_image='ghcr.io/aquasecurity/trivy@sha256:62b1e65e8869bc4b4c6aa4fa2b21595256c7c2f6018a9d9ad61caf87187c1969'
grep -Fq "$trivy_image" .github/workflows/ci.yml
grep -Fq "$trivy_image" "$workflow"
grep -Fq 'fs --scanners vuln --severity HIGH,CRITICAL --ignore-unfixed --exit-code 1' .github/workflows/ci.yml
grep -Fq 'fs --scanners vuln --severity HIGH,CRITICAL --ignore-unfixed --exit-code 1' "$workflow"
grep -Fq 'image --severity HIGH,CRITICAL --ignore-unfixed --exit-code 1 asset-api:verify' .github/workflows/ci.yml
grep -Fq 'image --severity HIGH,CRITICAL --ignore-unfixed --exit-code 1 asset-scan:verify' .github/workflows/ci.yml
grep -Fq 'docker pull "$IMAGE_REF"' "$workflow"
grep -Fq 'docker pull "$SCAN_IMAGE_REF"' "$workflow"
grep -Fq 'image --severity HIGH,CRITICAL --ignore-unfixed --exit-code 1 "$IMAGE_REF"' "$workflow"
grep -Fq 'image --severity HIGH,CRITICAL --ignore-unfixed --exit-code 1 "$SCAN_IMAGE_REF"' "$workflow"
scan_line="$(grep -n 'name: Scan immutable images' "$workflow" | cut -d: -f1)"
what_if_line="$(grep -n 'name: Reject destructive infrastructure changes' "$workflow" | cut -d: -f1)"
test "$scan_line" -lt "$what_if_line"
grep -Fq "docker export \"\$(docker create asset-api:verify)\" | tar -tf - | grep -Fxq 'asset-derivative-worker'" "$workflow"
grep -Fq "docker export \"\$(docker create asset-api:verify)\" | tar -tf - | grep -Fxq 'asset-derivative-worker'" .github/workflows/ci.yml
grep -Fq 'docker run --rm --entrypoint clamscan asset-scan:verify --help' "$workflow"
for flag in --max-filesize --max-scansize --max-files --max-recursion --alert-exceeds-max --alert-encrypted; do
  grep -Fq -- "$flag" "$workflow"
done
grep -q "replicaTimeout: 720" infra/main.bicep
grep -q 'param deployScanWorker bool = false' infra/main.bicep
grep -q 'param provisionScanWarmInfrastructure bool = false' infra/main.bicep
grep -q "name: 'asset-scan-worker'" infra/main.bicep
grep -q "name: 'asset-scan-warmer'" infra/main.bicep
grep -q "name: 'asset-scan-warmer-identity'" infra/main.bicep
grep -q "cronExpression: '\*/1 \* \* \* \*'" infra/main.bicep
grep -q 'deployScanWarmer=false' "$workflow"
grep -q "name: 'ASSET_SCAN_IDLE_POLL', value: '1s'" infra/main.bicep
grep -q 'pollingInterval: 1' infra/main.bicep
grep -q 'cooldownPeriod: 120' infra/main.bicep
grep -q 'minReplicas: 0' infra/main.bicep
grep -q 'maxReplicas: 5' infra/main.bicep
grep -q "queueName: 'asset-scan-warm'" infra/main.bicep
scan_job_block="$(sed -n '/resource scanJob /,/^}/p' infra/main.bicep)"
printf '%s\n' "$scan_job_block" | grep -q "triggerType: 'Manual'"
if printf '%s\n' "$scan_job_block" | grep -q 'eventTriggerConfig'; then
  echo 'compatibility scan Job queue trigger must stay disabled' >&2
  exit 1
fi
grep -q "CLAMAV_SCAN_TIMEOUT', value: '10m'" infra/main.bicep
grep -q "ASSET_SCAN_MAX_FILE_SIZE_BYTES', value: '209715200'" infra/main.bicep
asset_runtime_block="$(awk "/name: 'asset-api'/ { seen++; if (seen == 2) capture = 1 } capture { print } capture && /probes:/ { exit }" infra/main.bicep)"
printf '%s\n' "${asset_runtime_block}" | grep -Fq "cpu: json('0.25')" &&
  printf '%s\n' "${asset_runtime_block}" | grep -Fq "memory: '0.5Gi'" || {
  echo 'asset-api runtime must remain at 0.25 CPU / 0.5Gi' >&2
  exit 1
}
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
test "$(grep -c 'deployScanWorker=true' "$workflow")" = 2
test "$(grep -c 'provisionScanWarmInfrastructure=true' "$workflow")" = 2
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
grep -q 'SCAN_WORKER_APP_NAME: asset-scan-worker' "$workflow"
grep -q 'az containerapp show.*"$SCAN_WORKER_APP_NAME"' "$workflow"
grep -q 'az containerapp job show.*"$DERIVATIVE_JOB_NAME"' "$workflow"
grep -q '"$derivative_image" == "$IMAGE_REF"' "$workflow"
grep -q 'PREVIOUS_IMAGE_REF=' "$workflow"
grep -q 'az containerapp revision copy' "$workflow"
grep -q -- '--image "$PREVIOUS_IMAGE_REF"' "$workflow"
grep -q '^  publish_openapi:' "$workflow"
publish_job="$(sed -n '/^  publish_openapi:/,$p' "$workflow")"
printf '%s\n' "$publish_job" | grep -q 'needs: deploy'
printf '%s\n' "$publish_job" | grep -q 'environment: production'
printf '%s\n' "$publish_job" | grep -q 'id-token: write'
printf '%s\n' "$publish_job" | grep -q 'API_DOCS_AZURE_CLIENT_ID'
printf '%s\n' "$publish_job" | grep -q 'api-docs-asset-api'
printf '%s\n' "$publish_job" | grep -q 'needs.deploy.outputs.image'
printf '%s\n' "$publish_job" | grep -q 'specs/${GITHUB_SHA}/openapi.yaml'
printf '%s\n' "$publish_job" | grep -q 'inputs.fail_openapi_before_pointer && github.run_attempt == 1'
printf '%s\n' "$publish_job" | grep -q -- '--overwrite false'
printf '%s\n' "$publish_job" | grep -q -- '--name current.json'
printf '%s\n' "$publish_job" | grep -q -- '--overwrite true'
workflow_body="$(sed -n '/^          spec_blob="specs\//,$p' "$workflow" | sed 's/^          //')"
run_openapi_pointer_guard_case() {
  pointer_json="$1"
  candidate_run_id="$2"
  expected="$3"
  spec_fixture="${4:-missing}"
  case_dir="$(mktemp -d)"
  mkdir "$case_dir/pointer"
  ln -s "$PWD/docs" "$case_dir/docs"
  if [ "$pointer_json" != missing ]; then
    printf '%s\n' "$pointer_json" > "$case_dir/pointer/current.json"
    cp "$case_dir/pointer/current.json" "$case_dir/expected-current.json"
  fi
  case "$spec_fixture" in
    identical)
      mkdir -p "$case_dir/blobs/specs/0123456789abcdef0123456789abcdef01234567"
      cp docs/openapi.yaml "$case_dir/blobs/specs/0123456789abcdef0123456789abcdef01234567/openapi.yaml"
      ;;
    different)
      mkdir -p "$case_dir/blobs/specs/0123456789abcdef0123456789abcdef01234567"
      printf 'different spec\n' > "$case_dir/blobs/specs/0123456789abcdef0123456789abcdef01234567/openapi.yaml"
      ;;
  esac

  if output="$(POINTER_CASE_DIR="$case_dir" WORKFLOW_BODY="$workflow_body" GITHUB_RUN_ID="$candidate_run_id" GITHUB_SHA=0123456789abcdef0123456789abcdef01234567 GITHUB_REPOSITORY=HallelujahHomeChurch/asset-api RELEASE_COMMIT=0123456789abcdef0123456789abcdef01234567 RELEASE_IMAGE=alive.azurecr.io/alive/asset-api@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef FAIL_OPENAPI_BEFORE_POINTER=false bash -e -c '
    az() {
      command="$1 $2 $3"
      name=""
      file=""
      overwrite=""
      while [ "$#" -gt 0 ]; do
        case "$1" in
          --name) name="$2"; shift 2 ;;
          --file) file="$2"; shift 2 ;;
          --overwrite) overwrite="$2"; shift 2 ;;
          *) shift ;;
        esac
      done
      blob="$POINTER_CASE_DIR/pointer/current.json"
      if [ "$name" != current.json ]; then
        blob="$POINTER_CASE_DIR/blobs/$name"
      fi
      case "$command" in
        "storage blob exists")
          if [ -f "$blob" ]; then printf true; else printf false; fi
          ;;
        "storage blob download")
          cp "$blob" "$file"
          if [ "$name" != current.json ]; then printf spec-download\\n >> "$POINTER_CASE_DIR/events"; fi
          ;;
        "storage blob upload")
          if [ -e "$blob" ] && [ "$overwrite" = false ]; then return 1; fi
          mkdir -p "$(dirname "$blob")"
          cp "$file" "$blob"
          if [ "$name" = current.json ]; then
            printf pointer-upload\\n >> "$POINTER_CASE_DIR/uploads"
          else
            printf spec-upload\\n >> "$POINTER_CASE_DIR/uploads"
          fi
          ;;
      esac
    }
    cd "$POINTER_CASE_DIR"
    eval "$WORKFLOW_BODY"
  ' 2>&1)"; then
    status=0
  else
    status=$?
  fi
  pointer_uploaded=false
  if [ -e "$case_dir/uploads" ] && grep -Fxq pointer-upload "$case_dir/uploads"; then
    pointer_uploaded=true
  fi

  case "$expected" in
    upload)
      test "$status" -eq 0
      test "$pointer_uploaded" = true
      grep -Fq "/runs/$candidate_run_id\"" "$case_dir/pointer/current.json"
      ;;
    noop)
      test "$status" -eq 0
      test "$pointer_uploaded" = false
      cmp "$case_dir/expected-current.json" "$case_dir/pointer/current.json"
      ;;
    invalid-pointer)
      test "$status" -ne 0
      test "$pointer_uploaded" = false
      cmp "$case_dir/expected-current.json" "$case_dir/pointer/current.json"
      printf '%s\n' "$output" | grep -Fq 'Invalid existing API docs pointer: expected canonical GitHub workflow run ID'
      ;;
    invalid-candidate)
      test "$status" -ne 0
      test "$pointer_uploaded" = false
      printf '%s\n' "$output" | grep -Fq 'Invalid GITHUB_RUN_ID: expected canonical positive decimal'
      ;;
    spec-idempotent)
      test "$status" -eq 0
      grep -Fxq spec-download "$case_dir/events"
      if [ -e "$case_dir/uploads" ] && grep -Fxq spec-upload "$case_dir/uploads"; then
        exit 1
      fi
      grep -Fxq pointer-upload "$case_dir/uploads"
      grep -Fq "/runs/$candidate_run_id\"" "$case_dir/pointer/current.json"
      ;;
    spec-mismatch)
      test "$status" -ne 0
      test "$pointer_uploaded" = false
      cmp "$case_dir/expected-current.json" "$case_dir/pointer/current.json"
      ;;
  esac
  rm -rf "$case_dir"
}

valid_pointer='{"releaseUrl":"https://github.com/HallelujahHomeChurch/asset-api/actions/runs/20"}'
run_openapi_pointer_guard_case missing 20 upload
run_openapi_pointer_guard_case "$valid_pointer" 21 spec-idempotent identical
run_openapi_pointer_guard_case "$valid_pointer" 21 spec-mismatch different
run_openapi_pointer_guard_case "$valid_pointer" 19 noop
run_openapi_pointer_guard_case "$valid_pointer" 20 noop
run_openapi_pointer_guard_case "$valid_pointer" 21 upload
run_openapi_pointer_guard_case '{' 22 invalid-pointer
run_openapi_pointer_guard_case '{}' 22 invalid-pointer
run_openapi_pointer_guard_case '{"releaseUrl":null}' 22 invalid-pointer
run_openapi_pointer_guard_case '{"releaseUrl":"https://github.com/HallelujahHomeChurch/asset-api/actions/runs/09"}' 22 invalid-pointer
run_openapi_pointer_guard_case '{"releaseUrl":"https://github.com/HallelujahHomeChurch/asset-api/actions/runs/0"}' 22 invalid-pointer
run_openapi_pointer_guard_case '{"releaseUrl":"https://github.com/HallelujahHomeChurch/asset-api/actions/runs/99999999999999999999"}' 100000000000000000000 upload
run_openapi_pointer_guard_case missing 0 invalid-candidate
run_openapi_pointer_guard_case missing 01 invalid-candidate
printf '%s\n' "$publish_job" | grep -q 'pointer_exists="$(az storage blob exists'
printf '%s\n' "$publish_job" | grep -q 'current_pointer="$(mktemp)"'
printf '%s\n' "$publish_job" | grep -Fq 'Invalid GITHUB_RUN_ID: expected canonical positive decimal'
printf '%s\n' "$publish_job" | grep -Fq 'Invalid existing API docs pointer: expected canonical GitHub workflow run ID'
printf '%s\n' "$publish_job" | grep -Fq 'exit 0'
ready_line="$(grep -n -- '- name: Verify derivative job release' "$workflow" | cut -d: -f1)"
publish_line="$(grep -n '^  publish_openapi:' "$workflow" | cut -d: -f1)"
guard_line="$(grep -nF 'pointer_exists="$(az storage blob exists' "$workflow" | cut -d: -f1)"
guard_exit_line="$(awk '/skipping stale or rerun publication/ { getline; if ($0 ~ /^[[:space:]]*exit 0$/) print NR }' "$workflow")"
pointer_upload_line="$(awk '/az storage blob upload/ { upload = 1 } upload && /--file current.json/ { print NR; exit }' "$workflow")"
test "$ready_line" -lt "$publish_line"
test "$guard_line" -lt "$guard_exit_line"
test "$guard_exit_line" -lt "$pointer_upload_line"
test "$publish_line" -lt "$pointer_upload_line"
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
