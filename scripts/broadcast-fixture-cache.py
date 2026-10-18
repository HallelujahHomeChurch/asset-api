#!/usr/bin/env python3
"""Cache only immutable synthetic broadcast inputs; never validation results."""
import hashlib
import json
from pathlib import Path
import platform
import shutil
import stat
import subprocess
import sys


def fingerprint():
    digest = hashlib.sha256()
    for source in (
        "internal/recordingvalidation/broadcast_media_test.go",
        "internal/assets/recording_live.go",
        "scripts/broadcast-fixture-cache.py",
    ):
        digest.update(source.encode() + b"\0" + Path(source).read_bytes())
    digest.update(f"{platform.system()}/{platform.machine()}".encode())
    for tool in ("ffmpeg", "ffprobe"):
        binary = shutil.which(tool)
        if binary is None:
            raise ValueError(f"missing {tool}")
        digest.update(subprocess.check_output([binary, "-version"]))
        digest.update(Path(binary).read_bytes())
    return digest.hexdigest()


def file_hashes(raw):
    renditions = {"1080p", "720p", "480p"}
    names = {"init.mp4", "index.m3u8", *(f"seg-{i:06d}.m4s" for i in range(24))}
    manifest = raw / ".fixture-manifest.json"
    if raw.is_symlink() or not raw.is_dir():
        raise ValueError("raw fixture must be a directory")
    if {p.name for p in raw.iterdir()} - {manifest.name} != renditions:
        raise ValueError("unexpected rendition layout")
    if manifest.exists() or manifest.is_symlink():
        if not stat.S_ISREG(manifest.lstat().st_mode):
            raise ValueError("manifest must be a regular file")
    hashes = {}
    for rendition in sorted(renditions):
        directory = raw / rendition
        if directory.is_symlink() or not directory.is_dir():
            raise ValueError("rendition must be a directory")
        if {p.name for p in directory.iterdir()} != names:
            raise ValueError("incomplete or unexpected raw fixture files")
        for name in sorted(names):
            path = directory / name
            if not stat.S_ISREG(path.lstat().st_mode) or path.stat().st_size == 0:
                raise ValueError("fixture must contain nonempty regular files")
            hashes[f"{rendition}/{name}"] = hashlib.sha256(path.read_bytes()).hexdigest()
    return hashes


def main():
    mode = sys.argv[1]
    if mode == "key":
        print(fingerprint())
        return
    raw, key = Path(sys.argv[2]), sys.argv[3]
    manifest = raw / ".fixture-manifest.json"
    expected = {"fingerprint": key, "files": file_hashes(raw)}
    if mode == "seal":
        manifest.write_text(json.dumps(expected, sort_keys=True) + "\n")
    elif mode == "validate":
        if json.loads(manifest.read_text()) != expected:
            raise ValueError("fixture fingerprint or SHA256 manifest mismatch")
    else:
        raise ValueError("unknown fixture cache command")


if __name__ == "__main__":
    try:
        main()
    except (OSError, ValueError, subprocess.CalledProcessError) as error:
        print(f"broadcast fixture cache miss: {error}", file=sys.stderr)
        sys.exit(1)
