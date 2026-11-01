#!/usr/bin/env python3
"""Recover this release's existing scan App and non-recording Job templates."""
import argparse
import json
import os
from pathlib import Path
import re
import subprocess
import time
from urllib.parse import urlsplit

JOBS = ('asset-scan', 'asset-clamav-signature-refresh', 'asset-derivative')
APP = 'asset-scan-worker'
WARMER = 'asset-scan-warmer'
ROOT = '/subscriptions/3765a72c-2c30-492c-8b88-7a26522903f8/resourceGroups/alive/providers/'
WARMER_ID = ROOT + 'Microsoft.App/jobs/' + WARMER
WARMER_QUERY = '{id:id,location:location,identity:identity,environmentId:properties.environmentId,configuration:properties.configuration,template:properties.template}'
CONFIG = ('triggerType', 'manualTriggerConfig', 'scheduleTriggerConfig', 'eventTriggerConfig',
          'replicaTimeout', 'replicaRetryLimit')
ENV = {
    'AZURE_CLIENT_ID', 'ASSET_AZURE_ACCOUNT_URL', 'ASSET_AZURE_CONTAINER',
    'ASSET_SCAN_QUEUE_URL', 'ASSET_SCAN_WARM_QUEUE_URL', 'ASSET_SCAN_POISON_QUEUE_URL',
    'CLAMAV_SIGNATURE_CONTAINER', 'CLAMAV_SIGNATURE_MAX_AGE', 'CLAMAV_SCAN_TIMEOUT',
    'ASSET_SCAN_MAX_FILE_SIZE_BYTES', 'CLAMAV_MAX_FILE_SIZE_BYTES', 'CLAMAV_MAX_SCAN_SIZE_BYTES',
    'CLAMAV_MAX_FILES', 'CLAMAV_MAX_RECURSION', 'CLAMAV_MAX_RETRIES',
    'ASSET_DERIVATIVE_QUEUE_URL', 'ASSET_DERIVATIVE_POISON_QUEUE_URL',
    'ASSET_RETENTION_APPLY_ENABLED',
}
QUERY = "{configuration:{" + ','.join(f'{k}:properties.configuration.{k}' for k in CONFIG) + "},template:properties.template}"


def az(*args):
    result = subprocess.run(['az', *args, '--only-show-errors', '-o', 'json'],
                            capture_output=True, text=True, timeout=120)
    if result.returncode:
        raise RuntimeError('Azure worker recovery operation failed (response suppressed)')
    return json.loads(result.stdout or '{}')


def image(value):
    if not isinstance(value, str) or not re.fullmatch(r'alive\.azurecr\.io/alive/asset-(?:api|scan)@sha256:[0-9a-f]{64}', value):
        raise ValueError('worker recovery requires an approved immutable image')


def selected_jobs():
    flag = os.environ.get('DEPLOY_RETENTION_JOB', 'false')
    if flag not in ('true', 'false'):
        raise ValueError('invalid retention deployment flag')
    return [*JOBS, *(['asset-retention'] if flag == 'true' else [])]


def job(name):
    value = az('containerapp', 'job', 'show', '-g', 'alive', '-n', name, '--query', QUERY)
    # Newer read APIs add this default; the pinned recovery API needs no override.
    for container in value['template']['containers']:
        if container.get('imageType') not in (None, 'ContainerImage'):
            raise ValueError('unreviewed worker image type')
        container.pop('imageType', None)
    return value


def validate_job(value):
    # Reject unreviewed shapes rather than persisting credentials in an artifact.
    if set(value) != {'template', 'configuration'} or set(value['configuration']) != set(CONFIG):
        raise ValueError('unexpected Job snapshot shape')
    template = value['template']
    if any(v not in (None, []) for k, v in template.items() if k != 'containers'):
        raise ValueError('unreviewed Job template field')
    containers = template.get('containers', [])
    if len(containers) != 1:
        raise ValueError('expected one worker container')
    c = containers[0]
    if any(v not in (None, []) for k, v in c.items() if k not in {'name', 'image', 'command', 'env', 'resources'}):
        raise ValueError('unreviewed worker container field')
    image(c['image'])
    if not re.fullmatch(r'[a-z0-9-]+', c['name']) or not re.fullmatch(r'/asset-[a-z-]+', ' '.join(c.get('command', []))):
        raise ValueError('unexpected worker command')
    if set(c['resources']) - {'cpu', 'memory', 'ephemeralStorage'}:
        raise ValueError('unexpected resource field')
    for env in c.get('env', []):
        if set(env) - {'name', 'value', 'secretRef'}:
            raise ValueError('unexpected environment field')
        if env.get('secretRef') and not env.get('value'):
            continue
        if env['name'] not in ENV or any(x in str(env.get('value', '')) for x in ('?', '@', '\n', '\r')):
            raise ValueError('unreviewed literal environment value')
    event = value['configuration'].get('eventTriggerConfig') or {}
    for rule in (event.get('scale') or {}).get('rules') or []:
        if rule.get('type') != 'azure-queue' or rule.get('auth'):
            raise ValueError('unreviewed scaler authentication')
        metadata = rule.get('metadata', {})
        if set(metadata) - {'accountName', 'queueName', 'queueLength', 'queueLengthStrategy'}:
            raise ValueError('unreviewed scaler metadata')


def capture(path):
    snapshot = {'jobs': {name: job(name) for name in selected_jobs()}}
    for value in snapshot['jobs'].values():
        validate_job(value)
    app = az('containerapp', 'show', '-g', 'alive', '-n', APP,
             '--query', '{revision:properties.latestReadyRevisionName}')
    revision = app.get('revision')
    if not isinstance(revision, str) or not re.fullmatch(r'asset-scan-worker--[a-z0-9-]+', revision):
        raise ValueError('scan worker lacks a known ready revision')
    app['image'] = az('containerapp', 'revision', 'show', '-g', 'alive', '-n', APP,
                      '--revision', revision, '--query', 'properties.template.containers[0].image')
    image(app['image'])
    snapshot['scan'] = app
    snapshot['warmer'] = warmer()
    with open(path, 'x', opener=lambda p, flags: os.open(p, flags, 0o600)) as out:
        json.dump(snapshot, out)


def warmer():
    names = az('containerapp', 'job', 'list', '-g', 'alive', '--query', "[?name=='asset-scan-warmer'].name")
    if not names:
        return None
    raw = az('containerapp', 'job', 'show', '-g', 'alive', '-n', WARMER, '--query', WARMER_QUERY)
    config = raw['configuration']
    if any(v not in (None, []) for k, v in config.items() if k not in {*CONFIG, 'registries'}):
        raise ValueError('unreviewed warmer configuration')
    identity = raw['identity']
    if set(identity) != {'type', 'userAssignedIdentities'}:
        raise ValueError('unreviewed warmer identity')
    identity['userAssignedIdentities'] = {k: {} for k in identity['userAssignedIdentities']}
    for c in raw['template']['containers']:
        if c.pop('imageType', None) not in (None, 'ContainerImage'):
            raise ValueError('unreviewed warmer image type')
    value = {'id': raw['id'], 'location': raw['location'], 'identity': identity, 'properties': {
        'environmentId': raw['environmentId'], 'configuration': {k: config.get(k) for k in (*CONFIG, 'registries')},
        'template': raw['template']}}
    validate_warmer(value)
    value['location'] = value['location'].replace(' ', '').lower()
    return value


def validate_warmer(value):
    if value is None:
        return
    if set(value) != {'id', 'location', 'identity', 'properties'} or value['id'].lower() != WARMER_ID.lower() or value['location'].replace(' ', '').lower() != 'eastasia':
        raise ValueError('unexpected warmer resource')
    props = value['properties']
    if set(props) != {'environmentId', 'configuration', 'template'} or props['environmentId'].lower() != (ROOT + 'Microsoft.App/managedEnvironments/alive-env').lower():
        raise ValueError('unexpected warmer environment')
    identities = {(ROOT + 'Microsoft.ManagedIdentity/userAssignedIdentities/' + name).lower() for name in ('asset-api-acr-pull', 'asset-scan-warmer-identity')}
    identity = value['identity']
    if set(identity) != {'type', 'userAssignedIdentities'} or identity['type'] != 'UserAssigned' or {k.lower() for k in identity['userAssignedIdentities']} != identities or any(identity['userAssignedIdentities'].values()):
        raise ValueError('unexpected warmer identity')
    config = props['configuration']
    expected = dict.fromkeys(CONFIG)
    expected.update(triggerType='Schedule', replicaTimeout=120, replicaRetryLimit=1,
                    scheduleTriggerConfig={'cronExpression': '*/1 * * * *', 'parallelism': 1, 'replicaCompletionCount': 1},
                    registries=[{'server': 'alive.azurecr.io', 'identity': ROOT + 'Microsoft.ManagedIdentity/userAssignedIdentities/asset-api-acr-pull'}])
    if config != expected:
        raise ValueError('unexpected warmer schedule or registry')
    template = props['template']
    if any(v not in (None, []) for k, v in template.items() if k != 'containers') or len(template.get('containers', [])) != 1:
        raise ValueError('unexpected warmer template')
    c = template['containers'][0]
    if set(c) != {'name', 'image', 'command', 'env', 'resources'} or c['name'] != WARMER or c['command'] != ['/asset-scan-warmer'] or c['resources'] != {'cpu': 0.25, 'memory': '0.5Gi'}:
        raise ValueError('unexpected warmer container')
    image(c['image'])
    env = c['env']
    if not c['image'].startswith('alive.azurecr.io/alive/asset-api@'):
        raise ValueError('warmer requires the API image containing its binary')
    expected_names = {'AZURE_CLIENT_ID', 'MEETING_API_BASE_URL', 'MEETING_API_AUDIENCE', 'ASSET_SCAN_WARM_QUEUE_URL', 'ASSET_SCAN_WARM_LEAD', 'ASSET_SCAN_WARM_TAIL', 'ASSET_SCAN_WARM_HTTP_TIMEOUT'}
    if len(env) != len(expected_names) or any(set(e) != {'name', 'value'} for e in env) or {e['name'] for e in env} != expected_names:
        raise ValueError('unexpected warmer environment variables')
    values = {e['name']: e['value'] for e in env}
    if values['AZURE_CLIENT_ID'] != '1c9c4b5a-927d-48fb-9f01-1d1e66e281a4' or values['MEETING_API_AUDIENCE'] != 'api://63e5ac29-f5b4-462d-ab8d-9029b78fcc58' or [values[k] for k in ('ASSET_SCAN_WARM_LEAD', 'ASSET_SCAN_WARM_TAIL', 'ASSET_SCAN_WARM_HTTP_TIMEOUT')] != ['5m', '10m', '10s']:
        raise ValueError('unexpected warmer settings')
    for name, host, path in [('MEETING_API_BASE_URL', 'operations-api.internal.gentleriver-81abd7bc.eastasia.azurecontainerapps.io', ''), ('ASSET_SCAN_WARM_QUEUE_URL', 'alivestoragebb99ee6e.queue.core.windows.net', '/asset-scan-warm')]:
        uri = urlsplit(values[name])
        if uri.scheme != 'https' or uri.netloc != host or uri.path != path or uri.query or uri.fragment:
            raise ValueError('unexpected warmer endpoint')


def restore_warmer(value):
    if value is None:
        return
    if warmer() != value:
        az('rest', '--method', 'put', '--url', WARMER_ID + '?api-version=2025-07-01',
           '--body', json.dumps({k: v for k, v in value.items() if k != 'id'}))
    for _ in range(24):
        state = az('containerapp', 'job', 'show', '-g', 'alive', '-n', WARMER, '--query', 'properties.provisioningState')
        if state in ('Failed', 'Canceled', 'Cancelled'):
            raise RuntimeError('warmer rollback provisioning failed')
        if state == 'Succeeded' and warmer() == value:
            return
        time.sleep(5)
    raise RuntimeError('warmer rollback readback timed out')


def restore_scan(scan):
    suffix = f"rollback-{os.environ['GITHUB_RUN_ID']}-{os.environ['GITHUB_RUN_ATTEMPT']}"
    if not re.fullmatch(r'rollback-[0-9]+-[0-9]+', suffix):
        raise ValueError('invalid workflow run identity')
    az('containerapp', 'revision', 'copy', '-g', 'alive', '-n', APP,
       '--from-revision', scan['revision'], '--image', scan['image'], '--revision-suffix', suffix)
    expected = APP + '--' + suffix
    for _ in range(60):
        ready = az('containerapp', 'show', '-g', 'alive', '-n', APP,
                   '--query', '{latest:properties.latestRevisionName,ready:properties.latestReadyRevisionName}')
        if ready == {'latest': expected, 'ready': expected}:
            actual = az('containerapp', 'revision', 'show', '-g', 'alive', '-n', APP,
                        '--revision', expected, '--query', 'properties.template.containers[0].image')
            if actual == scan['image']:
                return
            raise RuntimeError('scan rollback image mismatch')
        time.sleep(5)
    raise RuntimeError('scan rollback readiness timed out')


def wait_job(name, properties):
    for _ in range(24):
        state = az('containerapp', 'job', 'show', '-g', 'alive', '-n', name,
                   '--query', 'properties.provisioningState')
        if state in ('Failed', 'Canceled', 'Cancelled'):
            raise RuntimeError('Job rollback provisioning failed')
        if state == 'Succeeded' and job(name) == properties:
            return
        time.sleep(5)
    raise RuntimeError('Job rollback provisioning/readback timed out')


def restore(path):
    snapshot = json.loads(path.read_text())
    if set(snapshot) != {'jobs', 'scan', 'warmer'} or set(snapshot['jobs']) != set(selected_jobs()):
        raise ValueError('recovery inventory mismatch')
    for value in snapshot['jobs'].values():
        validate_job(value)
    validate_warmer(snapshot['warmer'])
    scan = snapshot['scan']
    image(scan['image'])
    if not re.fullmatch(r'asset-scan-worker--[a-z0-9-]+', scan['revision']):
        raise ValueError('invalid prior scan revision')
    failures = []
    # Every resource gets an attempt even if another restore failed. Existing
    # Job executions retain their original template; never stop or replay them.
    for name, properties in snapshot['jobs'].items():
        try:
            current = job(name)  # Fail closed if a prior resource disappeared.
            if current != properties:
                resource_id = az('containerapp', 'job', 'show', '-g', 'alive', '-n', name, '--query', 'id')
                if not re.fullmatch(r'/subscriptions/[0-9a-f-]+/resourceGroups/alive/providers/Microsoft.App/jobs/' + re.escape(name), resource_id, re.I):
                    raise ValueError('unexpected Job resource ID')
                az('rest', '--method', 'patch', '--url', resource_id+'?api-version=2025-07-01',
                   '--body', json.dumps({'properties': properties}))
            wait_job(name, properties)
        except Exception:
            failures.append(name)
    try:
        # Keep cron warming unless its old queue producer is restored first.
        restore_warmer(snapshot['warmer'])
    except Exception:
        failures.append(WARMER)
    else:
        try:
            restore_scan(scan)
        except Exception:
            failures.append(APP)
    if failures:
        raise RuntimeError('Worker rollback incomplete: ' + ', '.join(failures))


if __name__ == '__main__':
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('action', choices=('capture', 'restore'))
    parser.add_argument('path', type=Path)
    args = parser.parse_args()
    if os.environ.get('RESOURCE_GROUP') != 'alive':
        raise SystemExit('unexpected resource group')
    try:
        globals()[args.action](args.path)
    except Exception as error:
        # Never dump ARM responses, environment values or snapshot payloads.
        raise SystemExit(str(error) if isinstance(error, (ValueError, RuntimeError)) else 'Worker recovery failed')
