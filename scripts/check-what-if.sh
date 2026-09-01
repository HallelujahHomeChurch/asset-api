#!/bin/sh
set -eu

file="${1:?what-if JSON is required}"

jq -e '
  ([.changes[] | select(.changeType == "Delete" or .changeType == "Unsupported")] | length == 0)
  and
  ([.changes[]
    | .resourceId as $resourceId
    | ..
    | objects
    | select(.propertyChangeType? == "Delete")
    | select(
        ((.path == "properties.runningStatus" or .path == "properties.template.revisionSuffix")
          and ($resourceId | contains("/Microsoft.App/containerApps/")))
        | not
      )
  ] | length == 0)
  and
  ([.changes[]
    | select(.changeType != "Ignore" and .changeType != "NoChange")
      | select(
        ((.changeType == "Modify")
          and (.resourceId | endswith("/Microsoft.App/containerApps/asset-api") or endswith("/Microsoft.App/jobs/asset-migrate")))
        or ((.changeType == "Create" or .changeType == "Modify")
          and (.resourceId | endswith("/Microsoft.App/jobs/asset-scan") or endswith("/Microsoft.App/jobs/asset-derivative") or endswith("/Microsoft.App/jobs/asset-clamav-signature-refresh")))
        or ((.changeType == "Create" or .changeType == "Modify")
          and (.resourceId | endswith("/Microsoft.App/jobs/asset-retention")))
        or ((.changeType == "Create" or .changeType == "Modify")
          and (.resourceId | endswith("/Microsoft.App/containerApps/asset-api/authConfigs/current")))
        or ((.changeType == "Create" or .changeType == "Modify")
          and (.resourceId | endswith("/Microsoft.App/containerApps/asset-scan-worker")))
        or ((.changeType == "Create")
          and (.resourceId | endswith("/queueServices/default/queues/asset-scan-warm")))
        or ((.changeType == "Create")
          and (.resourceId | contains("/queueServices/default/queues/asset-scan-warm/providers/Microsoft.Authorization/roleAssignments/")))
        | not
      )
  ] | length == 0)
' "$file" >/dev/null
