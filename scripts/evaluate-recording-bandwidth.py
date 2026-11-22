"""Compare packaged H.264 and AV1 download bytes for 1080p30 SDR recording excerpts.

Supply 1920x1080 progressive square-pixel SDR clips at 30 or 30000/1001 fps,
with audio and duration >=65 seconds. Output folders retain playable HLS for review.
"""
import argparse
from fractions import Fraction
import hashlib
import json
from pathlib import Path
import subprocess

# height, actual output width for a 16:9 input, AVC and AV1 kbps, target savings %
LADDER = [(480, 852, 800, 540, 20), (720, 1280, 1500, 960, 30),
          (1080, 1920, 3000, 1650, 40)]


def probe(path, count=False):
    command = ["ffprobe", "-v", "error", "-show_streams", "-show_format", "-of", "json"]
    if count:
        command += ["-count_frames"]
    return json.loads(subprocess.check_output([*command, str(path)]))


def audio_digest(path):
    hashes = subprocess.check_output([
        "ffprobe", "-v", "error", "-select_streams", "a:0", "-show_packets",
        "-show_data_hash", "sha256", "-show_entries", "packet=data_hash", "-of", "csv=p=0", str(path)])
    if b"SHA256:" not in hashes:
        raise ValueError("Missing packaged audio packets")
    return hashlib.sha256(hashes).hexdigest()


def encode(source, directory, codec, bitrate, width, height, fps, duration):
    directory.mkdir(parents=True)
    source_args = (["-c:v", "libx264", "-profile:v", "high", "-preset", "medium",
                    "-bf", "3", "-sc_threshold", "0", "-rc-lookahead", "40"] if codec == "h264"
                   else ["-c:v", "libsvtav1", "-preset", "8", "-svtav1-params", "lp=3"])
    if codec == "h264":
        maximum = bitrate * 1000 * 4 // 3
        source_args += ["-maxrate", str(maximum), "-bufsize", str(2 * maximum)]
    playlist = directory / "index.m3u8"
    subprocess.run(["ffmpeg", "-nostdin", "-loglevel", "error", "-i", str(source),
                    "-map", "0:v:0", "-map", "0:a:0", "-vf",
                    f"scale={width}:{height}:flags=lanczos,setsar=1",
                    *source_args, "-threads", "2", "-pix_fmt", "yuv420p", "-b:v", f"{bitrate}k",
                    "-r", str(fps), "-fps_mode", "cfr", "-g", "900", "-keyint_min", "900", "-flags", "+cgop",
                    "-force_key_frames", "expr:gte(t,n_forced*30)", "-c:a", "aac", "-b:a", "128k",
                    "-ar", "48000", "-ac", "2", "-af", "apad", "-t", str(duration),
                    "-f", "hls", "-hls_time", "30", "-hls_segment_type", "fmp4", "-hls_playlist_type",
                    "vod", "-hls_flags", "independent_segments", str(playlist)], check=True)
    streams = probe(playlist, count=True)["streams"]
    video, audio = streams
    if (video["codec_name"], video["width"], video["height"], audio["codec_name"]) != (codec, width, height, "aac"):
        raise ValueError("Unexpected packaged streams")
    if (audio["sample_rate"], audio["channels"]) != ("48000", 2):
        raise ValueError("Unexpected audio format")
    lengths = [float(line[8:].split(",")[0]) for line in playlist.read_text().splitlines()
               if line.startswith("#EXTINF:")]
    if len(lengths) < 3 or not 0 < lengths[-1] <= 30.1 or abs(sum(lengths) - duration) > 0.1 or any(abs(t - 30) > 0.1 for t in lengths[:-1]):
        raise ValueError(f"Unexpected segment boundaries: {lengths}")
    segments = [directory / name for name in playlist.read_text().splitlines() if name.endswith(".m4s")]
    init = (directory / "init.mp4").read_bytes()
    for segment in segments:
        subprocess.run(["ffmpeg", "-nostdin", "-v", "error", "-xerror", "-i", "pipe:0", "-map",
                        "0:v:0", "-f", "null", "-"], input=init + segment.read_bytes(), check=True)
    segment_kbps = [f.stat().st_size * 8 / (1000 * length) for f, length in zip(segments, lengths)]
    return dict(target_kbps=bitrate, peak_segment_kbps=max(segment_kbps), frames=int(video["nb_read_frames"]), segment_seconds=lengths, audio_digest=audio_digest(playlist),
                bytes=sum(f.stat().st_size for f in directory.iterdir()))


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("output", type=Path, help="new directory")
    parser.add_argument("clips", nargs="+", type=Path)
    args = parser.parse_args()
    args.output.mkdir(parents=True, exist_ok=False)
    results = []
    for index, source in enumerate(args.clips):
        info = probe(source)
        video = next(s for s in info["streams"] if s["codec_type"] == "video")
        fps = Fraction(video["avg_frame_rate"])
        duration = float(info["format"]["duration"])
        if ((video["width"], video["height"]) != (1920, 1080) or fps not in (30, Fraction(30000, 1001))
                or video.get("field_order") != "progressive" or duration < 65 or video.get("sample_aspect_ratio", "1:1") != "1:1"
                or video.get("color_transfer") in ("smpte2084", "arib-std-b67")):
            raise ValueError(f"Expected 1080p30 SDR excerpt of at least 65 seconds: {source.name}")
        samples = subprocess.check_output([
            "ffprobe", "-v", "error", "-select_streams", "v:0", "-show_frames",
            "-show_entries", "frame=best_effort_timestamp_time", "-of", "json", str(source)])
        pts = [float(frame["best_effort_timestamp_time"]) for frame in json.loads(samples)["frames"]]
        if (len(pts) < 65 * float(fps) - 1
                or any(abs(t - pts[0] - i / float(fps)) > 0.002 for i, t in enumerate(pts))
                or abs(len(pts) / float(fps) - duration) > 0.1):
            raise ValueError("Need aligned, constant-rate source video covering the full excerpt")
        for height, width, baseline, candidate, goal in LADDER:
            root = args.output / str(index) / str(height)
            avc = encode(source, root / "h264", "h264", baseline, width, height, fps, duration)
            av1 = encode(source, root / "av1", "av1", candidate, width, height, fps, duration)
            if avc["audio_digest"] != av1["audio_digest"]:
                raise ValueError("Packaged AAC payload differs between codecs")
            if not (len(pts) == avc["frames"] == av1["frames"]) or avc["segment_seconds"] != av1["segment_seconds"]:
                raise ValueError("Time-alignment mismatch")
            savings = 100 * (1 - av1["bytes"] / avc["bytes"])
            results.append(dict(clip=source.name, height=height, goal_percent=goal, h264=avc,
                                av1=av1, savings_percent=savings, quality_status="unmeasured", bandwidth_passed=savings >= goal))
    (args.output / "report.json").write_text(json.dumps(results, indent=2) + "\n")
    if not all(row["bandwidth_passed"] for row in results):
        raise SystemExit("At least one excerpt missed its bandwidth target")


if __name__ == "__main__":
    main()
