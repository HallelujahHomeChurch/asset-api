#!/usr/bin/env python3
"""Route manifest-only changes without application verification."""
import importlib.util
import re
from pathlib import Path
import subprocess
import sys

ROOT = Path(__file__).resolve().parents[2]
spec = importlib.util.spec_from_file_location('docs_classifier', Path(__file__).with_name('ci-agent-docs.py'))
docs = importlib.util.module_from_spec(spec)
spec.loader.exec_module(docs)

sys.path.insert(0, str(ROOT / 'scripts'))
import runtime_env


def release_paths():
    workflow = ROOT / '.github/workflows/release.yml'
    if not workflow.exists():
        # Account release is explicitly dispatched; source changes retain that gate.
        return None
    text = workflow.read_text()
    block = text.split('    paths:\n', 1)[1].split('  workflow_dispatch:', 1)[0]
    patterns = re.findall(r"^      - ['\"]?([^'\"\n]+)['\"]?$", block, re.M)
    if not patterns:
        raise ValueError('Missing release path policy')
    return patterns


def release_matches(path, patterns):
    selected = False
    for pattern in patterns:
        negative = pattern.startswith('!')
        glob = pattern[1:] if negative else pattern
        expression = re.escape(glob).replace(r'\*\*/', '(?:.*/)?').replace(r'\*\*', '.*').replace(r'\*', '[^/]*').replace(r'\?', '[^/]')
        if re.fullmatch(expression, path):
            selected = not negative
    return selected


def deployment_only(event, base, head):
    if event not in ('push', 'pull_request') or not all(re.fullmatch('[0-9a-f]{40}', sha) and set(sha) != {'0'} for sha in (base, head)):
        return False
    if docs.git('rev-parse', 'HEAD').decode().strip() != head:
        return False
    docs.git('merge-base', '--is-ancestor', base, head)
    records = docs.git('diff', '--raw', '--no-abbrev', '-z', '--no-renames', '--no-ext-diff', '--no-textconv', base, head, '--').split(b'\0')
    if records.pop() != b'' or not records or len(records) % 2:
        return False
    manifest = False
    for index in range(0, len(records), 2):
        match = re.fullmatch(rb':(\d{6}) (\d{6}) [0-9a-f]{40} [0-9a-f]{40} ([AMD])', records[index])
        if not match:
            return False
        old, new, status = (part.decode() for part in match.groups())
        regular = {'100644', '100755'}
        if not ((status == 'A' and old == '000000' and new in regular) or (status == 'M' and old in regular and new in regular) or (status == 'D' and old in regular and new == '000000')):
            return False
        path = records[index + 1].decode('utf-8')
        if path == 'infra/runtime-env.json':
            if (old, new, status) != ('100644', '100644', 'M'):
                return False
            manifest = True
        elif not docs.document(path):
            return False
    return manifest


def config_only(event, base, head, deploy=False):
    original_document = docs.document
    if deploy:
        patterns = release_paths()
        def eligible(path):
            if path == 'infra/runtime-env.json':
                return True
            if patterns is None:
                return original_document(path) or path.startswith('scripts/test-') or path.endswith('_test.go')
            return not release_matches(path, patterns)
        docs.document = eligible
    else:
        docs.document = lambda path: path == 'infra/runtime-env.json'
    try:
        return deployment_only(event, base, head) if deploy else docs.docs_only(event, base, head)
    finally:
        docs.document = original_document


def validate_transition(event, base, head):
    if event not in ('push', 'pull_request') or not all(re.fullmatch('[0-9a-f]{40}', sha) and set(sha) != {'0'} for sha in (base, head)):
        return
    changed = subprocess.check_output(['git', 'diff', '--name-only', base, head, '--', 'infra/runtime-env.json'], text=True)
    if not changed:
        return
    def read(sha):
        result = subprocess.run(['git', 'show', sha + ':infra/runtime-env.json'], capture_output=True, text=True)
        if result.returncode:
            if sha == base:
                return {}
            raise ValueError('Runtime manifest must remain present')
        text = result.stdout
        return runtime_env.parse(text)
    before, after = read(base), read(head)
    if any(not set(owned) <= set(after[target]) for target, owned in before.items()):
        raise ValueError('Use null tombstones; do not delete owned keys')


if __name__ == '__main__':
    transition_args = sys.argv[2:] if sys.argv[1:2] == ['deploy'] else sys.argv[1:]
    if len(transition_args) == 3:
        try:
            validate_transition(*transition_args)
        except (ValueError, KeyError, TypeError, OSError, subprocess.SubprocessError):
            sys.exit('Invalid runtime configuration transition; values withheld')
    try:
        args = sys.argv[1:]
        deploy = args[:1] == ['deploy']
        if deploy:
            args = args[1:]
        result = len(args) == 3 and config_only(*args, deploy=deploy)
    except (OSError, ValueError, KeyError, IndexError, TypeError, UnicodeError, subprocess.SubprocessError):
        result = False
    print('config_only=' + str(result).lower())
