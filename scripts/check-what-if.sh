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
        (.path == "properties.runningStatus"
          and ($resourceId | contains("/Microsoft.App/containerApps/")))
        | not
      )
  ] | length == 0)
  and
  ([.changes[]
    | select(.changeType != "Ignore" and .changeType != "NoChange")
    | select(
        .changeType != "Modify"
        or ((.resourceId | endswith("/Microsoft.App/containerApps/asset-api") or endswith("/Microsoft.App/jobs/asset-migrate") or endswith("/Microsoft.App/jobs/asset-scan") or endswith("/Microsoft.App/jobs/asset-clamav-signature-refresh")) | not)
      )
  ] | length == 0)
' "$file" >/dev/null
