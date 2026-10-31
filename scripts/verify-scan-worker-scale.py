#!/usr/bin/env python3
"""Verify the deployed asset-scan-worker scale rules against infra/main.bicep.

Reads `properties.template.scale.rules` JSON on stdin. The deployed app must
carry exactly the `asset-scan-queue` rule plus the cron rules declared in the
template (with identical metadata), and no `asset-scan-warm` queue rule.
"""
import importlib.util
import json
from pathlib import Path
import sys

SCRIPTS = Path(__file__).resolve().parent
spec = importlib.util.spec_from_file_location("meeting_cron_drift", SCRIPTS / "meeting-cron-drift.py")
drift = importlib.util.module_from_spec(spec)
spec.loader.exec_module(drift)

QUEUE_RULE = "asset-scan-queue"
CRON_KEYS = ("timezone", "start", "end", "desiredReplicas")


def expected_cron(template_text):
    return {
        rule["name"]: {
            "timezone": "Asia/Taipei",
            "start": rule["startExpression"],
            "end": rule["endExpression"],
            "desiredReplicas": "1",
        }
        for rule in drift.cron_rules(template_text)
    }


def verify(rules, template_text):
    if not isinstance(rules, list) or not all(isinstance(rule, dict) for rule in rules):
        return False
    names = [rule.get("name") for rule in rules]
    # Compare only the declared metadata keys; ACA may echo extra server-side fields.
    observed = {
        rule.get("name"): {key: (rule["custom"].get("metadata") or {}).get(key) for key in CRON_KEYS}
        for rule in rules
        if isinstance(rule.get("custom"), dict) and rule["custom"].get("type") == "cron"
    }
    expected = expected_cron(template_text)
    return len(names) == len(set(names)) and set(names) == {QUEUE_RULE, *expected} and observed == expected


def main(argv=None):
    template = Path(argv[0]) if argv else SCRIPTS.parent / "infra" / "main.bicep"
    try:
        ok = verify(json.load(sys.stdin), template.read_text(encoding="utf-8"))
    except (drift.DriftInputError, OSError, ValueError) as error:
        print(f"scan worker scale verification failed: {error}", file=sys.stderr)
        return 2
    if not ok:
        print("asset-scan-worker scale rules do not match infra/main.bicep", file=sys.stderr)
        return 1
    print("asset-scan-worker scale rules match infra/main.bicep")
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
