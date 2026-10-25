#!/usr/bin/env bash
set -euo pipefail
# Default-off rollout must not silently change the existing production trigger.
grep -Fq 'param recordingEventsProvisioned bool = false' infra/main.bicep
grep -Fq 'param recordingEventsEnabled bool = false' infra/main.bicep
grep -Fq 'var recordingEventQueueEnv = recordingEventsProvisioned ? flatten(recordingEventQueueGroups) : []' infra/main.bicep
grep -Fq "triggerType: enabled ? 'Event' : 'Manual'" infra/recording-events.bicep
grep -Fq "queueLengthStrategy: 'visibleonly'" infra/recording-events.bicep
grep -Fq 'maxExecutions: 1' infra/recording-events.bicep
grep -Fq "cronExpression: '17 * * * *'" infra/recording-events.bicep
grep -Fq 'processingCPU' infra/recording-events.bicep
grep -Fq "? '4Gi' : '8Gi'" infra/recording-events.bicep
grep -Fq "json('0.5')" infra/recording-events.bicep
grep -Fq 'verify-recording-jobs.sh' .github/workflows/release.yml

bash scripts/validate-recording-event-rollout.sh
if RECORDING_EVENTS_ENABLED=true bash scripts/validate-recording-event-rollout.sh >/dev/null 2>&1; then
 echo 'event activation bypassed approval/provisioning' >&2; exit 1
fi
if RECORDING_EVENTS_PROVISIONED=true RECORDING_EVENTS_CHANGE_APPROVED=false bash scripts/validate-recording-event-rollout.sh >/dev/null 2>&1; then
 echo 'event provisioning bypassed approval' >&2; exit 1
fi
RECORDING_EVENTS_PROVISIONED=true RECORDING_EVENTS_ENABLED=true RECORDING_EVENTS_CHANGE_APPROVED=true RECORDING_HLS_ENABLED=true MEMBER_VIDEO_ENABLED=true bash scripts/validate-recording-event-rollout.sh
for cpu in 2 4; do
 RECORDING_LIVE_CPU="$cpu" bash scripts/validate-recording-event-rollout.sh
done
for cpu in 1 3 0 invalid; do
 if RECORDING_LIVE_CPU="$cpu" bash scripts/validate-recording-event-rollout.sh >/dev/null 2>&1; then
  echo 'invalid live tier accepted' >&2; exit 1
 fi
done
grep -Fq 'param recordingLiveCPU int = 4' infra/main.bicep
grep -Fq 'liveCPU:recordingLiveCPU' infra/main.bicep
grep -Fq 'param liveCPU int = 4' infra/recording-events.bicep
grep -Fq 'RECORDING_LIVE_CPU: ${{ vars.RECORDING_LIVE_CPU' .github/workflows/release.yml
test "$(grep -Fc 'recordingLiveCPU="$RECORDING_LIVE_CPU"' .github/workflows/release.yml)" = 2

tmp="$(mktemp)"
trap 'rm -f "$tmp"' EXIT
printf '%s' '{"changes":[{"resourceId":"/subscriptions/test/resourceGroups/alive/providers/Microsoft.App/jobs/asset-recording-live","changeType":"Create","delta":[]}]}' > "$tmp"
if bash scripts/check-recording-event-what-if.sh "$tmp" >/dev/null 2>&1; then echo 'missing event approval accepted' >&2; exit 1; fi
RECORDING_EVENTS_CHANGE_APPROVED=true RECORDING_WORKER_PRINCIPAL_ID=worker RECORDING_API_PRINCIPAL_ID=api bash scripts/check-recording-event-what-if.sh "$tmp"
printf '%s' '{"changes":[{"resourceId":"/subscriptions/test/resourceGroups/alive/providers/Microsoft.App/jobs/asset-recording-live","changeType":"Delete","delta":[]}]}' > "$tmp"
if RECORDING_EVENTS_CHANGE_APPROVED=true RECORDING_WORKER_PRINCIPAL_ID=worker RECORDING_API_PRINCIPAL_ID=api bash scripts/check-recording-event-what-if.sh "$tmp" >/dev/null 2>&1; then echo 'event job deletion accepted' >&2; exit 1; fi
printf '%s' '{"changes":[{"resourceId":"/subscriptions/test/resourceGroups/alive/providers/Microsoft.Storage/storageAccounts/alive/queueServices/default/queues/asset-recording-live/providers/Microsoft.Authorization/roleAssignments/bad","changeType":"Create","after":{"properties":{"principalId":"attacker","roleDefinitionId":"/roles/Contributor"}},"delta":[]}]}' > "$tmp"
if RECORDING_EVENTS_CHANGE_APPROVED=true RECORDING_WORKER_PRINCIPAL_ID=worker RECORDING_API_PRINCIPAL_ID=api bash scripts/check-recording-event-what-if.sh "$tmp" >/dev/null 2>&1; then echo 'broad event role accepted' >&2; exit 1; fi

python3 scripts/test-recording-event-topology.py

python3 scripts/test-recording-event-permissions.py
