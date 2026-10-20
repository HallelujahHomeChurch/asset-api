"""Regression cases use disposable local Git repositories only."""
from pathlib import Path
import importlib.util
import subprocess
import tempfile
import unittest

SCRIPT = Path(__file__).with_name('ci-agent-docs.py')


class AgentDocsTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.repo = Path(self.temp.name)
        self.git('init', '-q')
        self.git('config', 'user.name', 'CI fixture')
        self.git('config', 'user.email', 'fixture@example.invalid')
        self.git('config', 'core.fileMode', 'true')
        self.put('AGENTS.md', 'instructions\n')
        self.put('source.go', 'package example\n')
        self.commit()
        self.base = self.git('rev-parse', 'HEAD')

    def tearDown(self):
        self.temp.cleanup()

    def git(self, *args):
        return subprocess.check_output(['git', *args], cwd=self.repo, text=True, stderr=subprocess.DEVNULL).strip()

    def put(self, path, content='human documentation\n'):
        dest = self.repo / path
        dest.parent.mkdir(parents=True, exist_ok=True)
        dest.write_text(content)

    def commit(self):
        self.git('add', '-A')
        self.git('-c', 'commit.gpgsign=false', 'commit', '-qm', 'fixture')

    def classify(self, event='pull_request', base=None, head=None):
        result = subprocess.run(['python3', str(SCRIPT), event, base or self.base, head or self.git('rev-parse', 'HEAD')], cwd=self.repo, text=True, capture_output=True)
        self.assertEqual(result.returncode, 0, result.stderr)
        return result.stdout.strip()

    def test_inert_entrypoints(self):
        for path in ['AGENTS.md', 'README.md', '.agents/skills/hhc-delivery/SKILL.md', '.github/pull_request_template.md', 'docs/guide.md']:
            self.put(path, 'updated instructions\n')
        self.commit()
        for event in ['pull_request', 'push']:
            self.assertEqual(self.classify(event), 'docs_only=true')

    def test_every_non_document_keeps_full_verification(self):
        for path in ['source.go', 'package.json', 'go.mod', '.github/workflows/ci.yml', '.github/scripts/ci-agent-docs.py', '.agents/skills/hhc-delivery/start.sh', 'docs/openapi.yaml', 'docs/fixtures/input.md', 'docs/contracts/api.md', 'docs/generated/api.md']:
            previous = self.git('rev-parse', 'HEAD')
            self.put('AGENTS.md', path)
            self.put(path, 'changed\n')
            self.commit()
            self.assertEqual(self.classify(base=previous), 'docs_only=false', path)

    def test_source_rename_is_not_document_only(self):
        self.git('mv', 'source.go', 'README.md')
        self.commit()
        self.assertEqual(self.classify(), 'docs_only=false')

    def test_executable_and_symlink_entrypoints_are_not_documents(self):
        (self.repo / 'AGENTS.md').chmod(0o755)
        self.commit()
        self.assertEqual(self.classify(), 'docs_only=false')
        (self.repo / 'AGENTS.md').unlink()
        (self.repo / 'AGENTS.md').symlink_to('source.go')
        self.commit()
        self.assertEqual(self.classify(), 'docs_only=false')

    def test_unknown_events_refs_and_empty_diffs_keep_full_verification(self):
        self.put('AGENTS.md', 'updated\n')
        self.commit()
        for event in ['workflow_dispatch', 'workflow_call', 'pull_request_target', 'merge_group']:
            self.assertEqual(self.classify(event), 'docs_only=false')
        self.assertEqual(self.classify(base='--help'), 'docs_only=false')
        self.assertEqual(self.classify(head=self.base), 'docs_only=false')
        self.assertEqual(self.classify(base=self.git('rev-parse', 'HEAD')), 'docs_only=false')


if __name__ == '__main__':
    unittest.main()
