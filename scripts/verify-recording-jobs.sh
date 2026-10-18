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
 [[ "$(get asset-recording-maintenance 'properties.template.containers[0].resources.memory')" == 1Gi ]]
fi
