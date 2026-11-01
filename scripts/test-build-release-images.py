"""Test the build supervisor with owned fake Docker processes, never a daemon."""
import json
import os
from pathlib import Path
import signal
import subprocess
import sys
import tempfile
import time
import unittest

HELPER = Path('scripts/build-release-images.py').resolve()
FAKE = '''#!/usr/bin/env python3
import json, os, pathlib, signal, subprocess, sys, time
root=pathlib.Path(os.environ['DOCKER_FIXTURE_ROOT'])
args=sys.argv[1:]
assert args[0]=='build' and args[-1]=='.'
role=args[args.index('-t')+1].removeprefix('asset-').removesuffix(':verify')
assert args[args.index('-f')+1]=={'api':'Dockerfile','scan':'Dockerfile.scan','recording':'Dockerfile.recording'}[role]
def event(kind,pid=None):
 fd=os.open(root/'events',os.O_WRONLY|os.O_APPEND|os.O_CREAT,0o600)
 os.write(fd,(json.dumps({'kind':kind,'role':role,'pid':pid or os.getpid(),'time':time.monotonic()})+'\\n').encode());os.close(fd)
(root/(role+'.pid')).write_text(str(os.getpid()))
event('start')
def stop(*_):
 event('stop');raise SystemExit(143)
signal.signal(signal.SIGTERM,stop)
if role in ('api','scan'):
 other='scan' if role=='api' else 'api'
 deadline=time.monotonic()+5
 while not (root/(other+'.pid')).exists():
  assert time.monotonic()<deadline,'first two builds did not overlap'
  time.sleep(.01)
mode=(root/'mode').read_text()
if mode=='failure' and role=='api':
 child=subprocess.Popen([sys.executable,'-c','import signal,time;signal.signal(signal.SIGTERM,signal.SIG_IGN);time.sleep(60)'])
 event('child',child.pid);time.sleep(.15);raise SystemExit(7)
time.sleep(60 if mode in ('failure','cancel') else {'api':.15,'scan':.35,'recording':.1}[role])
event('end')
'''


class BuildSupervisor(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.root = Path(self.temp.name)
        binary = self.root / 'docker'
        binary.write_text(FAKE)
        binary.chmod(0o755)
        self.env = {**os.environ, 'DOCKER_FIXTURE_ROOT': str(self.root), 'PATH': str(self.root)+os.pathsep+os.environ['PATH']}
        self.process = None

    def tearDown(self):
        if self.process and self.process.poll() is None:
            self.process.terminate()
            self.process.communicate(timeout=15)
        self.temp.cleanup()

    def start(self, mode):
        (self.root/'mode').write_text(mode)
        self.process = subprocess.Popen([sys.executable, str(HELPER)], env=self.env, stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True)

    def events(self):
        path=self.root/'events'
        return [json.loads(line) for line in path.read_text().splitlines()] if path.exists() else []

    def assert_children_stopped(self):
        pids={e['pid'] for e in self.events() if e['kind'] in ('start','child')}
        deadline=time.monotonic()+3
        while time.monotonic()<deadline:
            alive=set()
            for pid in pids:
                try:os.kill(pid,0);alive.add(pid)
                except ProcessLookupError:pass
            if not alive:return
            time.sleep(.05)
        self.fail(f'owned build processes still alive: {alive}')

    def test_all_three_roles_overlap_but_never_exceed_two(self):
        self.start('success')
        out,err=self.process.communicate(timeout=15)
        self.assertEqual(self.process.returncode,0,err)
        active=set();peak=0;ended=set()
        for e in self.events():
            if e['kind']=='start':active.add(e['role'])
            elif e['kind']=='end':active.remove(e['role']);ended.add(e['role'])
            peak=max(peak,len(active))
        self.assertEqual(peak,2)
        self.assertEqual(ended,{'api','scan','recording'})
        self.assertFalse(active)
        self.assertIn('image_builds_completed=3',out)
        self.assert_children_stopped()

    def test_first_failure_propagates_and_reaps_owned_siblings_and_descendants(self):
        self.start('failure')
        out,err=self.process.communicate(timeout=15)
        self.assertEqual(self.process.returncode,7,err)
        self.assertNotIn('image_builds_completed=3',out)
        self.assertNotIn('recording',{e['role'] for e in self.events()})
        self.assert_children_stopped()

    def test_cancellation_reaps_both_builds_and_never_reports_completion(self):
        self.start('cancel')
        deadline=time.monotonic()+5
        while len([e for e in self.events() if e['kind']=='start'])<2:
            self.assertLess(time.monotonic(),deadline)
            time.sleep(.02)
        self.process.send_signal(signal.SIGTERM)
        out,err=self.process.communicate(timeout=15)
        self.assertEqual(self.process.returncode,143,err)
        self.assertNotIn('image_builds_completed=3',out)
        self.assert_children_stopped()


if __name__=='__main__':unittest.main()
