#!/usr/bin/env bash
set -euo pipefail

base="${1:?base commit required}"
head="${2:?head commit required}"
mode="${3:-compatible}"
[[ "$base" =~ ^[0-9a-f]{40}$ && "$head" =~ ^[0-9a-f]{40}$ ]]
[[ "$mode" == compatible || "$mode" == exact-fix ]]
git merge-base --is-ancestor "$base" "$head"
git diff --quiet "$base" "$head" -- . \
  ':(exclude).github/workflows/release.yml' ':(exclude)scripts/check-what-if.sh' \
  ':(exclude)scripts/test-what-if-policy.sh' ':(exclude)scripts/test-release-workflow.sh' \
  ':(exclude)scripts/check-release-dapr-config-delta.sh' ':(exclude)infra/main.bicep' \
  ':(exclude)scripts/recording-event-topology.py' ':(exclude)scripts/test-recording-event-topology.py'

if git diff --quiet "$base" "$head" -- infra/main.bicep; then
  [[ "$mode" == compatible ]]
  exit
fi

# A recovery may change only the known ARM boolean rendering defect in Bicep.
[[ "$(git ls-tree --format='%(objectmode)' "$base" -- infra/main.bicep)" == 100644 ]]
[[ "$(git ls-tree --format='%(objectmode)' "$head" -- infra/main.bicep)" == 100644 ]]
source_template="$(mktemp)"
expected_template="$(mktemp)"
trap 'rm -f "$source_template" "$expected_template"' EXIT
git show "$base:infra/main.bicep" > "$source_template"
[[ "$(grep -Fxc "            { name: 'ASSET_DAPR_ENABLED', value: string(daprEnabled) }" "$source_template")" == 1 ]]
sed "s/^            { name: 'ASSET_DAPR_ENABLED', value: string(daprEnabled) }$/            { name: 'ASSET_DAPR_ENABLED', value: daprEnabled ? 'true' : 'false' }/" "$source_template" > "$expected_template"
git show "$head:infra/main.bicep" | cmp -s "$expected_template" -
