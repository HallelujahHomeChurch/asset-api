#!/bin/sh
set -eu

file="${1:?what-if JSON is required}"

jq -e '
  # ARM compares the env array by position. Preserve the deletion guard unless
  # the exact original entry is present once under the same name elsewhere.
  def ignore_reordered_asset_env:
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
          | if (($entry.name | type) == "string")
              and ((($entry.secretRef | type) == "string") or (($entry.value | type) == "string"))
              and $after[$index].name != $entry.name
              and [$before[] | select(.name == $entry.name)] == [$entry]
              and [$after[] | select(.name == $entry.name)] == [$entry] then
              ($entry | if has("secretRef") then "secretRef" else "value" end) as $field
              | .children |= map(select(.propertyChangeType != "Delete" or .path != $field))
            else . end)
    else . end;

  # Only remove the retired service-auth environment values from this app.
  def ignore_retired_asset_auth_env:
    if .changeType == "Modify" and (.resourceId | endswith("/Microsoft.App/containerApps/asset-api"))
      and ([.before.properties.template.containers[]?.name] == ["asset-api"])
      and ([.after.properties.template.containers[]?.name] == ["asset-api"]) then
      .before.properties.template.containers[0].env as $before
      | .after.properties.template.containers[0].env as $after
      | (.delta[] | select(.path == "properties.template.containers" and .propertyChangeType == "Array")
        | .children[]? | select(.path == "0")
        | .children[]? | select(.path == "env" and .propertyChangeType == "Array")
        | .children[]? | select(.path | test("^(0|[1-9][0-9]*)$"))) |= (
          $before[(.path | tonumber)] as $entry
          | if (($entry.name | type) == "string") and ($entry.name | test("^ASSET_(WORKLOAD_(TENANT_ID|ISSUER|AUDIENCE|REQUIRED_ROLE)|(LINE|LINE_API|EXTRACTOR|WATERMARK|ACCOUNT|GATEWAY)_WORKLOAD_(CLIENT_ID|OBJECT_ID))$"))
              and [$after[] | select(.name == $entry.name)] == [] then
              # Keep non-deletion changes visible to the rest of the policy.
              walk(if type == "object" and .propertyChangeType? == "Delete" then .propertyChangeType = "NoChange" else . end)
            else . end)
    else . end;

  .changes |= map(ignore_reordered_asset_env | ignore_retired_asset_auth_env) |
  ([.changes[] | select(.changeType == "Delete" or .changeType == "Unsupported")] | length == 0)
  and
  ([.changes[]
    | . as $change
    | .resourceId as $resourceId
    | ..
    | objects
    | select(.propertyChangeType? == "Delete")
      | select(
        ((.path | test("^properties\\.(identityProviders|globalValidation)(\\.|$)"))
          and ($resourceId | endswith("/Microsoft.App/containerApps/asset-api/authConfigs/current"))
          and ($change.after.properties.platform.enabled == false))
        or ((.path == "properties.runningStatus" or .path == "properties.template.revisionSuffix")
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
