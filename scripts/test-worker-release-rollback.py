import copy
import importlib.util
import json
import os
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch

spec = importlib.util.spec_from_file_location('rollback', Path(__file__).with_name('worker-release-rollback.py'))
mod = importlib.util.module_from_spec(spec)
spec.loader.exec_module(mod)
OLD = 'alive.azurecr.io/alive/asset-api@sha256:' + 'a'*64
NEW = 'alive.azurecr.io/alive/asset-api@sha256:' + 'b'*64


class RecoveryTest(unittest.TestCase):
    def setUp(self):
        self.env = patch.dict(os.environ, {'DEPLOY_RETENTION_JOB': 'true', 'GITHUB_RUN_ID': '123', 'GITHUB_RUN_ATTEMPT': '2'})
        self.env.start()
        self.temp = tempfile.TemporaryDirectory()
        self.path = Path(self.temp.name) / 'snapshot.json'
        self.calls = []
        self.fail = set()
        self.drift = set()
        self.states = {}
        self.jobs = {name: {'configuration': {k: None for k in mod.CONFIG}, 'template': {'containers': [
            {'name': name, 'image': OLD, 'command': ['/asset-scan-worker'],
             'resources': {'cpu': 2, 'memory': '4Gi'}, 'env': [{'name': 'DATABASE_URL', 'secretRef': 'database-url'}]}]}}
            for name in mod.selected_jobs()}
        self.az_patch = patch.object(mod, 'az', self.az)
        self.az_patch.start()
        self.sleep = patch.object(mod.time, 'sleep')
        self.sleep.start()
        self.scan_ready = True

    def tearDown(self):
        self.sleep.stop()
        self.az_patch.stop()
        self.env.stop()
        self.temp.cleanup()

    def az(self, *args):
        self.calls.append(args)
        if args[:3] == ('containerapp', 'job', 'show'):
            name = args[args.index('-n')+1]
            if name not in self.jobs:
                raise RuntimeError('missing resource')
            if args[-1] == 'properties.provisioningState':
                states = self.states.get(name, ['Succeeded'])
                return states.pop(0) if len(states) > 1 else states[0]
            if args[-1] == 'id':
                return '/subscriptions/1234-abcd/resourceGroups/alive/providers/Microsoft.App/jobs/' + name
            return copy.deepcopy(self.jobs[name])
        if args[:1] == ('rest',):
            name = args[args.index('--url')+1].split('/')[-1].split('?')[0]
            if name in self.fail:
                raise RuntimeError('patch failed')
            if name not in self.drift:
                self.jobs[name] = json.loads(args[args.index('--body')+1])['properties']
            return {}
        if args[:3] == ('containerapp', 'revision', 'copy'):
            if 'scan' in self.fail:
                raise RuntimeError('copy failed')
            return {}
        if args[:3] == ('containerapp', 'revision', 'show'):
            return OLD
        if args[:2] == ('containerapp', 'show'):
            if args[-1] == '{revision:properties.latestReadyRevisionName}':
                return {'revision': 'asset-scan-worker--old'}
            name = 'asset-scan-worker--rollback-123-2' if self.scan_ready else 'asset-scan-worker--new'
            return {'latest': name, 'ready': name}
        raise AssertionError('unexpected operation')

    def deploy(self):
        for value in self.jobs.values():
            value['template']['containers'][0]['image'] = NEW
            value['configuration']['replicaTimeout'] = 99

    def test_restore_images_templates_and_flags_without_execution_mutation(self):
        mod.capture(self.path)
        original = copy.deepcopy(self.jobs)
        self.assertEqual(self.path.stat().st_mode & 0o777, 0o600)
        self.deploy()
        mod.restore(self.path)
        self.assertEqual(self.jobs, original)
        self.assertFalse(any('execution' in call or 'stop' in call or 'delete' in call for call in self.calls))

    def test_live_image_type_default_is_normalized_and_unknown_fails(self):
        for value in self.jobs.values():
            value['template']['containers'][0]['imageType'] = 'ContainerImage'
        mod.capture(self.path)
        self.assertNotIn('imageType', self.path.read_text())
        self.deploy()
        mod.restore(self.path)
        self.path.unlink()
        self.jobs['asset-scan']['template']['containers'][0]['imageType'] = 'Unknown'
        with self.assertRaises(ValueError):
            mod.capture(self.path)
        self.assertFalse(self.path.exists())

    def test_disabled_retention_is_never_read_or_written(self):
        os.environ['DEPLOY_RETENTION_JOB'] = 'false'
        mod.capture(self.path)
        self.deploy()
        mod.restore(self.path)
        self.assertFalse(any('asset-retention' in call for call in self.calls))
        self.assertEqual(self.jobs['asset-retention']['template']['containers'][0]['image'], NEW)

    def test_partial_failure_attempts_all_other_jobs_and_scan(self):
        mod.capture(self.path)
        self.deploy()
        self.fail.add('asset-scan')
        with self.assertRaisesRegex(RuntimeError, 'asset-scan'):
            mod.restore(self.path)
        self.assertEqual(self.jobs['asset-derivative']['template']['containers'][0]['image'], OLD)
        self.assertTrue(any(c[:3] == ('containerapp', 'revision', 'copy') for c in self.calls))

    def test_readback_drift_and_scan_failure_are_reported(self):
        mod.capture(self.path)
        self.deploy()
        self.drift.add('asset-derivative')
        self.fail.add('scan')
        with self.assertRaisesRegex(RuntimeError, 'asset-derivative, asset-scan-worker'):
            mod.restore(self.path)

    def test_async_patch_waits_for_success_not_just_desired_template(self):
        mod.capture(self.path)
        self.deploy()
        self.states['asset-scan'] = ['Updating', 'Updating', 'Succeeded']
        mod.restore(self.path)
        checks = [c for c in self.calls if c[-1] == 'properties.provisioningState' and 'asset-scan' in c]
        self.assertEqual(len(checks), 3)

    def test_async_failed_and_stuck_jobs_fail_but_others_restore(self):
        for state in ('Failed', 'Updating'):
            with self.subTest(state=state):
                self.path.unlink(missing_ok=True)
                mod.capture(self.path)
                self.deploy()
                self.states['asset-scan'] = [state]
                with self.assertRaisesRegex(RuntimeError, 'asset-scan'):
                    mod.restore(self.path)
                self.assertEqual(self.jobs['asset-derivative']['template']['containers'][0]['image'], OLD)
                self.states.clear()
                self.jobs['asset-scan']['template']['containers'][0]['image'] = OLD

    def test_missing_job_blocks_capture_before_artifact(self):
        del self.jobs['asset-scan']
        with self.assertRaises(RuntimeError):
            mod.capture(self.path)
        self.assertFalse(self.path.exists())

    def test_missing_job_during_recovery_does_not_skip_other_resources(self):
        mod.capture(self.path)
        self.deploy()
        del self.jobs['asset-scan']
        with self.assertRaises(RuntimeError):
            mod.restore(self.path)
        self.assertEqual(self.jobs['asset-derivative']['template']['containers'][0]['image'], OLD)

    def test_scan_must_reach_exact_recovery_revision(self):
        mod.capture(self.path)
        self.scan_ready = False
        with self.assertRaisesRegex(RuntimeError, 'asset-scan-worker'):
            mod.restore(self.path)

    def test_literal_credentials_and_unreviewed_template_fail_closed(self):
        for change in ('literal', 'args', 'tag', 'metadata'):
            with self.subTest(change=change):
                value = copy.deepcopy(self.jobs['asset-scan'])
                container = value['template']['containers'][0]
                if change == 'literal':
                    container['env'] = [{'name': 'DATABASE_URL', 'value': 'never-persist-this'}]
                elif change == 'args':
                    container['args'] = ['never-persist-this']
                elif change == 'tag':
                    container['image'] = 'alive.azurecr.io/alive/asset-api:latest'
                else:
                    value['configuration']['eventTriggerConfig'] = {'scale': {'rules': [
                        {'type': 'azure-queue', 'metadata': {'connectionString': 'never-persist-this'}}]}}
                with self.assertRaises(ValueError):
                    mod.validate_job(value)

    def test_inventory_tamper_fails_before_mutation(self):
        mod.capture(self.path)
        value = json.loads(self.path.read_text())
        value['jobs']['another-job'] = value['jobs'].pop('asset-scan')
        self.path.write_text(json.dumps(value))
        self.calls.clear()
        with self.assertRaises(ValueError):
            mod.restore(self.path)
        self.assertEqual(self.calls, [])

    def test_workflow_attempts_workers_even_if_prior_rollback_failed(self):
        workflow = Path('.github/workflows/release.yml').read_text()
        restore = workflow.split('      - name: Restore existing worker definitions on failure')[1].split('      - name:')[0]
        self.assertIn('failure()', restore)
        self.assertIn("steps.capture_workers.outcome == 'success'", restore)
        self.assertNotIn('rollback.outcome', restore)
        self.assertIn("steps.deploy_runtime.outcome == 'failure'", restore)
        self.assertLess(workflow.index('worker-release-rollback.py capture'), workflow.index('      - name: Deploy API'))


if __name__ == '__main__':
    unittest.main()
