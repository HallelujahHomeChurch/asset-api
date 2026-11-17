#!/usr/bin/env python3
import importlib.util
from pathlib import Path
import unittest

spec = importlib.util.spec_from_file_location("scale", Path(__file__).with_name("verify-scan-worker-scale.py"))
scale = importlib.util.module_from_spec(spec)
spec.loader.exec_module(scale)

class ScaleTests(unittest.TestCase):
    def test_queue_shapes(self):
        metadata = {"queueName": "asset-scan", "queueLength": "1"}
        self.assertTrue(scale.verify([{"name": "asset-scan-queue", "azureQueue": metadata}]))
        self.assertTrue(scale.verify([{"name": "asset-scan-queue", "custom": {"type": "azure-queue", "metadata": metadata}}]))

    def test_rejects_missing_queue_warming_and_changed_capacity(self):
        queue = {"name": "asset-scan-queue", "azureQueue": {"queueName": "asset-scan", "queueLength": 1}}
        for value in (None, [], {}, [queue, queue], [queue, {"name": "meeting", "custom": {"type": "cron"}}],
                      [{"name": "asset-scan-queue", "azureQueue": {"queueName": "asset-scan-warm", "queueLength": 1}}],
                      [{"name": "asset-scan-queue", "azureQueue": {"queueName": "asset-scan", "queueLength": 20}}]):
            with self.subTest(value=value):
                self.assertFalse(scale.verify(value))

if __name__ == "__main__":
    unittest.main()
