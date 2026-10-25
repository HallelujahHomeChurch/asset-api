#!/usr/bin/env bash
set -euo pipefail
get() { az containerapp job show -g "$RESOURCE_GROUP" -n "$1" --query "$2" -o tsv --only-show-errors; }
expected=Schedule
[[ "${RECORDING_EVENTS_ENABLED:-false}" == true ]] && expected=Manual
[[ "$(get asset-recording-validation properties.configuration.triggerType)" == "$expected" ]]
if [[ "${RECORDING_EVENTS_PROVISIONED:-false}" == true ]]; then
 for lane in live processing preview; do
  name="asset-recording-$lane"
  expected=Manual
  [[ "${RECORDING_EVENTS_ENABLED:-false}" == true ]] && expected=Event
  [[ "$(get "$name" properties.configuration.triggerType)" == "$expected" ]]
  [[ "$(get "$name" 'properties.template.containers[0].image')" == "$RECORDING_IMAGE_REF" ]]
  cpu=4; memory=8Gi
  if [[ "$lane" == live && "${RECORDING_LIVE_CPU:-4}" == 2 ]]; then cpu=2; memory=4Gi; fi
  if [[ "$lane" == processing && "${RECORDING_PROCESSING_CPU:-2}" == 2 ]]; then cpu=2; memory=4Gi; fi
  if [[ "$lane" == preview && "${RECORDING_PREVIEW_CPU:-1}" == 1 ]]; then cpu=1; memory=2Gi; fi
  actual_cpu="$(get "$name" 'properties.template.containers[0].resources.cpu')"
  awk -v actual="$actual_cpu" -v expected="$cpu" 'BEGIN { exit !(actual==expected) }'
  [[ "$(get "$name" 'properties.template.containers[0].resources.memory')" == "$memory" ]]
  if [[ "$expected" == Event ]]; then
   [[ "$(get "$name" properties.configuration.eventTriggerConfig.scale.minExecutions)" == 0 ]]
   [[ "$(get "$name" properties.configuration.eventTriggerConfig.scale.maxExecutions)" == 1 ]]
   [[ "$(get "$name" 'properties.configuration.eventTriggerConfig.scale.rules[0].metadata.queueLengthStrategy')" == visibleonly ]]
  fi
 done
 expected=Manual
 [[ "${RECORDING_EVENTS_ENABLED:-false}" == true ]] && expected=Schedule
 [[ "$(get asset-recording-maintenance properties.configuration.triggerType)" == "$expected" ]]
 if [[ "$expected" == Schedule ]]; then
  [[ "$(get asset-recording-maintenance properties.configuration.scheduleTriggerConfig.cronExpression)" == '17 * * * *' ]]
 fi
 [[ "$(get asset-recording-maintenance 'properties.template.containers[0].resources.cpu')" == 0.5 ]]
 [[ "$(get asset-recording-maintenance 'properties.template.containers[0].resources.memory')" == 1Gi ]]
fi
