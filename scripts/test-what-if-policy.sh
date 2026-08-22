#!/bin/sh
set -eu

tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

printf '%s\n' '{"changes":[
  {"resourceId":"/subscriptions/test/resourceGroups/alive/providers/Microsoft.App/containerApps/asset-api","changeType":"Modify","delta":[{"path":"properties.runningStatus","propertyChangeType":"Delete"},{"path":"properties.template.revisionSuffix","propertyChangeType":"Delete"}]},
  {"resourceId":"/subscriptions/test/resourceGroups/alive/providers/Microsoft.App/jobs/asset-migrate","changeType":"Modify","delta":[]},
  {"resourceId":"/subscriptions/test/resourceGroups/alive/providers/Microsoft.App/jobs/asset-scan","changeType":"Create","delta":[]},
  {"resourceId":"/subscriptions/test/resourceGroups/alive/providers/Microsoft.App/jobs/asset-derivative","changeType":"Modify","delta":[]},
  {"resourceId":"/subscriptions/test/resourceGroups/alive/providers/Microsoft.App/jobs/asset-clamav-signature-refresh","changeType":"Create","delta":[]},
  {"resourceId":"/subscriptions/test/resourceGroups/alive/providers/Microsoft.App/jobs/asset-retention","changeType":"Create","delta":[]},
  {"resourceId":"/subscriptions/test/resourceGroups/alive/providers/Microsoft.App/containerApps/asset-api/authConfigs/current","changeType":"Create","delta":[]}
]}' >"$tmp/safe.json"
./scripts/check-what-if.sh "$tmp/safe.json"

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
