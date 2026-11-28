"""Render timings from the completed Go lanes without running tests again."""
import json
import os
from pathlib import Path

packages = []
tests = []
print("### Go verification")
print(f"Commit `{os.environ['GITHUB_SHA']}`")
print(f"\nRaw fixture cache hit: {os.environ.get('FIXTURE_CACHE_HIT') or 'false'}")
print(f"\nFixture key: `{os.environ.get('FIXTURE_KEY', '')}`\n")
with Path(".artifacts/data-governance/test-events.jsonl").open() as events:
    for line in events:
        event = json.loads(line)
        if event["Action"] == "pass":
            name = event.get("Test")
            bucket = tests if name else packages
            bucket.append((event["Elapsed"], name or event["Package"]))
        if event.get("Test") == "TestBroadcastRealDecodedRangeAndDerivatives":
            output = event.get("Output", "")
            if "broadcast timing:" in output:
                print("- " + output.split("broadcast timing:", 1)[1].strip())

for label, results in (("packages", packages), ("tests", tests)):
    print(f"\nSlowest {label}\n")
    for elapsed, name in sorted(results, reverse=True)[:5]:
        print(f"- `{name}` {elapsed:.1f}s")
