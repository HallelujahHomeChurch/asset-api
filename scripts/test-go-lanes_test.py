#!/usr/bin/env python3
import importlib.util
import json
import os
from pathlib import Path
import subprocess
import tempfile
import time
import unittest

SCRIPT = Path(__file__).with_name("test-go-lanes.py")
spec = importlib.util.spec_from_file_location("lanes", SCRIPT)
lanes = importlib.util.module_from_spec(spec)
spec.loader.exec_module(lanes)


class GoLanesTest(unittest.TestCase):
    def test_all_packages_exactly_once_including_new_packages(self):
        packages = [*sorted(lanes.MEDIA), "hhc/asset-api/new", "hhc/asset-api/internal/postgres"]
        serial, media = lanes.partition(packages)
        self.assertEqual(sorted(serial + media), sorted(packages))
        self.assertFalse(set(serial) & set(media))
        self.assertIn("hhc/asset-api/new", serial)
        for invalid in (packages + [packages[0]], packages[1:]):
            with self.assertRaises(ValueError):
                lanes.partition(invalid)

    def test_requires_one_successful_terminal_event_per_package(self):
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "events"
            valid = [{"Action": "pass", "Package": "a", "Test": "TestA"},
                     {"Action": "pass", "Package": "a"},
                     {"Action": "skip", "Package": "b"}]
            def write(events):
                path.write_text("".join(json.dumps(e) + "\n" for e in events))
            write(valid)
            lanes.verify_events(path, ["a", "b"])
            for events in (valid[:-1], valid + [valid[-1]],
                           valid + [{"Action": "fail", "Package": "a", "Test": "TestA"}]):
                write(events)
                with self.assertRaises(ValueError):
                    lanes.verify_events(path, ["a", "b"])
            path.write_text("invalid json\n")
            with self.assertRaises(ValueError):
                lanes.verify_events(path, ["a", "b"])

    def test_real_subprocess_merge_failure_and_cancellation(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            go = root / "go"
            go.write_text('''#!/usr/bin/env python3
import json, os, sys, time
packages = ["hhc/asset-api/internal/recordingvalidation", "hhc/asset-api/internal/recordingprocessing", "hhc/asset-api/internal/derivatives", "hhc/asset-api/new"]
if sys.argv[1] == "list":
 if os.environ.get("FAKE_MODE") == "listfail": sys.exit(1)
 print("\\n".join(packages)); sys.exit(0)
assert sys.argv[1:4] == ["test", "-race", "-json"]
assert sys.argv[-2:] == ["-count=1", "-p=1"]
with open(os.environ["PID_FILE"], "a") as f: f.write(str(os.getpid()) + "\\n")
mode = os.environ.get("FAKE_MODE", "pass")
if mode == "sleep" or (mode == "fail" and packages[0] in sys.argv):
 time.sleep(30)
if mode == "fail":
 deadline = time.monotonic() + 3
 while len(open(os.environ["PID_FILE"]).read().splitlines()) < 2:
  if time.monotonic() > deadline: sys.exit(2)
  time.sleep(0.01)
 sys.exit(1)
if mode == "incomplete": sys.exit(0)
for p in sys.argv[4:-2]:
 print(json.dumps({"Action":"pass", "Package":p, "Test":"TestSynthetic"}))
 print(json.dumps({"Action":"pass", "Package":p}))
''')
            go.chmod(0o755)
            env = dict(os.environ, PATH=str(root) + os.pathsep + os.environ["PATH"])
            output = root / ".artifacts/data-governance/test-events.jsonl"
            for mode in ("pass", "fail", "sleep", "incomplete", "listfail"):
                env["FAKE_MODE"] = mode
                pids = root / (mode + ".pids")
                env["PID_FILE"] = str(pids)
                process = subprocess.Popen(["python3", str(SCRIPT.resolve())], cwd=root,
                                           env=env, stdout=subprocess.DEVNULL, stderr=subprocess.PIPE)
                if mode == "sleep":
                    deadline = time.monotonic() + 3
                    while not pids.exists() or len(pids.read_text().splitlines()) < 2:
                        if time.monotonic() > deadline:
                            process.kill()
                            process.wait()
                            self.fail("both test lane processes did not start")
                        time.sleep(0.01)
                    process.terminate()
                _, stderr = process.communicate(timeout=5)
                if mode == "pass":
                    self.assertEqual(process.returncode, 0, stderr)
                    events = [json.loads(line) for line in output.read_text().splitlines()]
                    self.assertEqual(len(events), 8)
                    self.assertEqual(len({e["Package"] for e in events}), 4)
                else:
                    self.assertNotEqual(process.returncode, 0)
                    self.assertFalse(output.exists())
                if mode in ("fail", "sleep"):
                    self.assertEqual(len(pids.read_text().splitlines()), 2)
                if pids.exists():
                    for pid in pids.read_text().splitlines():
                        with self.assertRaises(ProcessLookupError):
                            os.kill(int(pid), 0)


if __name__ == "__main__":
    unittest.main()
