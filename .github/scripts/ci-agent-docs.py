#!/usr/bin/env python3
"""Prove inert documentation changes; uncertainty retains product verification."""
import re
import subprocess
import sys


def git(*args):
    return subprocess.check_output(['git', '--literal-pathspecs', *args], stderr=subprocess.DEVNULL)


def document(path):
    if path in {'AGENTS.md', 'README.md', '.github/pull_request_template.md'}:
        return True
    if re.fullmatch(r'\.agents/skills/hhc-delivery/(?:[A-Za-z0-9_-]+/)*[A-Za-z0-9_.-]+\.md', path):
        return True
    return (re.fullmatch(r'docs/(?:[A-Za-z0-9_-]+/)*[A-Za-z0-9_.-]+\.md', path) is not None
            and not re.search(r'(?:^|[/_.-])(?:generated|fixtures?|testdata|contracts?|schemas?|snapshots?|tests?)(?:[/_.-]|$)', path, re.I)
            and not path.startswith('docs/obs-capture/'))


def docs_only(event, base, head):
    if event not in {'pull_request', 'push'} or not all(re.fullmatch('[0-9a-f]{40}', x) and set(x) != {'0'} for x in (base, head)):
        return False
    if git('rev-parse', 'HEAD').decode().strip() != head:
        return False
    git('merge-base', '--is-ancestor', base, head)
    records = git('diff', '--raw', '--no-abbrev', '-z', '--no-renames', '--no-ext-diff', '--no-textconv', base, head, '--').split(b'\0')
    if records.pop() != b'' or not records or len(records) % 2:
        return False
    modes = {'A': ('000000', '100644'), 'M': ('100644', '100644'), 'D': ('100644', '000000')}
    for i in range(0, len(records), 2):
        meta = re.fullmatch(rb':(\d{6}) (\d{6}) [0-9a-f]{40} [0-9a-f]{40} ([AMD])', records[i])
        if not meta or tuple(x.decode() for x in meta.group(1, 2)) != modes[meta.group(3).decode()]:
            return False
        if not document(records[i + 1].decode('utf-8')):
            return False
    return True


if __name__ == '__main__':
    try:
        result = len(sys.argv) == 4 and docs_only(*sys.argv[1:])
    except (OSError, UnicodeError, subprocess.SubprocessError):
        result = False
    print('docs_only=' + str(result).lower())
