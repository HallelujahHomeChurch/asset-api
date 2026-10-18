#!/usr/bin/env bash
set -euo pipefail
# Fixed three-image handoff; Python parses provenance and archives without executing their contents.
python3 - "$@" <<'PY'
import gzip, hashlib, json, os, pathlib, re, shutil, subprocess, sys, tarfile

ROLES = ('api', 'scan', 'recording')
FILES = {'IMAGE_PROVENANCE.json', 'images.tar.gz', 'SHA256SUMS'}
DIGEST = re.compile(r'sha256:[0-9a-f]{64}\Z')

def require(condition, message):
    if not condition:
        raise ValueError(message)

def unique_object(pairs):
    result = {}
    for key, value in pairs:
        require(key not in result, 'duplicate JSON key')
        result[key] = value
    return result

def read_json(data):
    return json.loads(data, object_pairs_hook=unique_object)

def sha256(path):
    with open(path, 'rb') as stream:
        digest = hashlib.sha256()
        for chunk in iter(lambda: stream.read(1024 * 1024), b''):
            digest.update(chunk)
        return digest.hexdigest()

def inspect(ref):
    return read_json(subprocess.check_output(['docker', 'image', 'inspect', ref]))[0]

def archive_configs(path, images):
    with tarfile.open(path, 'r:gz') as archive:
        members = {}
        for member in archive.getmembers():
            parts = pathlib.PurePosixPath(member.name).parts
            require(not member.name.startswith('/') and '..' not in parts, 'unsafe archive path')
            require(member.name not in members, 'duplicate archive member')
            require(member.isfile() or member.isdir(), 'unsupported archive member')
            members[member.name] = member

        def data(name):
            member = members.get(name)
            require(member is not None and member.isfile(), 'missing archive metadata')
            require(member.size <= 1024 * 1024, 'oversized archive metadata')
            return archive.extractfile(member).read()

        manifest = read_json(data('manifest.json'))
        require(isinstance(manifest, list) and len(manifest) == 3, 'archive must contain three images')
        require({tag for entry in manifest for tag in entry['RepoTags']} == {image['ref'] for image in images}, 'unexpected archive image tags')
        configs = []
        for image in images:
            matches = [entry for entry in manifest if image['ref'] in entry['RepoTags']]
            require(len(matches) == 1, 'missing or duplicate archive image')
            entry = matches[0]
            config_bytes = data(entry['Config'])
            config_id = 'sha256:' + hashlib.sha256(config_bytes).hexdigest()
            config = read_json(config_bytes)
            require(config.get('os') == 'linux' and config.get('architecture') == 'amd64', 'archive platform must be linux/amd64')
            require(all(layer in members and members[layer].isfile() for layer in entry['Layers']), 'missing archive layer')

            def reaches_config(digest, depth=0):
                require(isinstance(digest, str) and DIGEST.fullmatch(digest), 'invalid archive image digest')
                if digest == config_id:
                    return True
                require(depth < 4, 'invalid archive image graph')
                blob = data('blobs/sha256/' + digest[7:])
                require('sha256:' + hashlib.sha256(blob).hexdigest() == digest, 'archive image digest mismatch')
                node = read_json(blob)
                if 'config' in node:
                    return node['config']['digest'] == config_id
                return any(reaches_config(child['digest'], depth + 1) for child in node.get('manifests', []))

            # Classic Docker IDs are config hashes; containerd IDs can be OCI index/manifest hashes.
            require(reaches_config(image['image_id']), 'archive does not contain verified image identity')
            configs.append(config_id)
        return configs

try:
    require(len(sys.argv) >= 4, 'usage: pack DIR COMMIT API SCAN RECORDING | load DIR COMMIT')
    mode, directory, commit = sys.argv[1:4]
    directory = pathlib.Path(directory)
    require(re.fullmatch('[0-9a-f]{40}', commit), 'expected full commit SHA')
    require((mode == 'pack' and len(sys.argv) == 7) or (mode == 'load' and len(sys.argv) == 4), 'invalid handoff arguments')
    if mode == 'pack':
        directory.mkdir()
        refs = sys.argv[4:]
        require(len(set(refs)) == 3, 'expected three distinct image refs')
        images = []
        for role, ref in zip(ROLES, refs):
            image = inspect(ref)
            require(image['Os'] == 'linux' and image['Architecture'] == 'amd64', 'producer platform must be linux/amd64')
            require(DIGEST.fullmatch(image['Id']), 'invalid producer image ID')
            images.append(dict(role=role, ref=ref, image_id=image['Id'], platform='linux/amd64'))
        with open(directory / 'images.tar.gz', 'wb') as output:
            process = subprocess.Popen(['docker', 'save', *refs], stdout=subprocess.PIPE)
            with gzip.GzipFile(fileobj=output, mode='wb', compresslevel=1, mtime=0) as compressed:
                shutil.copyfileobj(process.stdout, compressed)
            process.stdout.close()
            require(process.wait() == 0, 'docker save failed')
        for image, config_id in zip(images, archive_configs(directory / 'images.tar.gz', images)):
            image['config_id'] = config_id
        (directory / 'IMAGE_PROVENANCE.json').write_text(json.dumps(dict(commit=commit, images=images), indent=2) + '\n')
        (directory / 'SHA256SUMS').write_text(''.join(sha256(directory / name) + '  ' + name + '\n'
            for name in ('IMAGE_PROVENANCE.json', 'images.tar.gz')))
    else:
        require({p.name for p in directory.iterdir()} == FILES, 'unexpected artifact file set')
        require(all(p.is_file() and not p.is_symlink() for p in directory.iterdir()), 'artifact files must be regular files')
        expected_sums = ''.join(sha256(directory / name) + '  ' + name + '\n' for name in ('IMAGE_PROVENANCE.json', 'images.tar.gz'))
        require((directory / 'SHA256SUMS').read_text() == expected_sums, 'artifact checksum or manifest mismatch')
        require((directory / 'IMAGE_PROVENANCE.json').stat().st_size < 16384, 'oversized provenance')
        provenance = read_json((directory / 'IMAGE_PROVENANCE.json').read_text())
        require(set(provenance) == {'commit', 'images'} and provenance['commit'] == commit, 'artifact commit mismatch')
        images = provenance['images']
        require(isinstance(images, list) and len(images) == 3, 'expected three image records')
        for role, image in zip(ROLES, images):
            require(set(image) == {'role', 'ref', 'image_id', 'config_id', 'platform'}, 'unexpected image record fields')
            require(image['role'] == role and image['platform'] == 'linux/amd64', 'image role or platform mismatch')
            require(isinstance(image['ref'], str) and re.fullmatch(r'[a-z0-9][a-z0-9./:_-]*', image['ref']), 'invalid image ref')
            require(DIGEST.fullmatch(image['image_id']) and DIGEST.fullmatch(image['config_id']), 'invalid image identity')
        require(len({image['ref'] for image in images}) == 3, 'duplicate image refs')
        require(archive_configs(directory / 'images.tar.gz', images) == [image['config_id'] for image in images], 'archive config identity mismatch')
        subprocess.run(['docker', 'load', '--input', str(directory / 'images.tar.gz')], check=True)
        for image in images:
            loaded = inspect(image['ref'])
            require(loaded['Id'] == image['image_id'] and loaded['Os'] == 'linux' and loaded['Architecture'] == 'amd64', 'loaded image identity or platform mismatch')
        with open(os.environ['GITHUB_OUTPUT'], 'a') as output:
            for image in images:
                output.write(image['role'] + '_image_id=' + image['image_id'] + '\n')
except (ValueError, KeyError, TypeError, OSError, tarfile.TarError, subprocess.SubprocessError) as error:
    sys.exit('Image artifact rejected: ' + str(error))
PY
