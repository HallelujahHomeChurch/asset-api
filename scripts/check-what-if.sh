#!/bin/sh
set -eu

file="${1:?what-if JSON is required}"

jq -e '
  # ARM compares the env array by position. Preserve the deletion guard unless
  # the exact original DB entry is present once under the same name elsewhere.
  def ignore_reordered_asset_db_env:
    if .changeType == "Modify" and (.resourceId | endswith("/Microsoft.App/containerApps/asset-api"))
      and ([.before.properties.template.containers[]?.name] == ["asset-api"])
      and ([.after.properties.template.containers[]?.name] == ["asset-api"]) then
      .before.properties.template.containers[0].env as $before
      | .after.properties.template.containers[0].env as $after
      | (.delta[] | select(.path == "properties.template.containers" and .propertyChangeType == "Array")
        | .children[]? | select(.path == "0")
        | .children[]? | select(.path == "env" and .propertyChangeType == "Array")
        | .children[]? | select(.path | test("^(0|[1-9][0-9]*)$"))) |= (
          (.path | tonumber) as $index
          | $before[$index] as $entry
          | if (($entry.name == "DATABASE_URL" and ($entry.secretRef | type) == "string")
                or ($entry.name == "DB_MAX_OPEN_CONNS" and ($entry.value | type) == "string"))
              and $after[$index].name != $entry.name
              and [$before[] | select(.name == $entry.name)] == [$entry]
              and [$after[] | select(.name == $entry.name)] == [$entry] then
              ($entry | if .name == "DATABASE_URL" then "secretRef" else "value" end) as $field
              | .children |= map(select(.propertyChangeType != "Delete" or .path != $field))
            else . end)
    else . end;

  .changes |= map(ignore_reordered_asset_db_env) |
  ([.changes[] | select(.changeType == "Delete" or .changeType == "Unsupported")] | length == 0)
  and
  ([.changes[]
    | . as $change
    | .resourceId as $resourceId
    | ..
    | objects
    | select(.propertyChangeType? == "Delete")
      | select(
        ((.path == "properties.runningStatus" or .path == "properties.template.revisionSuffix")
          and ($resourceId | contains("/Microsoft.App/containerApps/")))
        or ((.path == "properties.configuration.eventTriggerConfig.parallelism"
            or .path == "properties.configuration.eventTriggerConfig.replicaCompletionCount"
            or .path == "properties.configuration.eventTriggerConfig.scale.pollingInterval"
            or .path == "properties.configuration.eventTriggerConfig.scale.rules")
          and ($resourceId | endswith("/Microsoft.App/jobs/asset-scan")))
        or ((.path == "properties.configuration.manualTriggerConfig")
          and ($resourceId | endswith("/Microsoft.App/jobs/asset-retention"))
          and ($change.changeType == "Modify")
          and any($change.delta[]?; .path == "properties.configuration.triggerType"
            and .propertyChangeType == "Modify" and .before == "Manual" and .after == "Schedule"))
        or ((.path == "properties.configuration.maxInactiveRevisions" or .path == "azureQueue")
          and ($resourceId | endswith("/Microsoft.App/containerApps/asset-scan-worker")))
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
          and (.resourceId | endswith("/Microsoft.App/jobs/asset-recording-validation")))
        or ((.changeType == "Create" or .changeType == "Modify")
          and (.resourceId | endswith("/Microsoft.App/containerApps/asset-api/authConfigs/current")))
        or ((.changeType == "Create" or .changeType == "Modify")
          and (.resourceId | endswith("/Microsoft.App/containerApps/asset-scan-worker")))
        or ((.changeType == "Create")
          and (.resourceId | endswith("/queueServices/default/queues/asset-scan-warm")))
        or ((.changeType == "Create")
          and (.resourceId | contains("/queueServices/default/queues/asset-scan-warm/providers/Microsoft.Authorization/roleAssignments/")))
        or ((.changeType == "Create")
          and (.resourceId | endswith("/Microsoft.ManagedIdentity/userAssignedIdentities/asset-scan-warmer-identity")))
        | not
      )
  ] | length == 0)
' "$file" >/dev/null
