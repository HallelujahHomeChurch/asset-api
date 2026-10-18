#!/usr/bin/env python3
import json
import os
from pathlib import Path
import platform
import subprocess
import sys
import tempfile
import unittest


SCRIPT = Path(__file__).with_name("broadcast-fixture-cache.py")


class FixtureCacheTest(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.raw = Path(self.temp.name) / "raw"
        for rendition in ("1080p", "720p", "480p"):
            directory = self.raw / rendition
            directory.mkdir(parents=True)
            for name in ("init.mp4", "index.m3u8", *(f"seg-{i:06d}.m4s" for i in range(24))):
                (directory / name).write_bytes(b"synthetic fixture")

    def run_cache(self, mode, fingerprint="source-and-tools"):
        return subprocess.run([sys.executable, str(SCRIPT), mode, str(self.raw), fingerprint], capture_output=True)

    def seal(self):
        result = self.run_cache("seal")
        self.assertEqual(result.returncode, 0, result.stderr)

    def test_complete_fixture_is_accepted(self):
        self.seal()
        self.assertEqual(self.run_cache("validate").returncode, 0)

    def test_invalid_cache_never_reaches_media_reuse(self):
        mutations = {
            "missing": lambda: (self.raw / "720p/seg-000010.m4s").unlink(),
            "corrupt": lambda: (self.raw / "720p/seg-000010.m4s").write_bytes(b"changed"),
            "extra": lambda: (self.raw / "720p/extra").touch(),
            "empty": lambda: (self.raw / "720p/seg-000010.m4s").write_bytes(b""),
            "symlink": self.replace_with_symlink,
            "directory_symlink": self.replace_directory_with_symlink,
            "root_symlink": self.replace_root_with_symlink,
            "manifest_symlink": self.replace_manifest_with_symlink,
            "partial_manifest": self.truncate_manifest,
            "malformed_manifest": lambda: (self.raw / ".fixture-manifest.json").write_text("{"),
            "missing_manifest": lambda: (self.raw / ".fixture-manifest.json").unlink(),
        }
        for name, mutate in mutations.items():
            with self.subTest(name=name):
                self.setUp()
                self.seal()
                mutate()
                self.assertNotEqual(self.run_cache("validate").returncode, 0)

    def replace_with_symlink(self):
        target = self.raw / "720p/seg-000010.m4s"
        target.unlink()
        target.symlink_to(self.raw / "480p/seg-000010.m4s")

    def replace_directory_with_symlink(self):
        original = self.raw.parent / "original"
        (self.raw / "720p").rename(original)
        (self.raw / "720p").symlink_to(original, target_is_directory=True)

    def replace_root_with_symlink(self):
        original = self.raw.parent / "original"
        self.raw.rename(original)
        self.raw.symlink_to(original, target_is_directory=True)

    def replace_manifest_with_symlink(self):
        manifest = self.raw / ".fixture-manifest.json"
        target = self.raw.parent / "manifest.json"
        manifest.rename(target)
        manifest.symlink_to(target)

    def truncate_manifest(self):
        path = self.raw / ".fixture-manifest.json"
        manifest = json.loads(path.read_text())
        del manifest["files"]["720p/seg-000010.m4s"]
        path.write_text(json.dumps(manifest))

    def test_changed_source_or_tools_rejects_cache(self):
        self.seal()
        self.assertNotEqual(self.run_cache("validate", "changed-source-or-tools").returncode, 0)

    def test_seal_rejects_partial_generation(self):
        (self.raw / "720p/seg-000010.m4s").unlink()
        self.assertNotEqual(self.run_cache("seal").returncode, 0)

    @unittest.skipUnless(platform.system() == "Linux", "fingerprinting requires Linux ldd")
    def test_key_changes_with_generation_source_tool_version_binary_or_library(self):
        root = Path(self.temp.name)
        for source in ("internal/recordingvalidation/broadcast_media_test.go", "internal/assets/recording_live.go", "scripts/broadcast-fixture-cache.py"):
            path = root / source
            path.parent.mkdir(parents=True, exist_ok=True)
            path.write_text("generation input")
        tools = root / "bin"
        tools.mkdir()
        library = root / "libx264.so.164"
        library.write_bytes(b"encoder-v1")
        loader = root / "ld-linux.so"
        loader.write_bytes(b"loader-v1")
        ldd = tools / "ldd"
        ldd.write_text(f'#!/bin/sh\necho "linux-vdso.so.1 (0x0000)"\necho "libx264.so.164 => {library} (0x0000)"\necho "{loader} (0x0000)"\n')
        ldd.chmod(0o700)
        for name in ("ffmpeg", "ffprobe"):
            tool = tools / name
            tool.write_text('#!/bin/sh\necho "${VERSION:-version-1}"\n')
            tool.chmod(0o700)
        env = {**os.environ, "PATH": str(tools)}

        def key():
            return subprocess.check_output([sys.executable, str(SCRIPT), "key"], cwd=root, env=env)

        previous = key()
        for source in ("internal/recordingvalidation/broadcast_media_test.go", "internal/assets/recording_live.go", "scripts/broadcast-fixture-cache.py", "bin/ffmpeg", "bin/ffprobe"):
            path = root / source
            path.write_text(path.read_text() + "\n# changed input\n")
            current = key()
            self.assertNotEqual(previous, current, source)
            previous = current
        env["VERSION"] = "version-2"
        self.assertNotEqual(previous, key())
        previous = key()
        library.write_bytes(b"encoder-v2")
        self.assertNotEqual(previous, key(), "library-only changes must invalidate raw fixtures")
        for output in ('echo "libx264.so.164 => not found"', 'exit 0'):
            ldd.write_text(f"#!/bin/sh\n{output}\n")
            result = subprocess.run([sys.executable, str(SCRIPT), "key"], cwd=root, env=env, capture_output=True)
            self.assertNotEqual(result.returncode, 0, "incomplete library resolution must refuse caching")

    @unittest.skipIf(platform.system() == "Linux", "Linux dependency resolution is supported")
    def test_unsupported_platform_refuses_cache_fingerprinting(self):
        result = subprocess.run([sys.executable, str(SCRIPT), "key"], capture_output=True)
        self.assertNotEqual(result.returncode, 0)
        self.assertIn(b"requires Linux shared-library resolution", result.stderr)


if __name__ == "__main__":
    unittest.main()
