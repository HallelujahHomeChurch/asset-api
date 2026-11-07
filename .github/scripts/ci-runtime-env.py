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
        expression = re.escape(glob).replace(r'\*\*', '.*').replace(r'\*', '[^/]*').replace(r'\?', '[^/]')
        if re.fullmatch(expression, path):
            selected = not negative
    return selected


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
        return docs.docs_only(event, base, head)
    finally:
        docs.document = original_document


if __name__ == '__main__':
    try:
        args = sys.argv[1:]
        deploy = args[:1] == ['deploy']
        if deploy:
            args = args[1:]
        result = len(args) == 3 and config_only(*args, deploy=deploy)
    except (OSError, ValueError, KeyError, IndexError, TypeError, UnicodeError, subprocess.SubprocessError):
        result = False
    print('config_only=' + str(result).lower())
