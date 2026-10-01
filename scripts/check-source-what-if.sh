#!/bin/sh
set -eu

# Bootstrap only: never modify existing storage settings or delete resources.
jq -e '
  .status == "Succeeded" and (.changes | type == "array") and
  all(.changes[];
    .changeType == "Ignore" or .changeType == "NoChange" or
    (.changeType == "Create" and
      (.resourceId | test("^/subscriptions/[0-9a-f-]+/resourceGroups/alive/providers/Microsoft.Storage/storageAccounts/aliverecordingsprod(/|$)"))))
' "${1:?what-if JSON is required}" >/dev/null
