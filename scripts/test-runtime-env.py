"""Offline coverage for config routing, env ownership and same-image deployment."""
import copy
import importlib.util
import json
import os
from pathlib import Path
import shutil
import subprocess
import tempfile
import unittest
from unittest.mock import patch

ROOT = Path(__file__).resolve().parents[1]
spec = importlib.util.spec_from_file_location('runtime_env', ROOT / 'scripts/runtime_env.py')
runtime = importlib.util.module_from_spec(spec)
spec.loader.exec_module(runtime)


class RuntimeEnvironmentTests(unittest.TestCase):
    def manifest(self, resource, entries):
        value = {'resources': {name: {} for name in runtime.RESOURCES}}
        value['resources'][resource] = entries
        return value

    def test_schema_rejects_invalid_env_objects(self):
        target = next(iter(runtime.RESOURCES))
        overrides = {'POOL': {'value': '20'}, 'TOKEN': {'secretRef': 'existing'}, 'OLD': None}
        self.assertEqual(runtime.parse(json.dumps(self.manifest(target, overrides)))[target], overrides)
        for value in [20, '20', {'value': 'secretref:wrong'}, {'secretRef': 'not valid'}, {'value': 'x', 'secretRef': 'x'}]:
            with self.subTest(value=value), self.assertRaises(ValueError):
                runtime.parse(json.dumps(self.manifest(target, {'POOL': value})))

    def test_release_artifact_identity_cannot_be_overridden_or_removed(self):
        target = next(iter(runtime.RESOURCES))
        for value in ({'value': 'operator-value'}, {'secretRef': 'existing'}, None):
            with self.subTest(value=value), self.assertRaisesRegex(ValueError, 'reserved'):
                runtime.parse(json.dumps(self.manifest(target, {'RELEASE': value})))

    def test_command_merges_once_without_shell_and_null_removes(self):
        # CLI options are separated from env args; shell-looking values remain one argv element.
        command = ['az', 'containerapp', 'update', '--image', 'same-image', '--set-env-vars', 'POOL=5', 'KEEP=yes', '--remove-env-vars', 'RETAIN', '--output', 'none']
        result = runtime.arguments(command, {'POOL': {'value': '20'}, 'RETAIN': {'value': '$(never-run)'}, 'OLD': None})
        self.assertEqual(result.count('--set-env-vars'), 1)
        self.assertEqual(result.count('--remove-env-vars'), 1)
        self.assertIn('POOL=20', result)
        self.assertIn('KEEP=yes', result)
        self.assertIn('RETAIN=$(never-run)', result)
        self.assertEqual(result[-2:], ['--remove-env-vars', 'OLD'])
        self.assertEqual(result[result.index('--image') + 1], 'same-image')

    def deploy_fixture(self, kind='app', multiple=False, unchanged=False, stale=False, invalid_ref=False, prefixed_ref=False):
        target = next(name for name, value in runtime.RESOURCES.items() if value == kind)
        new = self.manifest(target, {'POOL': {'value': '20'}, 'OLD': None})
        # Identical previous manifest proves convergence after a skipped older run.
        old = copy.deepcopy(new)
        image = 'alive.azurecr.io/alive/fixture@sha256:' + 'a' * 64
        template = {'containers': [{'name': target, 'image': image,
                     'env': [{'name': 'KEEP', 'value': 'retained'}, {'name': 'POOL', 'value': '20' if unchanged else '5'}],
                     'resources': {'cpu': 0.5, 'memory': '1Gi'}, 'command': ['server'],
                     'probes': [{'type': 'Liveness', 'tcpSocket': {'port': 8080}}]}],
                    'scale': {'minReplicas': 1, 'maxReplicas': 2}, 'revisionSuffix': 'serving'}
        if invalid_ref:
            new['resources'][target]['TOKEN'] = {'secretRef': 'unprovisioned'}
        current = {'id': '/subscriptions/00000000-0000-0000-0000-000000000001/resourceGroups/alive/providers/Microsoft.App/' + ('jobs' if kind == 'job' else 'containerApps') + '/' + target,
                   'properties': {'configuration': {'activeRevisionsMode': 'Multiple' if multiple else 'Single', 'secrets': []},
                                  'latestReadyRevisionName': target + '--ready', 'template': copy.deepcopy(template)}}
        if kind == 'app':
            # Current app template deliberately differs from the serving revision.
            current['properties']['template']['containers'][0]['image'] = 'wrong-current-image'
            current['properties']['template']['scale']['minReplicas'] = 99
        if prefixed_ref:
            template['containers'][0]['env'].append({'name': 'TOKEN', 'secretRef': target + '-token'})
            current['properties']['configuration']['secrets'] = [{'name': 'token'}]
        before = copy.deepcopy(template)
        calls = []
        def az(*args):
            calls.append(args)
            if args[:3] == ('containerapp', 'revision', 'show'):
                return {'properties': {'template': template}}
            if args[:3] == ('containerapp', 'revision', 'list'):
                return [{'name': target + '--ready', 'properties': {'trafficWeight': 100, 'template': template}}]
            return current
        env = {'GITHUB_REF': 'refs/heads/main', 'RUNTIME_ENV_BASE_SHA': 'b' * 40,
               'GITHUB_RUN_ID': '12', 'GITHUB_RUN_ATTEMPT': '1'}
        with patch.dict(os.environ, env), patch.object(runtime, 'is_current_main', return_value=not stale), patch.object(runtime, 'load', return_value=new['resources']), patch.object(runtime.subprocess, 'check_output', return_value=json.dumps(old)), patch.object(runtime, 'az', side_effect=az), patch.object(runtime, 'patch_template') as update, patch.object(runtime, 'wait_for_template') as wait:
            if invalid_ref:
                with self.assertRaisesRegex(ValueError, 'not provisioned'):
                    runtime.deploy()
                update.assert_not_called()
                return
            runtime.deploy()
        self.assertEqual(template, before)
        if stale or unchanged:
            update.assert_not_called()
            wait.assert_not_called()
            if stale:
                self.assertEqual(calls, [])
            return
        update.assert_called_once()
        result = update.call_args.args[1]
        expected = copy.deepcopy(before)
        expected['containers'][0]['env'] = [{'name': 'KEEP', 'value': 'retained'}, {'name': 'POOL', 'value': '20'}]
        if prefixed_ref:
            expected['containers'][0]['env'].insert(1, {'name': 'TOKEN', 'secretRef': 'token'})
        if kind == 'app':
            expected['revisionSuffix'] = 'env-12-1'
        self.assertEqual(result, expected)
        wait.assert_called_once()
        self.assertTrue(all('start' not in call for call in calls))
        if multiple:
            traffic = [call for call in calls if call[:3] == ('containerapp', 'ingress', 'traffic')]
            self.assertEqual([call[-1] for call in traffic], [target + '--ready=100', target + '--env-12-1=100'])

    def test_full_serving_template_preserved_and_old_desired_values_converge(self):
        self.deploy_fixture()

    def test_job_config_preserves_template_without_start(self):
        self.deploy_fixture(kind='job')

    def test_revision_prefixed_secret_ref_resolves_only_to_existing_secret(self):
        self.deploy_fixture(prefixed_ref=True)

    def test_multiple_revision_traffic_uses_serving_template(self):
        self.deploy_fixture(multiple=True)

    def test_unchanged_env_skips_patch(self):
        self.deploy_fixture(unchanged=True)

    def test_stale_run_skips_all_azure_calls(self):
        self.deploy_fixture(stale=True)

    def test_unknown_secret_ref_fails_before_patch(self):
        self.deploy_fixture(invalid_ref=True)

    def test_patch_body_is_stdin_not_argv(self):
        with patch.object(runtime.subprocess, 'run', return_value=subprocess.CompletedProcess([], 0)) as run:
            runtime.patch_template('/resource', {'containers': [{'env': [{'name': 'SENSITIVE', 'value': 'private-marker'}]}]})
        args, kwargs = run.call_args
        self.assertIn('@/dev/stdin', args[0])
        self.assertNotIn('private-marker', ' '.join(args[0]))
        self.assertIn('private-marker', kwargs['input'])
        self.assertTrue(kwargs['capture_output'])

    def test_key_deletion_fails_before_any_azure_update(self):
        target = next(iter(runtime.RESOURCES))
        old = self.manifest(target, {'POOL': {'value': '5'}})
        new = self.manifest(target, {})
        env = {'GITHUB_REF': 'refs/heads/main', 'RUNTIME_ENV_BASE_SHA': 'b' * 40}
        with patch.dict(os.environ, env), patch.object(runtime, 'is_current_main', return_value=True), patch.object(runtime, 'load', return_value=new['resources']), patch.object(runtime.subprocess, 'check_output', return_value=json.dumps(old)), patch.object(runtime, 'az') as az:
            with self.assertRaisesRegex(ValueError, 'explicit null'):
                runtime.deploy()
            az.assert_not_called()

    def test_workflow_uses_owner_identity_gate_and_only_deploy_lock(self):
        import re
        workflow = (ROOT / '.github/workflows/runtime-env.yml').read_text()
        owner_file = 'release-production.yml' if ROOT.name == 'account-api' else 'release.yml'
        owner = (ROOT / '.github/workflows' / owner_file).read_text()
        deploy = owner.split('  deploy:\n', 1)[1].split('    steps:', 1)[0]
        configure = workflow.split('  configure:\n', 1)[1]
        expected = re.search(r'^    environment: (.+)$', deploy, re.M)
        actual = re.search(r'^    environment: (.+)$', configure, re.M)
        self.assertEqual(actual.group(1) if actual else None, expected.group(1) if expected else None)
        gate = "vars.PRODUCTION_DEPLOY_ENABLED == 'true'"
        self.assertEqual(gate in configure, gate in deploy)
        self.assertIn('queue: max', configure)
        self.assertIn('queue: max', owner.split('jobs:', 1)[0])
        self.assertNotIn('concurrency:', workflow.split('  configure:\n', 1)[0])
        self.assertEqual(re.search(r'group: (.+)', configure)[1], re.search(r'group: (.+)', owner)[1])

    def test_normal_release_refreshes_latest_settings_before_preflight(self):
        target = next(iter(runtime.RESOURCES))
        desired = self.manifest(target, {'POOL': {'value': '30'}})
        with tempfile.TemporaryDirectory() as directory, patch.dict(os.environ, {'GITHUB_REF': 'refs/heads/main'}), patch.object(runtime, 'SOURCE', Path(directory) / 'manifest.json'), patch.object(runtime, 'main_manifest', return_value=(desired['resources'], 'a' * 40)):
            runtime.refresh()
            self.assertEqual(runtime.load(), desired['resources'])
        owner_file = 'release-production.yml' if ROOT.name == 'account-api' else 'release.yml'
        owner = (ROOT / '.github/workflows' / owner_file).read_text()
        for job in ('plan', 'deploy'):
            if '  ' + job + ':\n' not in owner:
                continue
            import re
            block = re.split(r'\n  [A-Za-z0-9_-]+:', owner.split('  ' + job + ':\n', 1)[1])[0]
            self.assertLess(block.index('scripts/runtime_env.py refresh'), block.index('azure/login@'))

    def test_freshness_compares_manifest_not_main_commit(self):
        import base64
        import io
        target = next(iter(runtime.RESOURCES))
        desired = self.manifest(target, {'POOL': {'value': '20'}})
        env = {'GITHUB_REPOSITORY': 'HallelujahHomeChurch/fixture', 'GH_TOKEN': 'fixture'}
        with patch.dict(os.environ, env), patch.object(runtime, 'load', return_value=desired['resources']):
            for same in (True, False):
                remote = desired if same else self.manifest(target, {'POOL': {'value': '30'}})
                response = io.BytesIO(json.dumps({'content': base64.b64encode(json.dumps(remote).encode()).decode()}).encode())
                with patch.object(runtime.urllib.request, 'urlopen', return_value=response):
                    self.assertEqual(runtime.is_current_main(), same)

    def test_deploy_routing_has_exactly_one_writer_for_mixed_paths(self):
        target = next(iter(runtime.RESOURCES))
        repo = ROOT.name
        test_path = 'scripts/release-static.test.sh' if repo == 'notification-api' else 'scripts/test-release-workflow.sh'
        workflow_path = '.github/workflows/release-production.yml' if repo == 'account-api' else '.github/workflows/release.yml'
        # Expected existing release policy: Account mixed source remains manual.
        fast = {
            'account-api': [True, True, True, False, False],
            'operations-api': [True, True, True, False, False],
            'asset-api': [True, False, True, True, False],
            'notification-api': [True, True, True, True, False],
            'engagement-api': [True, True, False, False, False],
        }[repo]
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            files = ['.github/scripts/ci-agent-docs.py', '.github/scripts/ci-runtime-env.py', workflow_path]
            for name in files:
                destination = root / name
                destination.parent.mkdir(parents=True, exist_ok=True)
                shutil.copyfile(ROOT / name, destination)
            (root / 'infra').mkdir()
            manifest = root / 'infra/runtime-env.json'
            manifest.write_text(json.dumps(self.manifest(target, {})))
            def git(*args):
                return subprocess.check_output(['git', *args], cwd=root, stderr=subprocess.DEVNULL, text=True).strip()
            git('init'); git('config', 'user.name', 'Fixture'); git('config', 'user.email', 'fixture@example.invalid')
            git('add', '.'); git('commit', '-qm', 'baseline'); base = git('rev-parse', 'HEAD')
            for path, expected in zip([None, 'README.md', test_path, workflow_path, 'internal/feature.go'], fast):
                git('reset', '--hard', base); git('clean', '-fd')
                manifest.write_text(json.dumps(self.manifest(target, {'POOL': {'value': '20'}})))
                if path:
                    file = root / path; file.parent.mkdir(parents=True, exist_ok=True)
                    file.write_text((file.read_text() if file.exists() else '') + '\n# fixture\n')
                git('add', '.'); git('commit', '-qm', 'change')
                command = ['python3', '.github/scripts/ci-runtime-env.py']
                args = ['push', base, git('rev-parse', 'HEAD')]
                self.assertEqual(subprocess.check_output(command + ['deploy'] + args, cwd=root, text=True).strip(), 'config_only=' + str(expected).lower(), path)
                # All mixed cases retain application CI regardless of deploy owner.
                self.assertEqual(subprocess.check_output(command + args, cwd=root, text=True).strip(), 'config_only=' + str(path is None).lower(), path)

    def test_classifier_accepts_only_exact_owned_manifest_diff(self):
        target = next(iter(runtime.RESOURCES))
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            for name in ['.github/scripts/ci-agent-docs.py', '.github/scripts/ci-runtime-env.py', 'scripts/runtime_env.py']:
                destination = root / name
                destination.parent.mkdir(parents=True, exist_ok=True)
                shutil.copyfile(ROOT / name, destination)
            (root / 'infra').mkdir()
            manifest = root / 'infra/runtime-env.json'
            def git(*args):
                return subprocess.check_output(['git', *args], cwd=root, stderr=subprocess.DEVNULL, text=True).strip()
            git('init'); git('config', 'user.name', 'Fixture'); git('config', 'user.email', 'fixture@example.invalid')
            manifest.write_text(json.dumps(self.manifest(target, {'POOL': {'value': '5'}})))
            git('add', '.'); git('commit', '-qm', 'baseline'); base = git('rev-parse', 'HEAD')
            def classify():
                return subprocess.check_output(['python3', '.github/scripts/ci-runtime-env.py', 'push', base, git('rev-parse', 'HEAD')], cwd=root, text=True).strip()
            manifest.write_text(json.dumps(self.manifest(target, {'POOL': {'value': '20'}, 'DROP': None})))
            git('add', '.'); git('commit', '-qm', 'env')
            self.assertEqual(classify(), 'config_only=true')
            (root / 'code.go').write_text('package changed\n')
            git('add', '.'); git('commit', '-qm', 'mixed')
            self.assertEqual(classify(), 'config_only=false')
            git('reset', '--hard', base)
            manifest.write_text(json.dumps(self.manifest(target, {})))
            git('add', '.'); git('commit', '-qm', 'drop ownership')
            self.assertEqual(classify(), 'config_only=true')


if __name__ == '__main__':
    unittest.main()
