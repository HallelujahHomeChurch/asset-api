#!/usr/bin/env python3
"""CI-only recording topology rollback; never reads or persists secret values."""
import argparse
import json
import os
from pathlib import Path
import re
import subprocess
import time

NAMES = {'asset-recording-validation', 'asset-recording-live', 'asset-recording-processing',
         'asset-recording-preview', 'asset-recording-maintenance'}
CONFIG = ('triggerType', 'manualTriggerConfig', 'scheduleTriggerConfig', 'eventTriggerConfig',
          'replicaTimeout', 'replicaRetryLimit')
ENV = {'AZURE_CLIENT_ID', 'ASSET_R2_ACCOUNT_ID', 'ASSET_R2_BUCKET', 'ASSET_RECORDING_EVENTS_ENABLED',
       'ASSET_RECORDING_HLS_ENABLED', 'ASSET_RECORDING_SOURCE_ENABLED', 'ASSET_RECORDING_SOURCE_ACCOUNT_URL',
       'ASSET_RECORDING_SOURCE_CONTAINER', 'ASSET_RECORDING_WORKER_MODE'} | {
           f'ASSET_RECORDING_{lane}_{suffix}' for lane in ('LIVE', 'PROCESSING', 'PREVIEW') for suffix in ('QUEUE_URL', 'POISON_URL')}
# ARM selection excludes configuration.secrets entirely; env credentials must be secretRefs.
QUERY = "[].{name:name,id:id,properties:{configuration:{" + ','.join(f'{k}:properties.configuration.{k}' for k in CONFIG) + "},template:properties.template}}"
API = '2025-07-01'


def az(*args):
    result = subprocess.run(['az', *args, '--only-show-errors', '-o', 'json'], capture_output=True, text=True)
    if result.returncode:
        raise RuntimeError('Azure recording topology operation failed; inspect CI step without exposing response bodies')
    return json.loads(result.stdout or '{}')


def jobs(group):
    return [j for j in az('containerapp', 'job', 'list', '-g', group, '--query', QUERY) if j['name'] in NAMES]


def active_executions(group, current):
    for job in current:
        executions = az('containerapp', 'job', 'execution', 'list', '-g', group, '-n', job['name'])
        if any(e['properties']['status'] not in ('Succeeded', 'Failed', 'Canceled', 'Cancelled', 'Stopped') for e in executions):
            return True
    return False


def capture(path, group):
    current = jobs(group)
    provisioned = os.environ.get('RECORDING_EVENTS_PROVISIONED', 'false') == 'true'
    enabled = os.environ.get('RECORDING_EVENTS_ENABLED', 'false') == 'true'
    if not provisioned and any(j['name'] != 'asset-recording-validation' for j in current):
        raise ValueError('existing recording Event topology requires provisioned=true; do not orphan active triggers')
    legacy = next((j for j in current if j['name'] == 'asset-recording-validation'), None)
    changing = legacy and legacy['properties']['configuration']['triggerType'] != ('Manual' if enabled else 'Schedule')
    if changing and active_executions(group, current):
        raise RuntimeError('recording trigger switch requires all executions to drain before deployment')
    snapshots = []
    for job in current:
        props = job['properties']
        template = props['template']
        for container in template['containers']:
            if not re.fullmatch(r'[^\s]+@sha256:[0-9a-f]{64}', container['image']):
                raise ValueError('rollback requires immutable worker images')
            for env in container.get('env', []):
                if env.get('secretRef') and not env.get('value'):
                    continue
                if env['name'] not in ENV or any(x in env.get('value', '') for x in ('?', '@', '\n')):
                    raise ValueError('unexpected literal environment value; use a secretRef before snapshot')
        snapshots.append({'id': job['id'], 'properties': {
            'configuration': {k: props['configuration'].get(k) for k in CONFIG}, 'template': template}})
    # Capture only non-secret rollback state; CI retains it on failure for safe recovery.
    with open(path, 'x', opener=lambda p, flags: os.open(p, flags, 0o600)) as out:
        json.dump(snapshots, out)


def patch(resource_id, properties):
    az('rest', '--method', 'patch', '--url', resource_id+'?api-version='+API,
       '--body', json.dumps({'properties': properties}))


def pause(group):
    for job in jobs(group):
        patch(job['id'], {'configuration': {'triggerType': 'Manual',
              'manualTriggerConfig': {'parallelism': 1, 'replicaCompletionCount': 1},
              'scheduleTriggerConfig': None, 'eventTriggerConfig': None}})


def wait_idle(group):
    # Do not forcibly stop valid work or activate competing workers.
    deadline = time.monotonic()+300
    while True:
        active = active_executions(group, jobs(group))
        if not active:
            break
        if time.monotonic() >= deadline:
            raise RuntimeError('recording executions still active; triggers remain Manual, operator must resume rollback after safe drain')
        time.sleep(10)
def quiesce(path, group):
    if os.environ.get('RECORDING_EVENTS_ENABLED', 'false') != 'true':
        return
    prior = json.loads(path.read_text())
    if not any(j['id'].endswith('/asset-recording-validation') and j['properties']['configuration']['triggerType']=='Schedule' for j in prior):
        return
    if active_executions(group, jobs(group)):
        raise RuntimeError('recording work active; leave the legacy schedule unchanged and retry in a quiet window')
    # Signal rollback responsibility before the first mutation, including partial failures.
    with open(os.environ['GITHUB_OUTPUT'], 'a') as out:
        out.write('paused=true\n')
    pause(group)
    wait_idle(group)


def restore(path, group):
    wait_idle(group)
    current_ids = {j['id'] for j in jobs(group)}
    for job in json.loads(path.read_text()):
        if job['id'] not in current_ids:
            raise ValueError('prior recording Job missing; refusing incomplete topology restore')
        patch(job['id'], job['properties'])


if __name__ == '__main__':
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('action', choices=('capture', 'pause', 'quiesce', 'restore'))
    parser.add_argument('path', type=Path)
    args = parser.parse_args()
    group = os.environ['RESOURCE_GROUP']
    if args.action == 'pause':
        pause(group)
    else:
        globals()[args.action](args.path, group)
