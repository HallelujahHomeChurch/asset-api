"""Evaluate local 16:9 SDR recording excerpts at 720p. Print a video-only JSON report.

Usage: python3 scripts/evaluate-recording-av1.py sermon.mkv worship.mkv > report.json
Use pre-cut representative clips. Requires ffmpeg with libsvtav1, libx264, libvmaf.
"""

import argparse
import json
import math
from pathlib import Path
import statistics
import subprocess
import tempfile
import time


FPS = 30000 / 1001
MODEL = "vmaf_v0.6.1"
LADDER = {"libx264": (900, 1500), "libsvtav1": (900, 1200, 1500, 1800)}


def ffmpeg(folder, *args):
    subprocess.run(["ffmpeg", "-nostdin", "-loglevel", "error", "-y", *args],
                   cwd=folder, check=True)


def timestamps(folder, name):
    result = subprocess.check_output([
        "ffprobe", "-v", "error", "-select_streams", "v:0", "-show_frames",
        "-show_entries", "frame=best_effort_timestamp_time", "-of", "json", name,
    ], cwd=folder)
    return [frame["best_effort_timestamp_time"] for frame in json.loads(result)["frames"]]


def evaluate(folder, codec, rate, reference_pts):
    options = (["-preset", "fast"] if codec == "libx264" else
               ["-preset", "8", "-svtav1-params", "lp=3"])
    started = time.monotonic()
    ffmpeg(folder, "-i", "reference.mkv", "-c:v", codec, "-threads", "3", *options,
           "-b:v", f"{rate}k", "-g", "60", "candidate.mkv")
    elapsed = time.monotonic() - started
    if timestamps(folder, "candidate.mkv") != reference_pts:
        raise ValueError("Candidate presentation timestamps differ from the reference")
    ffmpeg(folder, "-i", "candidate.mkv", "-i", "reference.mkv", "-lavfi",
           f"libvmaf=model=version={MODEL}:n_threads=2:log_fmt=json:log_path=vmaf.json",
           "-f", "null", "-")
    frames = json.loads((folder / "vmaf.json").read_text())["frames"]
    scores = sorted(frame["metrics"]["vmaf"] for frame in frames)
    if len(scores) != len(reference_pts) or not scores or not all(map(math.isfinite, scores)):
        raise ValueError("Incomplete frame scores")
    packet_data = subprocess.check_output([
        "ffprobe", "-v", "error", "-select_streams", "v:0", "-show_packets",
        "-show_entries", "packet=pts_time,size,flags", "-of", "json", "candidate.mkv",
    ], cwd=folder)
    packets = sorted(json.loads(packet_data)["packets"], key=lambda p: float(p["pts_time"]))
    if len(packets) != len(scores) or "K" not in packets[0]["flags"]:
        raise ValueError("Expected one video packet per frame and an initial keyframe")
    keyframes = [i for i, packet in enumerate(packets) if "K" in packet["flags"]]
    sizes = [int(packet["size"]) for packet in packets]
    kbps = sum(sizes) * 8 * FPS / len(scores) / 1000
    gop_rates = [sum(sizes[start:end]) * 8 * FPS / (end - start) / 1000
                 for start, end in zip(keyframes, keyframes[1:] + [len(scores)])]
    mean = statistics.mean(scores)
    p5 = scores[math.ceil(len(scores) * 0.05) - 1]
    return dict(codec=codec, target_kbps=rate, video_kbps=kbps,
                peak_gop_video_kbps=max(gop_rates),
                encode_seconds=elapsed, vmaf_mean=mean, vmaf_p5=p5,
                passed=mean >= 93 and p5 >= 90)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("clips", type=Path, nargs="+")
    args = parser.parse_args()
    clips = []
    for source in args.clips:
        with tempfile.TemporaryDirectory() as temp:
            folder = Path(temp)
            if not source.is_file():
                parser.error(f"Missing local clip: {source}")
            ffmpeg(folder, "-i", str(source.resolve()), "-map", "0:v:0", "-an",
                   "-vf", "fps=30000/1001,scale=1280:720:flags=lanczos,setsar=1,format=yuv420p",
                   "-c:v", "ffv1", "reference.mkv")
            pts = timestamps(folder, "reference.mkv")
            rows = [evaluate(folder, codec, rate, pts)
                    for codec, rates in LADDER.items() for rate in rates]
            clips.append(dict(clip=source.name, frames=len(pts), candidates=rows))
    passing_sets = [{(r["codec"], r["target_kbps"]) for r in clip["candidates"] if r["passed"]}
                    for clip in clips]
    passing = [dict(codec=codec, target_kbps=rate)
               for codec, rate in sorted(set.intersection(*passing_sets))]
    version = subprocess.check_output(["ffmpeg", "-version"], text=True).splitlines()[0]
    print(json.dumps(dict(ffmpeg=version, model=MODEL, clips=clips,
                          passing_on_all_clips=passing), indent=2, allow_nan=False))


if __name__ == "__main__":
    main()
