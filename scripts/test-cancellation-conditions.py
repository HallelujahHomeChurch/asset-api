"""Exercise production recovery conditions against cancellation timing boundaries."""
import ast
from pathlib import Path
import re
import unittest

RELEASE = Path('.github/workflows/release.yml').read_text()
CI = Path('.github/workflows/ci.yml').read_text()


def condition(text, name):
    block = text.split('      - name: ' + name + '\n', 1)[1].split('\n      - ', 1)[0]
    return next(line.strip()[4:] for line in block.splitlines() if line.strip().startswith('if: '))


def evaluate(expression, *, failed=False, cancelled=False, runtime='skipped', settled='skipped', captured='success', paused='', previous='old', media='skipped'):
    values = {'steps.deploy_runtime.outcome': runtime, 'steps.settle_runtime.outcome': settled,
              'steps.capture_workers.outcome': captured, 'steps.quiesce_recording.outputs.paused': paused,
              'env.PREVIOUS_READY_REVISION': previous, 'env.PREVIOUS_MEDIA_DEPLOYMENT': previous,
              'steps.deploy_media_function.outcome': media}
    expression = expression.removeprefix('${{').removesuffix('}}').strip()
    expression = expression.replace('failure()', str(failed)).replace('cancelled()', str(cancelled))
    for key, value in values.items():
        expression = expression.replace(key, repr(value))
    expression = expression.replace('&&', ' and ').replace('||', ' or ')
    tree = ast.parse(expression, mode='eval')
    allowed = (ast.Expression, ast.BoolOp, ast.And, ast.Or, ast.Compare, ast.Eq, ast.NotEq, ast.Constant)
    if any(not isinstance(node, allowed) for node in ast.walk(tree)):
        raise ValueError('unexpected recovery expression syntax')
    return eval(compile(tree, '<recovery condition>', 'eval'), {'__builtins__': {}}, {})


class CancellationConditions(unittest.TestCase):
    def test_actual_fixture_uses_production_conditions(self):
        for name in ['Settle interrupted runtime deployment', 'Roll back failed runtime']:
            self.assertEqual(condition(RELEASE, name), condition(CI, name))
        self.assertIn('permissions:\n      contents: read', CI.split('  cancellation_fixture:', 1)[1])
        self.assertNotIn('azure/login', CI.split('  cancellation_fixture:', 1)[1])

    def test_settlement_does_not_touch_an_unstarted_deployment(self):
        expr = condition(RELEASE, 'Settle interrupted runtime deployment')
        self.assertFalse(evaluate(expr, cancelled=True, runtime='skipped'))
        self.assertFalse(evaluate(expr, runtime='success'))
        self.assertTrue(evaluate(expr, cancelled=True, runtime='cancelled'))

    def test_runtime_recovery_only_after_mutation_and_settlement(self):
        expr = condition(RELEASE, 'Roll back failed runtime')
        cases = [({}, False), ({'cancelled': True}, False),
                 ({'failed': True, 'runtime': 'failure', 'settled': 'success'}, True),
                 ({'cancelled': True, 'runtime': 'cancelled', 'settled': 'success'}, True),
                 ({'cancelled': True, 'runtime': 'success', 'settled': 'success'}, True),
                 ({'cancelled': True, 'runtime': 'cancelled', 'settled': 'failure'}, False),
                 ({'cancelled': True, 'runtime': 'cancelled', 'settled': 'success', 'previous': ''}, False),
                 ({'cancelled': True, 'paused': 'true'}, True)]
        for values, expected in cases:
            with self.subTest(values=values):
                self.assertEqual(evaluate(expr, **values), expected)

    def test_worker_recovery_requires_snapshot_and_settlement(self):
        expr = condition(RELEASE, 'Restore existing worker definitions on failure')
        self.assertTrue(evaluate(expr, cancelled=True, runtime='cancelled', settled='success'))
        self.assertFalse(evaluate(expr, cancelled=True, runtime='cancelled', settled='failure'))
        self.assertFalse(evaluate(expr, cancelled=True, runtime='cancelled', settled='success', captured='cancelled'))
        self.assertFalse(evaluate(expr, failed=True, runtime='skipped', settled='success'))

    def test_media_recovery_requires_possible_deployment_mutation(self):
        expr = condition(RELEASE, 'Restore previous media deployment on failure')
        self.assertTrue(evaluate(expr, cancelled=True, media='cancelled'))
        self.assertFalse(evaluate(expr, cancelled=True, media='skipped'))
        self.assertFalse(evaluate(expr, failed=True, media='failure', previous=''))


if __name__ == '__main__':
    unittest.main()
