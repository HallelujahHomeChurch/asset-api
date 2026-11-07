"""Exercise actual automatic workflow filters without GitHub or cloud access."""
from pathlib import Path
import fnmatch
import re
import unittest

ROOT = Path(__file__).resolve().parents[2]


def events(text):
    block = re.search(r'^on:\n(.*?)(?=^\S)', text, re.M | re.S).group(1)
    result = []
    for match in re.finditer(r'^  (push|pull_request|pull_request_target):[^\n]*\n(.*?)(?=^  \w|\Z)', block, re.M | re.S):
        event, config = match.groups()
        filters = re.findall(r'^    (paths|paths-ignore):([^\n]*)\n((?:      - .*\n)*)', config, re.M)
        assert len(filters) <= 1, 'paths and paths-ignore cannot coexist on one event'
        if filters:
            key, inline, values = filters[0]
            if inline.strip():
                assert inline.strip().startswith('[') and inline.strip().endswith(']')
                patterns = [value.strip().strip("\"'") for value in inline.strip()[1:-1].split(',')]
            else:
                patterns = [line.strip()[2:].strip().strip("\"'") for line in values.splitlines()]
        else:
            key, patterns = 'paths-ignore', []
        result.append((event, key, patterns))
    return result


def matches(path, pattern):
    return fnmatch.fnmatchcase(path, pattern) or (pattern.startswith('**/') and fnmatch.fnmatchcase(path, pattern[3:]))


def starts(key, patterns, paths):
    if key == 'paths-ignore':
        return any(not any(matches(path, pattern) for pattern in patterns) for path in paths)
    for path in paths:
        included = False
        for pattern in patterns:
            negative = pattern.startswith('!')
            if matches(path, pattern[1:] if negative else pattern):
                included = not negative
        if included:
            return True
    return False


class DocumentationTriggerTests(unittest.TestCase):
    def test_markdown_never_starts_an_automatic_workflow(self):
        docs = ['README.md', 'AGENTS.md', 'SECURITY.md', 'docs/guide.md',
                'docs/contracts/review.md', 'docs/reviews/notes with spaces.md',
                'internal/handlers/AGENTS.md', 'infra/README.md']
        for workflow in (ROOT / '.github/workflows').glob('*.yml'):
            for event, key, patterns in events(workflow.read_text()):
                if workflow.name == 'ci.yml' and event == 'pull_request':
                    continue  # Required verify remains until the owner resolves the protection conflict.
                for path in docs:
                    with self.subTest(workflow=workflow.name, event=event, path=path):
                        self.assertFalse(starts(key, patterns, [path]))

    def test_source_configuration_and_mixed_changes_keep_ci(self):
        ci_events = events((ROOT / '.github/workflows/ci.yml').read_text())
        self.assertTrue(ci_events)
        for event, key, patterns in ci_events:
            for path in ['source.go', 'src/app.tsx', '.github/workflows/ci.yml',
                         'infra/runtime-env.json', 'docs/data-governance.yaml',
                         'internal/testdata/input.json']:
                with self.subTest(event=event, path=path):
                    self.assertTrue(starts(key, patterns, [path]))
                    self.assertTrue(starts(key, patterns, ['README.md', path]))


if __name__ == '__main__':
    unittest.main()
