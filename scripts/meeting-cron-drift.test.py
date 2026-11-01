#!/usr/bin/env python3
"""Offline tests for the asset-scan-worker meeting cron drift check and scale verification."""
import contextlib
import datetime as dt
import importlib.util
import io
import json
from pathlib import Path
import tempfile
import unittest

ROOT = Path(__file__).resolve().parent.parent
spec = importlib.util.spec_from_file_location("meeting_cron_drift", ROOT / "scripts" / "meeting-cron-drift.py")
drift = importlib.util.module_from_spec(spec)
spec.loader.exec_module(drift)
verify_spec = importlib.util.spec_from_file_location("verify_scale", ROOT / "scripts" / "verify-scan-worker-scale.py")
verify_scale = importlib.util.module_from_spec(verify_spec)
verify_spec.loader.exec_module(verify_scale)

# Sunday 2026-11-01 00:00 Asia/Taipei.
NOW = dt.datetime(2026, 10, 31, 16, 0, tzinfo=dt.timezone.utc)

TEMPLATE = """resource other 'Microsoft.App/containerApps@2025-01-01' = {
  name: 'other-app'
  properties: {
    template: {
      scale: {
        rules: [
          {
            name: 'ignored-other-app'
            custom: {
              type: 'cron'
              metadata: {
                timezone: 'UTC'
                start: '0 0 * * 1'
                end: '0 1 * * 1'
                desiredReplicas: '1'
              }
            }
          }
        ]
      }
    }
  }
}

resource scanWorker 'Microsoft.App/containerApps@2025-01-01' = if (deployScanWorker) {
  name: 'asset-scan-worker'
  properties: {
    template: {
      containers: [
        {
          name: 'asset-scan-worker'
          command: ['/asset-scan-worker']
          env: [
            { name: 'URL', value: 'https://${storageAccount.name}.queue/asset-scan' }
          ]
        }
      ]
      scale: {
        pollingInterval: 5
        minReplicas: 0
        rules: [
          {
            name: 'asset-scan-queue'
            custom: {
              type: 'azure-queue'
              metadata: {
                accountName: storageAccount.name
                queueName: 'asset-scan'
                queueLength: '1'
              }
              identity: scanIdentity.id
            }
          }
          // comment lines are ignored
          {
            name: 'tue-fri-0625'
            custom: {
              type: 'cron'
              metadata: {
                timezone: 'Asia/Taipei'
                start: '25 6 * * 2,5'
                end: '40 8 * * 2,5'
                desiredReplicas: '1'
              }
            }
          }
          {
            name: 'sun-0855' // trailing comment
            custom: {
              type: 'cron'
              metadata: {
                timezone: 'Asia/Taipei'
                start: '55 8 * * 0'
                end: '10 12 * * 0'
                desiredReplicas: '1'
              }
            }
          }
        ]
      }
    }
  }
  dependsOn: [acrPull]
}
"""


def occurrence(starts_at, ends_at, status="scheduled", key="meeting"):
    return {"meetingKey": key, "startsAt": starts_at, "endsAt": ends_at, "status": status}


class MeetingCronDriftTest(unittest.TestCase):
    def gaps(self, *occurrences):
        result = drift.check(list(occurrences), TEMPLATE, NOW)
        return [item["meetingKey"] for item in result["gaps"]], result

    def test_parses_only_cron_rules(self):
        names = [rule["name"] for rule in drift.cron_rules(TEMPLATE)]
        self.assertEqual(names, ["tue-fri-0625", "sun-0855"])

    def test_covered_occurrence(self):
        gaps, result = self.gaps(
            occurrence("2026-11-03T06:30:00+08:00", "2026-11-03T08:30:00+08:00", key="tue"),
            occurrence("2026-11-06T06:30:00+08:00", "2026-11-06T08:30:00+08:00", key="fri"),
            occurrence("2026-11-01T01:00:00Z", "2026-11-01T04:00:00Z", key="sun-utc"),
        )
        self.assertEqual(gaps, [])
        self.assertEqual(len(result["occurrences"]), 3)

    def test_occurrence_starting_before_window(self):
        gaps, _ = self.gaps(occurrence("2026-11-03T06:29:00+08:00", "2026-11-03T08:30:00+08:00", key="early"))
        self.assertEqual(gaps, ["early"])

    def test_occurrence_ending_after_window(self):
        gaps, _ = self.gaps(occurrence("2026-11-03T06:30:00+08:00", "2026-11-03T08:31:00+08:00", key="late"))
        self.assertEqual(gaps, ["late"])

    def test_wrong_weekday(self):
        gaps, _ = self.gaps(occurrence("2026-11-04T06:30:00+08:00", "2026-11-04T08:30:00+08:00", key="wed"))
        self.assertEqual(gaps, ["wed"])

    def test_non_scheduled_ignored(self):
        gaps, result = self.gaps(
            occurrence("2026-11-04T06:30:00+08:00", "2026-11-04T08:30:00+08:00", status="cancelled", key="x")
        )
        self.assertEqual(gaps, [])
        self.assertEqual(result["occurrences"], [])

    def test_outside_horizon_and_finished_ignored(self):
        gaps, result = self.gaps(
            occurrence("2026-12-02T06:30:00+08:00", "2026-12-02T08:30:00+08:00", key="far"),
            occurrence("2026-10-28T06:30:00+08:00", "2026-10-28T08:30:00+08:00", key="past"),
        )
        self.assertEqual(gaps, [])
        self.assertEqual(result["occurrences"], [])

    def test_accepts_envelope(self):
        payload = {"occurrences": [occurrence("2026-11-04T06:30:00+08:00", "2026-11-04T08:30:00+08:00")]}
        self.assertEqual(len(drift.check(payload, TEMPLATE, NOW)["gaps"]), 1)

    def test_rejects_unsupported_cron(self):
        for expression in ["*/5 6 * * 2", "25 6 1 * 2", "25 6 * * 1-5", "25 6 * * 7", "60 6 * * 2", "25 6 * *"]:
            with self.subTest(expression=expression), self.assertRaises(drift.DriftInputError):
                drift.parse_cron(expression)
        broken = TEMPLATE.replace("end: '10 12 * * 0'", "end: '10 12 * * 1'")
        with self.assertRaises(drift.DriftInputError):
            drift.cron_rules(broken)
        wrong_zone = TEMPLATE.replace("timezone: 'Asia/Taipei'", "timezone: 'UTC'", 1)
        with self.assertRaises(drift.DriftInputError):
            drift.cron_rules(wrong_zone)

    def test_rejects_malformed_payload(self):
        for payload in [{"unexpected": 1}, [1], [occurrence("not-a-date", "2026-11-04T08:30:00+08:00")]]:
            with self.subTest(payload=payload), self.assertRaises(drift.DriftInputError):
                drift.check(payload, TEMPLATE, NOW)

    def test_suggested_rule(self):
        item = drift.upcoming([occurrence("2026-11-04T19:00:00+08:00", "2026-11-04T21:30:00+08:00")], NOW)[0]
        self.assertEqual(
            drift.suggested_rule(item), {"name": "wed-1855", "start": "55 18 * * 3", "end": "40 21 * * 3"}
        )

    def test_main_exit_codes_and_report(self):
        with tempfile.TemporaryDirectory() as directory:
            base = Path(directory)
            template = base / "main.bicep"
            template.write_text(TEMPLATE, encoding="utf-8")
            report = base / "report.md"
            covered = base / "covered.json"
            covered.write_text(
                json.dumps([occurrence("2026-11-03T06:30:00+08:00", "2026-11-03T08:30:00+08:00")]),
                encoding="utf-8",
            )
            gap = base / "gap.json"
            gap.write_text(
                json.dumps([occurrence("2026-11-04T06:30:00+08:00", "2026-11-04T08:30:00+08:00", key="wed")]),
                encoding="utf-8",
            )
            invalid = base / "invalid.json"
            invalid.write_text("{}", encoding="utf-8")
            common = ["--template", str(template), "--report", str(report), "--now", NOW.isoformat()]
            with contextlib.redirect_stdout(io.StringIO()), contextlib.redirect_stderr(io.StringIO()):
                self.assertEqual(drift.main(["--input", str(covered), *common]), 0)
                self.assertFalse(report.exists())
                self.assertEqual(drift.main(["--input", str(gap), *common]), 1)
                self.assertEqual(drift.main(["--input", str(invalid), *common]), 2)
            text = report.read_text(encoding="utf-8")
            self.assertIn("| wed | 2026-11-04T06:30+08:00", text)
            self.assertIn("start: '25 6 * * 3'", text)

    def test_rejects_unsupported_bicep(self):
        variants = [
            TEMPLATE.replace("start: '25 6 * * 2,5'", "start: '${cronStart}'"),
            TEMPLATE.replace("            name: 'tue-fri-0625'\n", "            name: 'tue-fri-0625'\n            name: 'dup'\n"),
            TEMPLATE.replace("  name: 'asset-scan-worker'", "  name: 'renamed-worker'"),
            TEMPLATE.replace("        rules: [\n          {\n            name: 'asset-scan-queue'", "        rules: [ {\n            name: 'asset-scan-queue'"),
            TEMPLATE + TEMPLATE[TEMPLATE.index("resource scanWorker"):],
            TEMPLATE.replace("desiredReplicas: '1'", "desiredReplicas: '2'"),
        ]
        for index, variant in enumerate(variants):
            with self.subTest(index=index), self.assertRaises(drift.DriftInputError):
                drift.cron_rules(variant)

    def test_repository_template_parses(self):
        rules = drift.cron_rules(drift.DEFAULT_TEMPLATE.read_text(encoding="utf-8"))
        self.assertEqual(
            [(rule["name"], rule["startExpression"], rule["endExpression"]) for rule in rules],
            [
                ("tue-fri-0625", "25 6 * * 2,5", "40 8 * * 2,5"),
                ("thu-1155", "55 11 * * 4", "10 14 * * 4"),
                ("fri-1855", "55 18 * * 5", "40 21 * * 5"),
                ("sat-0855", "55 8 * * 6", "40 11 * * 6"),
                ("sun-0855", "55 8 * * 0", "10 12 * * 0"),
            ],
        )


def deployed_rules():
    """Deployed ACA shape: queue rules are echoed as azureQueue, cron stays custom."""
    rules = [{"name": "asset-scan-queue", "azureQueue": {"queueName": "asset-scan", "queueLength": 1}}]
    for name, start, end in [("tue-fri-0625", "25 6 * * 2,5", "40 8 * * 2,5"), ("sun-0855", "55 8 * * 0", "10 12 * * 0")]:
        rules.append({"name": name, "custom": {"type": "cron", "metadata": {
            "timezone": "Asia/Taipei", "start": start, "end": end, "desiredReplicas": "1"}}})
    return rules


class VerifyScaleTest(unittest.TestCase):
    def test_matching_rules(self):
        self.assertTrue(verify_scale.verify(deployed_rules(), TEMPLATE))

    def test_rejects_drift(self):
        warm = {"name": "asset-scan-warm", "azureQueue": {"queueName": "asset-scan-warm", "queueLength": 20}}
        changed = deployed_rules()
        changed[1]["custom"]["metadata"]["end"] = "45 8 * * 2,5"
        variants = [
            [*deployed_rules(), warm],
            deployed_rules()[:-1],
            changed,
            [*deployed_rules(), deployed_rules()[1]],
            [rule for rule in deployed_rules() if rule["name"] != "asset-scan-queue"],
            {"rules": deployed_rules()},
        ]
        for index, variant in enumerate(variants):
            with self.subTest(index=index):
                self.assertFalse(verify_scale.verify(variant, TEMPLATE))


if __name__ == "__main__":
    unittest.main()
