#!/usr/bin/env python3
"""Run every Go package once, overlapping media work with the serial DB lane."""
import json
import os
from pathlib import Path
import signal
import subprocess
import sys
import tempfile
import time
from urllib.parse import parse_qs, unquote, urlsplit

MEDIA = frozenset({
    "hhc/asset-api/internal/recordingvalidation",
    "hhc/asset-api/internal/recordingprocessing",
    "hhc/asset-api/internal/derivatives",
})


def partition(packages):
    if len(set(packages)) != len(packages) or not MEDIA.issubset(packages):
        raise ValueError("duplicate packages or missing expected media package")
    # New packages default to the serial lane; no path-based test exclusions.
    return [[p for p in packages if p not in MEDIA],
            [p for p in packages if p in MEDIA]]


def lane_environments():
    serial = dict(os.environ)
    media = dict(serial)
    urls = [serial.get("TEST_DATABASE_URL", ""), serial.get("TEST_MEDIA_DATABASE_URL", "")]
    names = []
    for url in urls:
        parsed = urlsplit(url)
        name = unquote(parsed.path.removeprefix("/"))
        if (parsed.scheme not in ("postgres", "postgresql")
                or parsed.hostname not in ("localhost", "127.0.0.1", "::1")
                or not name or "/" in name
                or {key.lower() for key in parse_qs(parsed.query, keep_blank_values=True)} & {"dbname", "database", "host", "hostaddr", "port", "service"}):
            raise ValueError("two explicit loopback PostgreSQL database URLs are required")
        names.append(name)
    if names[0] == names[1]:
        raise ValueError("test lanes require different databases; schemas do not isolate advisory locks")
    media["TEST_DATABASE_URL"] = urls[1]
    return [serial, media]


def verify_events(path, packages):
    terminal = []
    with path.open() as source:
        for line in source:
            event = json.loads(line)
            if event.get("Action") == "fail":
                raise ValueError("failed Go test event")
            if not event.get("Test") and event.get("Action") in ("pass", "skip"):
                terminal.append(event["Package"])
    if sorted(terminal) != sorted(packages):
        raise ValueError("incomplete or duplicate package results")


def run(output):
    # Never leave a previous successful manifest usable after a failed run.
    output.unlink(missing_ok=True)
    environments = lane_environments()
    packages = subprocess.check_output(["go", "list", "./..."], text=True).splitlines()
    lanes = partition(packages)
    print(f"Go test lanes: {len(lanes[0])} serial + {len(lanes[1])} media packages",
          file=sys.stderr, flush=True)
    output.parent.mkdir(parents=True, exist_ok=True)
    processes = []
    with tempfile.TemporaryDirectory(prefix="go-lanes-", dir=output.parent) as directory:
        paths = [Path(directory) / f"{i}.jsonl" for i in range(len(lanes))]
        files = [path.open("w") for path in paths]
        try:
            for lane, stream, environment in zip(lanes, files, environments):
                processes.append(subprocess.Popen(
                    ["go", "test", "-race", "-json", *lane, "-count=1", "-p=1"],
                    stdout=stream, start_new_session=True, env=environment))
            while True:
                statuses = [p.poll() for p in processes]
                if any(code not in (None, 0) for code in statuses):
                    raise RuntimeError("Go test lane failed")
                if all(code == 0 for code in statuses):
                    break
                time.sleep(0.05)
            for stream in files:
                stream.close()
            for path, lane in zip(paths, lanes):
                verify_events(path, lane)
            # Separate files prevent concurrent writers corrupting JSON records.
            merged = Path(directory) / "merged.jsonl"
            with merged.open("w") as destination:
                for path in paths:
                    destination.write(path.read_text())
            merged.replace(output)
        finally:
            for process in processes:
                if process.poll() is None:
                    try:
                        os.killpg(process.pid, signal.SIGKILL)
                    except ProcessLookupError:
                        pass
                process.wait()
            for stream in files:
                stream.close()
            for path in paths:
                sys.stdout.write(path.read_text())


if __name__ == "__main__":
    signal.signal(signal.SIGTERM, lambda *_: sys.exit(143))
    run(Path(".artifacts/data-governance/test-events.jsonl"))
