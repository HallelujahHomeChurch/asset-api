#!/usr/bin/env bash
set -euo pipefail
for name in RECORDING_EVENTS_PROVISIONED RECORDING_EVENTS_ENABLED; do
 [[ "${!name:-false}" == true || "${!name:-false}" == false ]] || { echo "invalid $name" >&2; exit 1; }
done
if [[ "${RECORDING_EVENTS_PROVISIONED:-false}" == true || "${RECORDING_EVENTS_ENABLED:-false}" == true ]]; then
 [[ "${RECORDING_EVENTS_CHANGE_APPROVED:-false}" == true ]] || { echo 'recording infrastructure requires reviewed what-if approval' >&2; exit 1; }
fi
if [[ "${RECORDING_EVENTS_ENABLED:-false}" == true ]]; then
 [[ "${RECORDING_EVENTS_PROVISIONED:-false}" == true && "${RECORDING_HLS_ENABLED:-false}" == true && "${MEMBER_VIDEO_ENABLED:-false}" == true ]] || { echo 'event activation requires provisioned HLS resources' >&2; exit 1; }
fi
[[ "${RECORDING_LIVE_CPU:-4}" =~ ^(1\.5|2|4)$ && "${RECORDING_PROCESSING_CPU:-2}" =~ ^(2|4)$ && "${RECORDING_PREVIEW_CPU:-1}" =~ ^(1|4)$ ]] || { echo 'invalid recording resource tier' >&2; exit 1; }
