#!/bin/sh
set -eu

tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

printf '%s\n' '{"changes":[
  {"resourceId":"/subscriptions/test/resourceGroups/alive/providers/Microsoft.App/containerApps/asset-api","changeType":"Modify","delta":[{"path":"properties.runningStatus","propertyChangeType":"Delete"}]},
  {"resourceId":"/subscriptions/test/resourceGroups/alive/providers/Microsoft.App/jobs/asset-migrate","changeType":"Modify","delta":[]}
]}' >"$tmp/safe.json"
./scripts/check-what-if.sh "$tmp/safe.json"

printf '%s\n' '{"changes":[{"resourceId":"/subscriptions/test/resourceGroups/alive/providers/Microsoft.App/containerApps/asset-api","changeType":"Modify","delta":[{"path":"properties.template.containers","propertyChangeType":"Array","children":[{"path":"env","propertyChangeType":"Delete"}]}]}]}' >"$tmp/nested-delete.json"
if ./scripts/check-what-if.sh "$tmp/nested-delete.json" 2>/dev/null; then
  echo "nested delete was not rejected" >&2
  exit 1
fi

printf '%s\n' '{"changes":[{"resourceId":"/subscriptions/test/resourceGroups/alive/providers/Microsoft.Storage/storageAccounts/alive","changeType":"Modify","delta":[]}]}' >"$tmp/unrelated-modify.json"
if ./scripts/check-what-if.sh "$tmp/unrelated-modify.json" 2>/dev/null; then
  echo "unrelated infrastructure modification was not rejected" >&2
  exit 1
fi
