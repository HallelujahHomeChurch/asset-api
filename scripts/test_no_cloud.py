"""Fail closed across Python subprocess boundaries in local unit tests."""
import atexit
import os
from pathlib import Path
import tempfile
_folder = tempfile.TemporaryDirectory(prefix='hhc-no-cloud-')
atexit.register(_folder.cleanup)
_root = Path(_folder.name)
(_root / 'config').mkdir()
(_root / 'az').write_text('#!/bin/sh\necho "Unexpected Azure invocation blocked by test harness" >&2\nexit 97\n')
(_root / 'az').chmod(0o755)
os.environ['PATH'] = str(_root) + os.pathsep + os.environ['PATH']
os.environ['AZ_BIN'] = str(_root / 'az')
os.environ['AZURE_CONFIG_DIR'] = str(_root / 'config')
