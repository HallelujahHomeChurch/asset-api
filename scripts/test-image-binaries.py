#!/usr/bin/env python3
import os
from pathlib import Path
import subprocess
import tempfile

FAKE = "#!/usr/bin/env python3\nimport os,sys,pathlib,tarfile,io\nargs=sys.argv[1:]; mode=os.environ['CASE']\nwith open(os.environ['CALLS'],'a') as f: f.write(args[0]+'\\n')\nif args[:2]==['image','inspect']:\n print(('linux/arm64' if mode=='platform' else 'linux/amd64') if 'Architecture' in args[3] else 'sha256:'+'a'*64)\nelif args[0]=='create': print('fixture-container')\nelif args[0]=='rm': pass\nelif args[0]=='cp':\n if mode=='copyfail': sys.exit(42)\n p=pathlib.Path(args[2]); p.write_text('binary'); p.chmod(0o644 if mode=='nonexec' else 0o755)\nelif args[0]=='export':\n if mode=='exportfail': sys.exit(42)\n with tarfile.open(fileobj=sys.stdout.buffer, mode='w|') as t:\n  for name in ['asset-derivative-worker','asset-scan-warmer']:\n   if mode==name: continue\n   info=tarfile.TarInfo(name); t.addfile(info)\nelse: sys.exit(99)\n"
for case in ['ok', 'asset-derivative-worker', 'asset-scan-warmer', 'exportfail']:
    with tempfile.TemporaryDirectory() as tmp:
        root = Path(tmp)
        docker = root / 'docker'
        docker.write_text(FAKE)
        docker.chmod(0o755)
        env = dict(os.environ, PATH=tmp+os.pathsep+os.environ['PATH'], CASE=case, CALLS=str(root/'calls'))
        result = subprocess.run(['bash', 'scripts/test-image-binaries.sh', 'fixture:image', tmp], env=env, capture_output=True)
        assert (result.returncode == 0) == (case == 'ok'), (case, result.stderr)
        calls = (root/'calls').read_text().splitlines()
        assert calls.count('create') == (0 if case == 'platform' else 1), calls
        assert calls.count('rm') == (0 if case == 'platform' else 1), calls
        assert calls.count('export') == 1, calls
print('image artifact fixtures passed')
