#!/usr/bin/env bash
set -euo pipefail
# Three tiny real images exercise publication trust without archiving runtime images in PRs.
python3 - "$@" <<'PY'
import hashlib, io, json, os, pathlib, shutil, subprocess, sys, tarfile, tempfile, time, uuid

helper = pathlib.Path('scripts/release-image-artifact.sh').resolve()
assert helper.is_file(), 'release image handoff helper is required'
commit = os.environ.get('GITHUB_SHA', 'a' * 40)
roles = ('api', 'scan', 'recording')
started = time.monotonic()
with tempfile.TemporaryDirectory() as temp:
    root = pathlib.Path(temp)
    refs = ['hhc-artifact-test-' + uuid.uuid4().hex + ':' + role for role in roles]
    ids = []
    (root / 'payload').write_text('artifact transport fixture\n')
    real_docker = shutil.which('docker')
    try:
        for role, ref in zip(roles, refs):
            (root / 'Dockerfile').write_text(f'FROM scratch\nCOPY payload /payload\nLABEL fixture="{ref}"\n')
            subprocess.run([real_docker, 'build', '--platform', 'linux/amd64', '-t', ref, str(root)], check=True, stdout=subprocess.DEVNULL)
            ids.append(subprocess.check_output([real_docker, 'image', 'inspect', '--format', '{{.Id}}', ref], text=True).strip())
        good = root / 'good'
        subprocess.run(['bash', str(helper), 'pack', str(good), commit, *refs], check=True)
        # A missing archive image must fail even with all producer images already present locally.
        watched = root / 'bin'
        watched.mkdir()
        calls = root / 'docker-calls'
        (watched / 'docker').write_text("#!/usr/bin/env python3\nimport json, os, subprocess, sys\nargs = sys.argv[1:]\nwith open(os.environ['DOCKER_CALLS'], 'a') as stream: stream.write(' '.join(args) + '\\n')\nif args[:2] == ['image', 'inspect'] and os.environ.get('LOADED_IDS'):\n    details = json.loads(subprocess.check_output([os.environ['REAL_DOCKER'], *args]))\n    details[0]['Id'] = json.loads(os.environ['LOADED_IDS'])[args[2]]\n    details[0]['Architecture'] = os.environ.get('LOADED_ARCH', details[0]['Architecture'])\n    print(json.dumps(details))\nelse:\n    sys.exit(subprocess.call([os.environ['REAL_DOCKER'], *args]))\n")
        (watched / 'docker').chmod(0o755)
        env = dict(os.environ, PATH=str(watched) + os.pathsep + os.environ['PATH'], DOCKER_CALLS=str(calls), REAL_DOCKER=real_docker)
        output = root / 'outputs'
        env['GITHUB_OUTPUT'] = str(output)

        def checksums(directory):
            (directory / 'SHA256SUMS').write_text(''.join(hashlib.sha256((directory / name).read_bytes()).hexdigest() + '  ' + name + '\n'
                for name in ('IMAGE_PROVENANCE.json', 'images.tar.gz')))

        def rejected(name, mutate, expected_commit=commit, rehash=True):
            directory = root / name
            shutil.copytree(good, directory)
            mutate(directory)
            if rehash:
                checksums(directory)
            calls.write_text('')
            output.write_text('')
            result = subprocess.run(['bash', str(helper), 'load', str(directory), expected_commit], env=env, capture_output=True, text=True)
            assert result.returncode != 0, name + ' must fail closed'
            assert not output.read_text(), name + ' exposed publication outputs'
            assert not calls.read_text(), name + ' reached Docker before validation'
            print('PASS reject ' + name)

        def metadata(directory, change):
            path = directory / 'IMAGE_PROVENANCE.json'
            value = json.loads(path.read_text())
            change(value)
            path.write_text(json.dumps(value))

        rejected('wrong-commit', lambda p: None, 'b' * 40)
        rejected('corrupt-archive', lambda p: (p / 'images.tar.gz').write_bytes(b'corrupt'), rehash=False)
        rejected('corrupt-checksum', lambda p: (p / 'SHA256SUMS').write_text('0' * 64 + '  images.tar.gz\n'), rehash=False)
        rejected('missing-file', lambda p: (p / 'images.tar.gz').unlink(), rehash=False)
        rejected('extra-file', lambda p: (p / 'unexpected').write_text('unexpected'))
        rejected('symlink-file', lambda p: ((p / 'images.tar.gz').unlink(), (p / 'images.tar.gz').symlink_to(good / 'images.tar.gz')))
        rejected('missing-role', lambda p: metadata(p, lambda v: v['images'].pop()))
        rejected('duplicate-role', lambda p: metadata(p, lambda v: v['images'].__setitem__(1, v['images'][0])))
        rejected('wrong-image-id', lambda p: metadata(p, lambda v: v['images'][0].update(image_id='sha256:' + '0' * 64)))
        rejected('wrong-platform', lambda p: metadata(p, lambda v: v['images'][0].update(platform='linux/arm64')))
        rejected('wrong-config-id', lambda p: metadata(p, lambda v: v['images'][0].update(config_id='sha256:' + '0' * 64)))
        rejected('duplicate-json-key', lambda p: (p / 'IMAGE_PROVENANCE.json').write_text('{"commit":"' + commit + '",' + (p / 'IMAGE_PROVENANCE.json').read_text()[1:]))

        def rewrite_archive(directory, change):
            path = directory / 'images.tar.gz'
            entries = []
            with tarfile.open(path, 'r:gz') as archive:
                for member in archive.getmembers():
                    entries.append((member, archive.extractfile(member).read() if member.isfile() else None))
            entries = change(entries)
            with tarfile.open(path, 'w:gz') as archive:
                for member, data in entries:
                    archive.addfile(member, io.BytesIO(data) if data is not None else None)

        def omit_image(entries):
            result = []
            for member, data in entries:
                if member.name == 'manifest.json':
                    manifest = json.loads(data)
                    assert len(manifest) == 3
                    data = json.dumps(manifest[:2]).encode()
                    member.size = len(data)
                result.append((member, data))
            return result
        rejected('archive-missing-image-with-stale-local-image', lambda p: rewrite_archive(p, omit_image))
        rejected('archive-missing-config', lambda p: rewrite_archive(p, lambda entries: [
            (m, d) for m, d in entries if m.name != json.loads(dict((x.name, y) for x, y in entries)['manifest.json'])[0]['Config']]))
        rejected('archive-missing-layer', lambda p: rewrite_archive(p, lambda entries: [
            (m, d) for m, d in entries if m.name != json.loads(dict((x.name, y) for x, y in entries)['manifest.json'])[0]['Layers'][0]]))
        rejected('malformed-provenance', lambda p: (p / 'IMAGE_PROVENANCE.json').write_text('{'))
        rejected('archive-duplicate-member', lambda p: rewrite_archive(p, lambda entries: entries + [next(e for e in entries if e[0].name == 'manifest.json')]))
        # Remove only this test's uniquely labelled images: a load must actually restore them.
        subprocess.run([real_docker, 'image', 'rm', *refs], check=True, stdout=subprocess.DEVNULL)
        calls.write_text('')
        output.write_text('')
        subprocess.run(['bash', str(helper), 'load', str(good), commit], env=env, check=True)
        actual = dict(line.split('=', 1) for line in output.read_text().splitlines())
        records = json.loads((good / 'IMAGE_PROVENANCE.json').read_text())['images']
        expected = dict(zip((role + '_image_id' for role in roles), ids))
        expected.update({record['role'] + '_config_id': record['config_id'] for record in records})
        assert actual == expected, (actual, expected)
        for image_id in ids:
            assert subprocess.check_output([real_docker, 'image', 'inspect', '--format', '{{.Os}}/{{.Architecture}}', image_id], text=True).strip() == 'linux/amd64'
        print('PASS cold Docker round-trip and exact producer identities')
        records = json.loads((good / 'IMAGE_PROVENANCE.json').read_text())['images']
        config_ids = {record['ref']: record['config_id'] for record in records}
        classic = dict(env, LOADED_IDS=json.dumps(config_ids))
        output.write_text('')
        subprocess.run(['bash', str(helper), 'load', str(good), commit], env=classic, check=True)
        assert dict(line.split('=', 1) for line in output.read_text().splitlines()) == {record['role'] + suffix: record['config_id'] for record in records for suffix in ('_image_id', '_config_id')}
        wrong_ids = {**config_ids, refs[0]: 'sha256:' + '0' * 64}
        for override in [dict(LOADED_IDS=json.dumps(wrong_ids)), dict(LOADED_IDS=json.dumps(config_ids), LOADED_ARCH='arm64')]:
            output.write_text('')
            result = subprocess.run(['bash', str(helper), 'load', str(good), commit], env=dict(env, **override), capture_output=True)
            assert result.returncode != 0 and not output.read_text(), 'loaded identity/platform mismatch accepted'
        print('PASS OCI-to-classic identities and reject unrelated loaded identity/platform')
        output.write_text(''.join(key + '=' + value + '\n' for key, value in expected.items()))
        if len(sys.argv) == 2:
            destination = pathlib.Path(sys.argv[1])
            shutil.copytree(good, destination)
            with open(os.environ['GITHUB_OUTPUT'], 'a') as stream:
                stream.write(output.read_text())
        print(f'Artifact bytes={sum(p.stat().st_size for p in good.iterdir())}; test seconds={time.monotonic()-started:.2f}')
    finally:
        subprocess.run([real_docker, 'image', 'rm', *refs], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
PY
