#!/bin/sh
set -eu

source_preview='{"status":"Succeeded","changes":[{"resourceId":"/subscriptions/00000000-0000-0000-0000-000000000000/resourceGroups/alive/providers/Microsoft.Storage/storageAccounts/aliverecordingsprod","changeType":"Create"}]}'
printf '%s' "$source_preview" | ./scripts/check-source-what-if.sh /dev/stdin
for change in Modify Delete Unsupported; do
  if printf '%s' "$source_preview" | jq --arg change "$change" '.changes[0].changeType=$change' | ./scripts/check-source-what-if.sh /dev/stdin; then
    echo "source bootstrap accepted $change" >&2
    exit 1
  fi
done
for resource in aliverecordingsprod-other alivestoragebb99ee6e; do
  if printf '%s' "$source_preview" | jq --arg resource "$resource" '.changes[0].resourceId |= sub("aliverecordingsprod$"; $resource)' | ./scripts/check-source-what-if.sh /dev/stdin; then
    echo "source bootstrap accepted unrelated storage" >&2
    exit 1
  fi
done

tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

printf '%s\n' '{"changes":[
  {"resourceId":"/subscriptions/test/resourceGroups/alive/providers/Microsoft.App/containerApps/asset-api","changeType":"Modify","delta":[{"path":"properties.runningStatus","propertyChangeType":"Delete"},{"path":"properties.template.revisionSuffix","propertyChangeType":"Delete"}]},
  {"resourceId":"/subscriptions/test/resourceGroups/alive/providers/Microsoft.App/jobs/asset-migrate","changeType":"Modify","delta":[]},
  {"resourceId":"/subscriptions/test/resourceGroups/alive/providers/Microsoft.App/jobs/asset-scan","changeType":"Modify","delta":[
    {"path":"properties.configuration.eventTriggerConfig.parallelism","propertyChangeType":"Delete"},
    {"path":"properties.configuration.eventTriggerConfig.replicaCompletionCount","propertyChangeType":"Delete"},
    {"path":"properties.configuration.eventTriggerConfig.scale.pollingInterval","propertyChangeType":"Delete"},
    {"path":"properties.configuration.eventTriggerConfig.scale.rules","propertyChangeType":"Delete"}
  ]},
  {"resourceId":"/subscriptions/test/resourceGroups/alive/providers/Microsoft.App/jobs/asset-derivative","changeType":"Modify","delta":[]},
  {"resourceId":"/subscriptions/test/resourceGroups/alive/providers/Microsoft.App/jobs/asset-clamav-signature-refresh","changeType":"Create","delta":[]},
  {"resourceId":"/subscriptions/test/resourceGroups/alive/providers/Microsoft.App/jobs/asset-retention","changeType":"Create","delta":[]},
  {"resourceId":"/subscriptions/test/resourceGroups/alive/providers/Microsoft.App/jobs/asset-recording-validation","changeType":"Create","delta":[]},
  {"resourceId":"/subscriptions/test/resourceGroups/alive/providers/Microsoft.App/jobs/asset-scan-warmer","changeType":"Create","delta":[]},
  {"resourceId":"/subscriptions/test/resourceGroups/alive/providers/Microsoft.ManagedIdentity/userAssignedIdentities/asset-scan-warmer-identity","changeType":"Create","delta":[]},
  {"resourceId":"/subscriptions/test/resourceGroups/alive/providers/Microsoft.App/containerApps/asset-scan-worker","changeType":"Create","delta":[]},
  {"resourceId":"/subscriptions/test/resourceGroups/alive/providers/Microsoft.Storage/storageAccounts/alive/queueServices/default/queues/asset-scan-warm","changeType":"Create","delta":[]},
  {"resourceId":"/subscriptions/test/resourceGroups/alive/providers/Microsoft.Storage/storageAccounts/alive/queueServices/default/queues/asset-scan-warm/providers/Microsoft.Authorization/roleAssignments/scan-warm-reader","changeType":"Create","delta":[]},
  {"resourceId":"/subscriptions/test/resourceGroups/alive/providers/Microsoft.Storage/storageAccounts/alive/queueServices/default/queues/asset-scan-warm/providers/Microsoft.Authorization/roleAssignments/scan-warm-sender","changeType":"Create","delta":[]},
  {"resourceId":"/subscriptions/test/resourceGroups/alive/providers/Microsoft.App/containerApps/asset-api/authConfigs/current","changeType":"Create","delta":[]}
]}' >"$tmp/safe.json"
./scripts/check-what-if.sh "$tmp/safe.json"

printf '%s\n' '{"changes":[{"resourceId":"/subscriptions/test/resourceGroups/alive/providers/Microsoft.App/containerApps/asset-api/authConfigs/current","changeType":"Modify","delta":[{"path":"properties.identityProviders.azureActiveDirectory.validation.defaultAuthorizationPolicy.allowedApplications","propertyChangeType":"Array","before":["line"],"after":["line","cms"]}]}]}' >"$tmp/extractor-auth.json"
./scripts/check-what-if.sh "$tmp/extractor-auth.json"
for resource in Microsoft.ManagedIdentity/userAssignedIdentities/bulletin-extractor-identity Microsoft.Authorization/roleAssignments/extractor-owner; do
  printf '{"changes":[{"resourceId":"/subscriptions/test/resourceGroups/alive/providers/%s","changeType":"Create","delta":[]}]}' "$resource" >"$tmp/unexpected-extractor-permission.json"
  if ./scripts/check-what-if.sh "$tmp/unexpected-extractor-permission.json"; then
    echo 'Extractor must not create a new identity or broad ARM permission' >&2
    exit 1
  fi
done

printf '%s\n' '{"changes":[{"resourceId":"/subscriptions/test/resourceGroups/alive/providers/Microsoft.App/containerApps/asset-scan-worker","changeType":"Modify","delta":[{"after":null,"before":100,"children":null,"path":"properties.configuration.maxInactiveRevisions","propertyChangeType":"Delete"},{"after":null,"before":null,"children":[{"after":null,"before":null,"children":[{"after":null,"before":{"accountName":"alive","queueLength":1,"queueName":"asset-scan"},"children":null,"path":"azureQueue","propertyChangeType":"Delete"}],"path":"0","propertyChangeType":"Modify"}],"path":"properties.template.scale.rules","propertyChangeType":"Array"}]}]}' >"$tmp/scan-worker-schema-migration.json"
./scripts/check-what-if.sh "$tmp/scan-worker-schema-migration.json"

printf '%s\n' '{"changes":[
  {"resourceId":"/subscriptions/test/resourceGroups/alive/providers/Microsoft.App/jobs/asset-retention","changeType":"Modify","delta":[
    {"path":"properties.configuration.triggerType","propertyChangeType":"Modify","before":"Manual","after":"Schedule"},
    {"path":"properties.configuration.manualTriggerConfig","propertyChangeType":"Delete"}
  ]}
]}' >"$tmp/retention-schedule.json"
./scripts/check-what-if.sh "$tmp/retention-schedule.json"
for field in resource trigger; do
  if [ "$field" = resource ]; then
    jq '.changes[0].resourceId |= sub("asset-retention$"; "asset-derivative")' "$tmp/retention-schedule.json" >"$tmp/unsafe-trigger.json"
  else
    jq '.changes[0].delta[0].after="Manual"' "$tmp/retention-schedule.json" >"$tmp/unsafe-trigger.json"
  fi
  if ./scripts/check-what-if.sh "$tmp/unsafe-trigger.json"; then
    echo 'manual trigger deletion must be limited to retention schedule activation' >&2
    exit 1
  fi
done

printf '%s\n' '{"changes":[
  {"resourceId":"/subscriptions/test/resourceGroups/alive/providers/Microsoft.App/jobs/asset-retention","changeType":"Modify","delta":[{"path":"properties.configuration.triggerType","propertyChangeType":"Modify","before":"Schedule","after":"Manual"}]}
]}' >"$tmp/retention-manual.json"
./scripts/check-what-if.sh "$tmp/retention-manual.json"

printf '%s\n' '{"changes":[{"resourceId":"/subscriptions/test/resourceGroups/alive/providers/Microsoft.App/containerApps/asset-api","changeType":"Modify","delta":[{"path":"properties.template.containers","propertyChangeType":"Array","children":[{"path":"env","propertyChangeType":"Delete"}]}]}]}' >"$tmp/nested-delete.json"
if ./scripts/check-what-if.sh "$tmp/nested-delete.json" 2>/dev/null; then
  echo "nested delete was not rejected" >&2
  exit 1
fi

printf '%s\n' '{"changes":[{"resourceId":"/subscriptions/test/resourceGroups/alive/providers/Microsoft.App/containerApps/other/authConfigs/current","changeType":"Create","delta":[]}]}' >"$tmp/unrelated-auth.json"
if ./scripts/check-what-if.sh "$tmp/unrelated-auth.json" 2>/dev/null; then
  echo "unrelated auth configuration was not rejected" >&2
  exit 1
fi

printf '%s\n' '{"changes":[{"resourceId":"/subscriptions/test/resourceGroups/alive/providers/Microsoft.Storage/storageAccounts/alive","changeType":"Modify","delta":[]}]}' >"$tmp/unrelated-modify.json"
if ./scripts/check-what-if.sh "$tmp/unrelated-modify.json" 2>/dev/null; then
  echo "unrelated infrastructure modification was not rejected" >&2
  exit 1
fi

printf '%s\n' '{"changes":[{"resourceId":"/subscriptions/test/resourceGroups/alive/providers/Microsoft.App/jobs/asset-migrate","changeType":"Create","delta":[]}]}' >"$tmp/migration-create.json"
if ./scripts/check-what-if.sh "$tmp/migration-create.json" 2>/dev/null; then
  echo "migration job creation was not rejected" >&2
  exit 1
fi
