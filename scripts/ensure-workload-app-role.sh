#!/usr/bin/env bash
set -euo pipefail

# No application/consent creation: one existing managed identity, one API role.
# Apply is allowed only in the reviewed release under its Entra identity.
mode="${1:-check}"
fail() { echo "$1" >&2; exit 1; }
case "$mode" in check|apply) ;; *) fail 'Use check or apply.' ;; esac
for name in RESOURCE_GROUP AZURE_TENANT_ID ASSET_WORKLOAD_CLIENT_ID ASSET_WORKLOAD_AUDIENCE ASSET_EXTRACTOR_WORKLOAD_CLIENT_ID ASSET_EXTRACTOR_WORKLOAD_OBJECT_ID; do
  [[ -n "${!name:-}" ]] || fail "Missing configuration: $name"
done

session="$(az account show --output json --only-show-errors)" || fail 'Azure session unavailable.'
jq -e --arg tenant "$AZURE_TENANT_ID" '.tenantId == $tenant' <<<"$session" >/dev/null || fail 'Deployment tenant mismatch.'
if [[ "$mode" == apply ]]; then
  [[ "${EXTRACTOR_ROLE_APPROVED:-false}" == true && -n "${AZURE_CLIENT_ID:-}" ]] || fail 'Explicit reviewed role approval required.'
  jq -e --arg client "$AZURE_CLIENT_ID" '.user.type == "servicePrincipal" and .user.name == $client' <<<"$session" >/dev/null || fail 'Apply requires the existing Entra deployment identity.'
fi

identity="$(az identity show --resource-group "$RESOURCE_GROUP" --name hhc-web-api-identity --output json --only-show-errors)" || fail 'Existing CMS managed identity unavailable.'
jq -e --arg client "$ASSET_EXTRACTOR_WORKLOAD_CLIENT_ID" --arg object "$ASSET_EXTRACTOR_WORKLOAD_OBJECT_ID" --arg tenant "$AZURE_TENANT_ID" \
  '.clientId == $client and .principalId == $object and .tenantId == $tenant' <<<"$identity" >/dev/null || fail 'Configured extractor pair does not match the existing CMS identity.'
principal="$(az ad sp show --id "$ASSET_EXTRACTOR_WORKLOAD_CLIENT_ID" --output json --only-show-errors)" || fail 'CMS service principal unavailable.'
jq -e --arg client "$ASSET_EXTRACTOR_WORKLOAD_CLIENT_ID" --arg object "$ASSET_EXTRACTOR_WORKLOAD_OBJECT_ID" \
  '.appId == $client and .id == $object and .servicePrincipalType == "ManagedIdentity"' <<<"$principal" >/dev/null || fail 'Managed identity service principal mismatch.'
resource="$(az ad sp show --id "$ASSET_WORKLOAD_CLIENT_ID" --output json --only-show-errors)" || fail 'Asset API service principal unavailable.'
jq -e --arg client "$ASSET_WORKLOAD_CLIENT_ID" --arg audience "$ASSET_WORKLOAD_AUDIENCE" --arg tenant "$AZURE_TENANT_ID" \
  '.appId == $client and .appOwnerOrganizationId == $tenant and .servicePrincipalType == "Application" and (.servicePrincipalNames | index($audience) != null)' <<<"$resource" >/dev/null || fail 'Asset API audience/owner mismatch.'
roles="$(jq '[.appRoles[] | select(.value == "Asset.Invoke" and .isEnabled == true and (.allowedMemberTypes | index("Application") != null))]' <<<"$resource")"
[[ "$(jq length <<<"$roles")" == 1 ]] || fail 'Asset.Invoke role is missing, disabled or ambiguous.'
resource_id="$(jq -r .id <<<"$resource")"
role_id="$(jq -r '.[0].id' <<<"$roles")"
principal_id="$ASSET_EXTRACTOR_WORKLOAD_OBJECT_ID"
for id in "$resource_id" "$role_id" "$principal_id"; do
  [[ "$id" =~ ^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$ && "$id" != 00000000-0000-0000-0000-000000000000 ]] || fail 'Invalid principal/role identifier.'
done
assignments_url="https://graph.microsoft.com/v1.0/servicePrincipals/${principal_id}/appRoleAssignments?\$select=principalId,resourceId,appRoleId&\$top=999"
assignment_count() {
  local records
  records="$(az rest --method GET --url "$assignments_url" --output json --only-show-errors)" || fail 'Unable to verify app-role assignments.'
  # ponytail: reject paginated lists; add trusted Graph pagination above 999 assignments.
  jq -e --arg principal "$principal_id" --arg resource "$resource_id" --arg role "$role_id" '
    (.value | type == "array") and (."@odata.nextLink" == null) and
    ([.value[] | select(.principalId != $principal or (.appRoleId == $role and .resourceId != $resource) or (.resourceId == $resource and .appRoleId != $role))] | length == 0)
  ' <<<"$records" >/dev/null || fail 'Unexpected, wrong-resource or incomplete role assignment list.'
  jq --arg resource "$resource_id" --arg role "$role_id" '[.value[] | select(.resourceId == $resource and .appRoleId == $role)] | length' <<<"$records"
}
count="$(assignment_count)"
[[ "$count" -le 1 ]] || fail 'Duplicate exact app-role assignments require owner review.'
if [[ "$count" == 1 ]]; then
  echo 'Exact extractor Asset.Invoke assignment verified.'
  exit 0
fi
[[ "$mode" == apply ]] || fail 'Exact extractor Asset.Invoke assignment is missing; no changes made.'
body="$(jq -nc --arg principal "$principal_id" --arg resource "$resource_id" --arg role "$role_id" '{principalId:$principal,resourceId:$resource,appRoleId:$role}')"
az rest --method POST --url "https://graph.microsoft.com/v1.0/servicePrincipals/${resource_id}/appRoleAssignedTo" --body "$body" --output none --only-show-errors >/dev/null 2>&1 || fail 'Role assignment failed; platform owner must verify Entra deployment authority.'
[[ "$(assignment_count)" == 1 ]] || fail 'New role assignment not yet verified; stop and recheck, never broaden consent.'
echo 'Approved exact extractor Asset.Invoke assignment created and verified.'
