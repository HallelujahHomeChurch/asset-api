#!/usr/bin/env python3
"""Apply reviewed environment overrides without building or changing images."""
import copy
import json
import base64
import os
from pathlib import Path
import re
import subprocess
import sys
import time
import urllib.request

SOURCE = Path(__file__).resolve().parents[1] / 'infra/runtime-env.json'
RESOURCES = {'asset-api': 'app', 'asset-clamav-signature-refresh': 'job', 'asset-derivative': 'job', 'asset-migrate': 'job', 'asset-recording-validation': 'job', 'asset-retention': 'job', 'asset-scan': 'job', 'asset-scan-worker': 'app', 'asset-recording-live': 'job', 'asset-recording-processing': 'job', 'asset-recording-preview': 'job', 'asset-recording-maintenance': 'job'}


def parse(text):
    data = json.loads(text)
    if set(data) != {'resources'} or not isinstance(data['resources'], dict) or set(data['resources']) != set(RESOURCES):
        raise ValueError('Invalid runtime environment resources')
    for overrides in data['resources'].values():
        if not isinstance(overrides, dict):
            raise ValueError('Invalid environment map')
        for name, value in overrides.items():
            if name == 'RELEASE':
                raise ValueError('RELEASE is reserved for immutable artifact identity')
            if not re.fullmatch(r'[A-Z_][A-Z0-9_]*', name):
                raise ValueError('Invalid environment name')
            if value is None:
                continue
            if isinstance(value, dict) and set(value) == {'value'} and isinstance(value['value'], str) and '\x00' not in value['value'] and not value['value'].startswith('secretref:'):
                continue
            if isinstance(value, dict) and set(value) == {'secretRef'} and isinstance(value['secretRef'], str) and re.fullmatch(r'[a-z0-9][a-z0-9-]*', value['secretRef']):
                continue
            raise ValueError('Use value objects, existing secretRef objects, or null removals')
    return data['resources']


def load():
    return parse(SOURCE.read_text())


def az(*args):
    command = [os.environ.get('AZ_BIN', 'az'), *args, '--only-show-errors', '-o', 'json']
    result = subprocess.run(command, capture_output=True, text=True, timeout=300)
    if result.returncode:
        raise ValueError('Azure operation failed; environment values withheld')
    return json.loads(result.stdout) if result.stdout.strip() else None


def arguments(command, overrides):
    command = list(command)
    existing = {}
    removed = []
    for flag in ('--set-env-vars', '--remove-env-vars'):
        while flag in command:
            start = command.index(flag)
            end = start + 1
            while end < len(command) and not command[end].startswith('-'):
                if flag == '--set-env-vars':
                    name, value = command[end].split('=', 1)
                    existing[name] = value
                else:
                    removed.append(command[end])
                end += 1
            del command[start:end]
    for name, value in overrides.items():
        existing.pop(name, None)
        removed = [key for key in removed if key != name]
        if value is None:
            removed.append(name)
        else:
            existing[name] = value['value'] if 'value' in value else 'secretref:' + value['secretRef']
    if existing:
        command += ['--set-env-vars', *[name + '=' + value for name, value in existing.items()]]
    if removed:
        command += ['--remove-env-vars', *removed]
    return command


def run(resource, command):
    overrides = load()[resource]
    if not overrides and '--image' not in command:
        return
    refs = {v['secretRef'] for v in overrides.values() if isinstance(v, dict) and 'secretRef' in v}
    if refs:
        kind = ['job'] if RESOURCES[resource] == 'job' else []
        names = az('containerapp', *kind, 'show', '-g', 'alive', '-n', resource,
                   '--query', 'properties.configuration.secrets[].name')
        if not refs.issubset(set(names)):
            raise ValueError('Runtime secret reference is not provisioned')
    result = subprocess.run(arguments(command, overrides), capture_output=True, timeout=300)
    if result.returncode:
        raise ValueError('Runtime environment update failed; values withheld')


def main_manifest():
    repository = os.environ['GITHUB_REPOSITORY']
    if not re.fullmatch(r'HallelujahHomeChurch/[a-z0-9-]+', repository):
        raise ValueError('Invalid repository')
    request = urllib.request.Request(
        'https://api.github.com/repos/' + repository + '/contents/infra/runtime-env.json?ref=main',
        headers={'Authorization': 'Bearer ' + os.environ['GH_TOKEN'],
                 'Accept': 'application/vnd.github+json'})
    with urllib.request.urlopen(request, timeout=30) as response:
        content = json.load(response)
        return parse(base64.b64decode(content['content']).decode('utf-8')), content.get('sha', '')


def is_current_main():
    return main_manifest()[0] == load()


def refresh():
    if os.environ.get('GITHUB_REF') != 'refs/heads/main':
        raise ValueError('Runtime configuration requires merged main')
    desired, blob = main_manifest()
    if not re.fullmatch(r'[0-9a-f]{40}', blob):
        raise ValueError('Invalid merged configuration identity')
    SOURCE.write_text(json.dumps({'resources': desired}, indent=2) + '\n')
    # Code/image source stays immutable; settings source is recorded separately.
    print('Runtime configuration source blob: ' + blob)


def patch_template(resource_id, template):
    # Stdin keeps existing literal env values out of CLI argv and command logs.
    result = subprocess.run(
        [os.environ.get('AZ_BIN', 'az'), 'rest', '--method', 'patch', '--uri',
         resource_id + '?api-version=2025-01-01', '--body', '@/dev/stdin',
         '--only-show-errors', '--output', 'none'],
        input=json.dumps({'properties': {'template': template}}),
        text=True, capture_output=True, timeout=300)
    if result.returncode:
        raise ValueError('Runtime template update failed; values withheld')


def wait_for_template(resource, kind, template, revision):
    # Control-plane completion only; no application smoke or job execution.
    deadline = time.monotonic() + 180
    while time.monotonic() < deadline:
        current = az('containerapp', *kind, 'show', '-g', 'alive', '-n', resource)['properties']
        if current.get('provisioningState') in ('Failed', 'Canceled'):
            raise ValueError('Runtime template provisioning failed')
        if kind:
            if current.get('provisioningState') == 'Succeeded' and current['template'] == template:
                return
        elif current.get('provisioningState') == 'Succeeded' and current.get('latestReadyRevisionName') == revision:
            return
        time.sleep(5)
    raise ValueError('Runtime template provisioning timed out')


def deploy():
    if os.environ.get('GITHUB_REF') != 'refs/heads/main':
        raise ValueError('Runtime deployment requires merged main')
    if not is_current_main():
        print('Skipped superseded runtime configuration')
        return
    base_sha = os.environ['RUNTIME_ENV_BASE_SHA']
    if not re.fullmatch(r'[0-9a-f]{40}', base_sha):
        raise ValueError('Invalid deployment base')
    previous = parse(subprocess.check_output(['git', 'show', base_sha + ':infra/runtime-env.json'], text=True))
    desired = load()
    if any(not set(previous[name]).issubset(values) for name, values in desired.items()):
        raise ValueError('Use explicit null to remove an environment variable')
    for resource, overrides in desired.items():
        # Reconcile every owned target, including settings from superseded runs.
        if not overrides:
            continue
        kind = ['job'] if RESOURCES[resource] == 'job' else []
        current = az('containerapp', *kind, 'show', '-g', 'alive', '-n', resource)
        props = current['properties']
        template = props['template']
        multiple = not kind and props['configuration']['activeRevisionsMode'] == 'Multiple'
        predecessor = None
        if not kind:
            if multiple:
                revisions = az('containerapp', 'revision', 'list', '-g', 'alive', '-n', resource)
                serving = [r for r in revisions if r['properties'].get('trafficWeight', 0) > 0]
                if len(serving) != 1 or serving[0]['properties'].get('trafficWeight') != 100:
                    raise ValueError('Expected one serving revision with all traffic')
                predecessor = serving[0]['name']
                template = serving[0]['properties']['template']
            else:
                predecessor = props['latestReadyRevisionName']
                template = az('containerapp', 'revision', 'show', '-g', 'alive', '-n', resource,
                              '--revision', predecessor)['properties']['template']
        containers = template['containers']
        if len(containers) != 1 or not re.fullmatch(r'alive\.azurecr\.io/alive/[a-z0-9-]+@sha256:[0-9a-f]{64}', containers[0]['image']):
            raise ValueError('Expected one container with an immutable existing image')
        updated = copy.deepcopy(template)
        env = {entry['name']: copy.deepcopy(entry) for entry in containers[0].get('env', [])}
        names = {entry['name'] for entry in props.get('configuration', {}).get('secrets', [])}
        # Revision reads may return app-prefixed refs; only map to a proven secret.
        for entry in env.values():
            reference = entry.get('secretRef')
            if reference and reference not in names:
                prefix = resource + '-'
                if reference.startswith(prefix) and reference[len(prefix):] in names:
                    entry['secretRef'] = reference[len(prefix):]
                else:
                    raise ValueError('Serving template references an unknown secret')
        expected = {key: value for key, value in env.items() if key not in overrides}
        expected.update({key: {'name': key, **value} for key, value in overrides.items() if value is not None})
        if expected == env:
            continue
        refs = {entry['secretRef'] for entry in expected.values() if 'secretRef' in entry}
        if not refs.issubset(names):
            raise ValueError('Runtime secret reference is not provisioned')
        updated['containers'][0]['env'] = list(expected.values())
        revision = None
        if not kind:
            suffix = 'env-' + os.environ['GITHUB_RUN_ID'] + '-' + os.environ['GITHUB_RUN_ATTEMPT']
            if not re.fullmatch(r'env-[0-9]+-[0-9]+', suffix):
                raise ValueError('Invalid deployment revision identity')
            updated['revisionSuffix'] = suffix
            revision = resource + '--' + suffix
        resource_type = 'jobs' if kind else 'containerApps'
        if not re.fullmatch(r'/subscriptions/[0-9a-fA-F-]+/resourceGroups/alive/providers/Microsoft.App/' + resource_type + '/' + re.escape(resource), current['id'], re.IGNORECASE):
            raise ValueError('Unexpected runtime resource identity')
        if not is_current_main():
            print('Skipped superseded runtime configuration')
            return
        if multiple:
            az('containerapp', 'ingress', 'traffic', 'set', '-g', 'alive', '-n', resource, '--revision-weight', predecessor + '=100')
        patch_template(current['id'], updated)
        wait_for_template(resource, kind, updated, revision)
        if multiple:
            az('containerapp', 'ingress', 'traffic', 'set', '-g', 'alive', '-n', resource, '--revision-weight', revision + '=100')
        print('Applied runtime environment: ' + resource)


def main():
    if sys.argv[1:] == ['deploy']:
        deploy()
    elif sys.argv[1:] == ['refresh']:
        refresh()
    elif sys.argv[1:] == ['validate']:
        load()
    elif len(sys.argv) >= 5 and sys.argv[1] == 'run' and sys.argv[3] == '--':
        run(sys.argv[2], sys.argv[4:])
    else:
        raise ValueError('Expected validate, deploy, or run RESOURCE -- COMMAND')

if __name__ == '__main__':
    try:
        main()
    except (ValueError, KeyError, TypeError, OSError, subprocess.SubprocessError):
        raise SystemExit('Runtime environment operation failed; review manifest and deployment state (values withheld)')
