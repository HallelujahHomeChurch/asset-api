import importlib.util
from pathlib import Path
import tempfile
import unittest

spec = importlib.util.spec_from_file_location('topology', Path(__file__).with_name('recording-event-topology.py'))
mod = importlib.util.module_from_spec(spec)
spec.loader.exec_module(mod)


class TopologyTest(unittest.TestCase):
    def setUp(self):
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

    def test_workflow_coordinates_failure_recovery(self):
        workflow = Path('.github/workflows/release.yml').read_text()
        rollback = workflow[workflow.index('      - name: Roll back failed runtime'):workflow.index('  deploy_media:')]
        self.assertLess(rollback.index('recording-event-topology.py pause'), rollback.index('az containerapp revision copy'))
        self.assertLess(rollback.index('az containerapp revision copy'), rollback.index('recording-event-topology.py restore'))
        self.assertLess(workflow.index('recording-event-topology.py capture'), workflow.index('      - name: Deploy API'))


if __name__ == '__main__':
    unittest.main()
