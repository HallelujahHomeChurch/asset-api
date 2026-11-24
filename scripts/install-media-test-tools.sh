#!/usr/bin/env bash
# Install ffmpeg/ffprobe for media validation tests with bounded apt attempts.
# A rare apt mirror stall previously held CI and release verify jobs for
# 27-37 minutes; each attempt is now bounded and retried instead.
set -euo pipefail

attempts="${MEDIA_TOOLS_INSTALL_ATTEMPTS:-3}"
attempt_timeout="${MEDIA_TOOLS_INSTALL_TIMEOUT_SECONDS:-180}"
retry_delay="${MEDIA_TOOLS_INSTALL_RETRY_DELAY_SECONDS:-10}"
sudo_command="${MEDIA_TOOLS_SUDO-sudo}"

have_tools() { command -v ffmpeg >/dev/null && command -v ffprobe >/dev/null; }

if have_tools; then
  echo "ffmpeg and ffprobe already installed"
  exit 0
fi

script_dir="$(cd "${BASH_SOURCE[0]%/*}" && pwd)"
apt_options=(-o Acquire::Retries=3 -o Acquire::http::Timeout=30 -o Acquire::https::Timeout=30)
for ((attempt = 1; attempt <= attempts; attempt++)); do
  options=("${apt_options[@]}")
  if [[ "${ImageOS:-}" == ubuntu24 && "${RUNNER_ARCH:-}" == X64 && "$attempt" != 2 ]]; then
    # Use the signed Ubuntu archive directly. Keep the runner mirror as a fallback.
    options+=(-o "Dir::Etc::sourcelist=$script_dir/ubuntu-noble-media.sources" -o Dir::Etc::sourceparts=-)
  fi
  echo "Installing ffmpeg (attempt ${attempt}/${attempts}, ${attempt_timeout}s per apt step)"
  if $sudo_command timeout --kill-after=10 "$attempt_timeout" apt-get "${options[@]}" update \
    && $sudo_command timeout --kill-after=10 "$attempt_timeout" env DEBIAN_FRONTEND=noninteractive apt-get "${options[@]}" install -y --no-install-recommends ffmpeg \
    && have_tools; then
    exit 0
  fi
  if ((attempt < attempts)); then
    # A killed apt/dpkg may leave packages half-configured; repair before retrying.
    $sudo_command timeout 120 dpkg --configure -a || true
    sleep "$retry_delay"
  fi
done
echo "ffmpeg installation failed after ${attempts} bounded attempts" >&2
exit 1
