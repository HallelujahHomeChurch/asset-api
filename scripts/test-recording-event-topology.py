import importlib.util
from pathlib import Path
import tempfile
import os
import subprocess
from unittest.mock import patch
import unittest

spec = importlib.util.spec_from_file_location('topology', Path(__file__).with_name('recording-event-topology.py'))
mod = importlib.util.module_from_spec(spec)
spec.loader.exec_module(mod)


class TopologyTest(unittest.TestCase):
    def setUp(self):
        self.env = patch.dict(os.environ, {'RECORDING_EVENTS_PROVISIONED':'true', 'RECORDING_EVENTS_ENABLED':'false'})
        self.env.start()
        self.calls = []
        self.temp = tempfile.TemporaryDirectory()
        self.path = Path(self.temp.name) / 'topology.json'
        self.jobs = [self.job('asset-recording-validation', 'Schedule')]
        self.active = False
        self.clock = 0
        mod.az = self.az
        mod.time.sleep = lambda _: None
        mod.time.monotonic = self.now

    def tearDown(self):
        self.temp.cleanup()
        self.env.stop()

    def now(self):
        self.clock += 100
        return self.clock

    @staticmethod
    def job(name, trigger):
        return {'name': name, 'id': '/subscriptions/sub/resourceGroups/alive/providers/Microsoft.App/jobs/'+name,
                'properties': {'configuration': {'triggerType': trigger, 'scheduleTriggerConfig': {'cronExpression': '*/1 * * * *'}, 'secrets': [{'value': 'must-not-persist'}]},
                               'template': {'containers': [{'name': name, 'image': 'registry/worker@sha256:'+'a'*64, 'env': [{'name': 'DATABASE_URL', 'secretRef': 'database-url'}, {'name': 'ASSET_RECORDING_EVENTS_ENABLED', 'value': 'false'}], 'resources': {'cpu': 4, 'memory': '8Gi'}}]}}}

    def az(self, *args):
        self.calls.append(args)
        if args[:3] == ('containerapp', 'job', 'list'):
            return self.jobs
        if args[:4] == ('containerapp', 'job', 'execution', 'list'):
            return [{'properties': {'status': 'Running'}}] if self.active else []
        return {}

    def payloads(self):
        import json
        return [(a[a.index('--url')+1], json.loads(a[a.index('--body')+1])) for a in self.calls if a[:1]==('rest',)]

    def test_failed_first_activation_restores_schedule_image_and_env(self):
        mod.capture(self.path, 'alive')
        self.assertNotIn('must-not-persist', self.path.read_text())
        self.jobs.append(self.job('asset-recording-live', 'Event'))
        mod.pause('alive')
        self.assertTrue(all(p[1]['properties']['configuration']['triggerType'] == 'Manual' for p in self.payloads()))
        self.calls.clear()
        mod.restore(self.path, 'alive')
        restored = self.payloads()[0][1]['properties']
        self.assertEqual(restored['configuration']['triggerType'], 'Schedule')
        self.assertEqual(restored['template']['containers'][0]['env'][1]['value'], 'false')
        self.assertIn('@sha256:', restored['template']['containers'][0]['image'])
        self.assertEqual(len(self.payloads()), 1)  # newly provisioned job stays Manual

    def test_previous_event_topology_restored(self):
        self.jobs = [self.job('asset-recording-live', 'Event')]
        mod.capture(self.path, 'alive')
        mod.restore(self.path, 'alive')
        self.assertEqual(self.payloads()[0][1]['properties']['configuration']['triggerType'], 'Event')

    def test_live_resource_rollback_preserves_both_tiers(self):
        for cpu, memory in [(2, '4Gi'), (4, '8Gi')]:
            with self.subTest(cpu=cpu):
                self.calls.clear()
                self.path.unlink(missing_ok=True)
                self.jobs = [self.job('asset-recording-live', 'Event')]
                self.jobs[0]['properties']['template']['containers'][0]['resources'] = {'cpu': cpu, 'memory': memory}
                mod.capture(self.path, 'alive')
                mod.restore(self.path, 'alive')
                restored = self.payloads()[0][1]['properties']['template']['containers'][0]['resources']
                self.assertEqual(restored, {'cpu': cpu, 'memory': memory})

    def test_job_readback_accepts_selected_live_tier_and_rejects_drift(self):
        az = Path(self.temp.name) / 'az'
        az.write_text('''#!/usr/bin/env python3
import os, sys
args = sys.argv
name = args[args.index('-n')+1]
query = args[args.index('--query')+1]
cpu = {'live': int(os.environ['ACTUAL_LIVE_CPU']), 'processing': 2, 'preview': 1, 'maintenance': .5}.get(name.removeprefix('asset-recording-'), 4)
memory = {4:'8Gi', 2:'4Gi', 1:'2Gi', .5:'1Gi'}[cpu]
values = {'properties.configuration.triggerType': 'Schedule' if name.endswith('maintenance') else 'Manual' if name.endswith('validation') else 'Event',
'properties.template.containers[0].image': 'fixture@sha256:abc',
'properties.template.containers[0].resources.cpu': cpu,
'properties.template.containers[0].resources.memory': memory,
'properties.configuration.eventTriggerConfig.scale.minExecutions': 0,
'properties.configuration.eventTriggerConfig.scale.maxExecutions': 1,
'properties.configuration.eventTriggerConfig.scale.rules[0].metadata.queueLengthStrategy': 'visibleonly',
'properties.configuration.scheduleTriggerConfig.cronExpression': '17 * * * *'}
print(values[query])
''')
        az.chmod(0o755)
        for selected, actual, success in [('2', '2', True), ('4', '4', True), ('4', '2', False), ('2', '4', False)]:
            with self.subTest(selected=selected, actual=actual):
                env = {**os.environ, 'PATH': self.temp.name+os.pathsep+os.environ['PATH'], 'RESOURCE_GROUP': 'fixture',
                       'RECORDING_EVENTS_PROVISIONED': 'true', 'RECORDING_EVENTS_ENABLED': 'true',
                       'RECORDING_LIVE_CPU': selected, 'ACTUAL_LIVE_CPU': actual, 'RECORDING_IMAGE_REF': 'fixture@sha256:abc'}
                result = subprocess.run(['bash', 'scripts/verify-recording-jobs.sh'], env=env, capture_output=True, text=True)
                self.assertEqual(result.returncode == 0, success, result.stderr)

    def test_active_execution_does_not_restore_competing_schedule(self):
        mod.capture(self.path, 'alive')
        self.active = True
        with self.assertRaisesRegex(RuntimeError, 'still active'):
            mod.restore(self.path, 'alive')
        self.assertEqual(self.payloads(), [])

    def test_inline_secret_and_foreign_job_rejected(self):
        self.jobs[0]['properties']['template']['containers'][0]['env'][0] = {'name': 'DATABASE_URL', 'value': 'credential'}
        with self.assertRaises(ValueError):
            mod.capture(self.path, 'alive')
        self.assertFalse(self.path.exists())
        self.jobs = [self.job('unrelated', 'Schedule')]
        mod.pause('alive')
        self.assertEqual(self.payloads(), [])

    def test_switch_refuses_active_work(self):
        self.active = True
        os.environ['RECORDING_EVENTS_ENABLED'] = 'true'
        with self.assertRaisesRegex(RuntimeError, 'still active'):
            mod.capture(self.path, 'alive')
        self.assertFalse(self.path.exists())

    def test_capture_waits_for_short_scheduled_execution_without_mutating(self):
        os.environ['RECORDING_EVENTS_ENABLED'] = 'true'
        checks = []
        def finishing_execution(*args):
            if args[:4] == ('containerapp', 'job', 'execution', 'list'):
                checks.append(args)
                self.active = len(checks) == 1
            return self.az(*args)
        mod.az = finishing_execution
        mod.capture(self.path, 'alive')
        self.assertGreaterEqual(len(checks), 2)
        self.assertTrue(self.path.exists())
        self.assertEqual(self.payloads(), [])

    def test_quiesce_waits_for_short_execution_before_pausing(self):
        mod.capture(self.path, 'alive')
        checks = []
        def finishing_execution(*args):
            if args[:4] == ('containerapp', 'job', 'execution', 'list'):
                checks.append(args)
                self.active = len(checks) == 1
            return self.az(*args)
        mod.az = finishing_execution
        output = Path(self.temp.name) / 'outputs'
        with patch.dict(os.environ, {'RECORDING_EVENTS_ENABLED': 'true', 'GITHUB_OUTPUT': str(output)}):
            mod.quiesce(self.path, 'alive')
        self.assertIn('paused=true', output.read_text())
        self.assertGreaterEqual(len(checks), 3)
        self.assertTrue(all(p[1]['properties']['configuration']['triggerType'] == 'Manual' for p in self.payloads()))

    def test_missing_provisioned_flag_cannot_orphan_existing_triggers(self):
        self.jobs.append(self.job('asset-recording-live', 'Event'))
        os.environ['RECORDING_EVENTS_PROVISIONED'] = 'false'
        with self.assertRaisesRegex(ValueError, 'orphan'):
            mod.capture(self.path, 'alive')
        self.assertFalse(self.path.exists())

    def test_failed_drain_snapshot_survives_runner_cleanup(self):
        workflow = Path('.github/workflows/release.yml').read_text()
        section = workflow[workflow.index('      - name: Preserve non-secret recording rollback topology'):workflow.index('  deploy_media:')]
        self.assertIn("(failure() || cancelled()) && env.RECORDING_EVENTS_PROVISIONED == 'true'", section)
        self.assertIn('recording-topology.json', section)
        self.assertIn('retention-days: 7', section)

    def test_default_off_quiesce_does_not_touch_jobs(self):
        mod.quiesce(self.path, 'alive')
        self.assertEqual(self.calls, [])

    def test_switch_quiesces_before_runtime_activation(self):
        mod.capture(self.path,'alive')
        os.environ['RECORDING_EVENTS_ENABLED']='true'
        output=Path(self.temp.name)/'outputs'
        os.environ['GITHUB_OUTPUT']=str(output)
        try:
            mod.quiesce(self.path,'alive')
            self.assertIn('paused=true',output.read_text())
            self.assertTrue(all(p[1]['properties']['configuration']['triggerType']=='Manual' for p in self.payloads()))
        finally:
            os.environ.pop('GITHUB_OUTPUT',None)

    def test_quiesce_rejects_active_work_before_mutation(self):
        mod.capture(self.path,'alive')
        os.environ['RECORDING_EVENTS_ENABLED']='true'
        self.active=True
        with self.assertRaisesRegex(RuntimeError,'active'):
            mod.quiesce(self.path,'alive')
        self.assertEqual(self.payloads(),[])

    def test_partial_pause_failure_retains_rollback_responsibility(self):
        self.jobs.append(self.job('asset-recording-live', 'Manual'))
        mod.capture(self.path, 'alive')
        output = Path(self.temp.name) / 'outputs'
        def fail_second_patch(*args):
            if args[:1] == ('rest',) and self.payloads():
                raise RuntimeError('fixture patch failed')
            return self.az(*args)
        mod.az = fail_second_patch
        with patch.dict(os.environ, {'RECORDING_EVENTS_ENABLED': 'true', 'GITHUB_OUTPUT': str(output)}):
            with self.assertRaisesRegex(RuntimeError, 'fixture patch failed'):
                mod.quiesce(self.path, 'alive')
        self.assertIn('paused=true', output.read_text())
        self.assertEqual(len(self.payloads()), 1)
        self.assertEqual(self.payloads()[0][1]['properties']['configuration']['triggerType'], 'Manual')

    def test_execution_racing_with_pause_leaves_triggers_manual(self):
        mod.capture(self.path, 'alive')
        output = Path(self.temp.name) / 'outputs'
        def start_during_pause(*args):
            result = self.az(*args)
            if args[:1] == ('rest',):
                self.active = True
            return result
        mod.az = start_during_pause
        with patch.dict(os.environ, {'RECORDING_EVENTS_ENABLED': 'true', 'GITHUB_OUTPUT': str(output)}):
            with self.assertRaisesRegex(RuntimeError, 'still active'):
                mod.quiesce(self.path, 'alive')
        self.assertIn('paused=true', output.read_text())
        self.assertTrue(all(p[1]['properties']['configuration']['triggerType'] == 'Manual' for p in self.payloads()))

    def test_workflow_coordinates_failure_recovery(self):
        workflow = Path('.github/workflows/release.yml').read_text()
        rollback = workflow[workflow.index('      - name: Roll back failed runtime'):workflow.index('  deploy_media:')]
        self.assertLess(rollback.index('recording-event-topology.py pause'), rollback.index('az containerapp revision copy'))
        self.assertLess(rollback.index('az containerapp revision copy'), rollback.index('recording-event-topology.py restore'))
        self.assertLess(workflow.index('recording-event-topology.py capture'), workflow.index('      - name: Deploy API'))
        self.assertLess(workflow.index('      - name: Run migrations'), workflow.index('recording-event-topology.py quiesce'))
        self.assertLess(workflow.index('recording-event-topology.py quiesce'), workflow.index('      - name: Deploy API'))
        self.assertIn("steps.quiesce_recording.outputs.paused == 'true'",rollback)


if __name__ == '__main__':
    unittest.main()
