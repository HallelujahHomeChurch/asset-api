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


def capture(path, group):
    snapshots = []
    for job in jobs(group):
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
    # Only the CI runner holds this non-secret rollback state; never upload it as an artifact.
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


def restore(path, group):
    # Do not forcibly stop valid work or activate the old cron alongside running Event jobs.
    deadline = time.monotonic()+300
    while True:
        active = False
        for job in jobs(group):
            executions = az('containerapp', 'job', 'execution', 'list', '-g', group, '-n', job['name'])
            active |= any(e['properties']['status'] in ('Running', 'Processing', 'Pending', 'Unknown') for e in executions)
        if not active:
            break
        if time.monotonic() >= deadline:
            raise RuntimeError('recording executions still active; triggers remain Manual, operator must resume rollback after safe drain')
        time.sleep(10)
    current_ids = {j['id'] for j in jobs(group)}
    for job in json.loads(path.read_text()):
        if job['id'] not in current_ids:
            raise ValueError('prior recording Job missing; refusing incomplete topology restore')
        patch(job['id'], job['properties'])


if __name__ == '__main__':
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('action', choices=('capture', 'pause', 'restore'))
    parser.add_argument('path', type=Path)
    args = parser.parse_args()
    group = os.environ['RESOURCE_GROUP']
    if args.action == 'pause':
        pause(group)
    else:
        globals()[args.action](args.path, group)
