#!/usr/bin/env python3
"""Prove inert documentation changes; uncertainty retains product verification."""
import re
import subprocess
import sys


def git(*args):
    return subprocess.check_output(['git', '--literal-pathspecs', *args], stderr=subprocess.DEVNULL)


def document(path):
    return path.endswith('.md') and not any(ord(char) < 32 or ord(char) == 127 for char in path)


def unrelated_to_broadcast(path):
    # Only executable entry points with no recording responsibilities.
    return document(path) or bool(re.fullmatch(
        r'cmd/(scan-worker|retention-worker|signature-refresh)/[A-Za-z0-9_]+\.go', path))


def proven_paths(event, base, head, predicate):
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
        if not predicate(records[i + 1].decode('utf-8')):
            return False
    return True


def docs_only(event, base, head):
    return proven_paths(event, base, head, document)


if __name__ == '__main__':
    broadcast = sys.argv[1:2] == ['--broadcast']
    args = sys.argv[2:] if broadcast else sys.argv[1:]
    try:
        result = len(args) == 3 and proven_paths(*args, unrelated_to_broadcast if broadcast else document)
        if broadcast:
            result = result and args[0] == 'pull_request'
    except (OSError, UnicodeError, subprocess.SubprocessError):
        result = False
    print(('broadcast_unrelated=' if broadcast else 'docs_only=') + str(result).lower())
