#!/usr/bin/env bash
set -euo pipefail
file="${1:?what-if JSON required}"
[[ "${RECORDING_EVENTS_CHANGE_APPROVED:-false}" == true ]] || { echo 'recording event what-if approval missing' >&2; exit 1; }
: "${RECORDING_WORKER_PRINCIPAL_ID:?worker identity required}" "${RECORDING_API_PRINCIPAL_ID:?API identity required}"
tmp="$(mktemp)"
trap 'rm -f "$tmp"' EXIT
# Validate the exact new resource names and queue-scoped roles before passing
# unrelated changes to the existing production what-if policy.
jq -e --arg worker "$RECORDING_WORKER_PRINCIPAL_ID" --arg api "$RECORDING_API_PRINCIPAL_ID" '
 def eventjob: .resourceId|test("/Microsoft.App/jobs/asset-recording-(live|processing|preview|maintenance)$");
 def eventqueue: .resourceId|test("/queueServices/default/queues/asset-recording-(live|processing|preview)(-poison)?$");
 def eventrole: .resourceId|test("/queueServices/default/queues/asset-recording-(live|processing|preview)(-poison)?/providers/Microsoft.Authorization/roleAssignments/[^/]+$");
 ([.changes[]|select(.changeType=="Delete" or .changeType=="Unsupported")]|length==0) and
 ([.changes[]|select(eventrole and (.changeType=="Create" or .changeType=="Modify"))|
  select((.after.properties.principalId!=$worker and .after.properties.principalId!=$api) or
   (.after.properties.roleDefinitionId|test("/(c6a89b2d-59bc-44d0-9896-0f6e12d7b80a|8a0f0c08-91a1-4084-bc3d-661d67233fed|19e7f393-937e-4f77-808e-94535e297925)$")|not))]|length==0) and
 ([.changes[]|select(eventjob or eventqueue or eventrole)|..|objects|select(.propertyChangeType?=="Delete")|
  select((.path|test("^properties.configuration.(manualTriggerConfig|eventTriggerConfig|scheduleTriggerConfig)(\\.|$)"))|not)]|length==0)
' "$file" >/dev/null
jq '
 def covered: (.resourceId|test("/Microsoft.App/jobs/asset-recording-(live|processing|preview|maintenance)$|/queueServices/default/queues/asset-recording-(live|processing|preview)(-poison)?(/providers/Microsoft.Authorization/roleAssignments/[^/]+)?$|/Microsoft.Resources/deployments/recording-events$"));
 .changes |= map(if covered then .changeType="NoChange" | .delta=[] else . end |
  if (.resourceId|endswith("/Microsoft.App/jobs/asset-recording-validation")) then
   .delta |= ((. // []) | map(select((.path|test("^properties.configuration.(manualTriggerConfig|scheduleTriggerConfig)(\\.|$)"))|not)))
  else . end)
' "$file" > "$tmp"
bash scripts/check-what-if.sh "$tmp"
