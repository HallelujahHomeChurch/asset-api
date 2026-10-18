#!/usr/bin/env bash
set -euo pipefail
: "${RESOURCE_GROUP:?}" "${STORAGE_ACCOUNT_NAME:?}"
api="$(az containerapp show -g "$RESOURCE_GROUP" -n asset-api --query identity.principalId -o tsv --only-show-errors)"
worker="$(az identity show -g "$RESOURCE_GROUP" -n asset-api-runtime-identity --query principalId -o tsv --only-show-errors)"
account="$(az storage account show -g "$RESOURCE_GROUP" -n "$STORAGE_ACCOUNT_NAME" --query id -o tsv --only-show-errors)"
for lane in live processing preview; do
 for kind in work poison; do
  queue="asset-recording-$lane"
  [[ "$kind" == poison ]] && queue="$queue-poison"
  scope="$account/queueServices/default/queues/$queue"
  az resource show --ids "$scope" --api-version 2023-05-01 --only-show-errors -o none
  assignments="$(az role assignment list --scope "$scope" -o json --only-show-errors)"
  jq -e --arg api "$api" --arg worker "$worker" --arg scope "$scope" --arg kind "$kind" '
   def granted($principal;$role): any(.[]; .scope==$scope and .principalId==$principal and (.roleDefinitionId|endswith("/"+$role)));
   if $kind=="poison" then granted($worker;"c6a89b2d-59bc-44d0-9896-0f6e12d7b80a")
   else granted($api;"c6a89b2d-59bc-44d0-9896-0f6e12d7b80a") and granted($worker;"8a0f0c08-91a1-4084-bc3d-661d67233fed") and granted($worker;"19e7f393-937e-4f77-808e-94535e297925") end
  ' <<<"$assignments" >/dev/null || { echo "missing recording queue role: $queue; complete approved scoped bootstrap before activation" >&2; exit 1; }
 done
done
