#!/usr/bin/env python3
"""Verify the shared scanner retains only its work queue, without LINE warming."""
import json
import sys


def verify(rules):
    if not isinstance(rules, list) or len(rules) != 1 or not isinstance(rules[0], dict):
        return False
    rule = rules[0]
    queue = rule.get("azureQueue")
    if not isinstance(queue, dict):
        custom = rule.get("custom") or {}
        if custom.get("type") != "azure-queue":
            return False
        queue = custom.get("metadata") or {}
    return (rule.get("name") == "asset-scan-queue"
            and queue.get("queueName") == "asset-scan"
            and str(queue.get("queueLength")) == "1")


def main():
    try:
        ok = verify(json.load(sys.stdin))
    except (ValueError, TypeError):
        ok = False
    if not ok:
        print("asset-scan-worker must retain only its asset-scan queue rule", file=sys.stderr)
    return 0 if ok else 1


if __name__ == "__main__":
    raise SystemExit(main())
