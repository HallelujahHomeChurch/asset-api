#!/usr/bin/env bash
# Exercise install-media-test-tools.sh retry and timeout behavior with stubs.
set -euo pipefail
root="$(cd "$(dirname "$0")/.." && pwd)"
script="$root/scripts/install-media-test-tools.sh"
work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT

# Isolated PATH: only the utilities the script and stubs need, so a runner
# image that already ships ffmpeg cannot short-circuit the retry cases.
tools="$work/tools"; mkdir -p "$tools"
for tool in bash env timeout sleep grep cp chmod cat; do
  ln -s "$(command -v "$tool")" "$tools/$tool"
done

make_stubs() {
  local dir="$1" mode="$2"
  mkdir -p "$dir"
  cat >"$dir/apt-get" <<STUB
#!/usr/bin/env bash
echo "apt-get \$*" >>"$dir/calls"
for arg in "\$@"; do
  if [[ "\$arg" == install ]]; then
    count=\$(grep -c ' install ' "$dir/calls" || true)
    case "$mode" in
      hang-once) if [[ "\$count" == 1 ]]; then sleep 30; fi ;;
      always-fail) exit 100 ;;
    esac
    printf '#!/usr/bin/env bash\nexit 0\n' >"$dir/ffmpeg"; cp "$dir/ffmpeg" "$dir/ffprobe"; chmod +x "$dir/ffmpeg" "$dir/ffprobe"
  fi
done
exit 0
STUB
  printf '#!/usr/bin/env bash\necho "dpkg $*" >>"%s/calls"\n' "$dir" >"$dir/dpkg"
  chmod +x "$dir/apt-get" "$dir/dpkg"
}

run_case() {
  local mode="$1" expected="$2" dir="$work/$1" status=0
  make_stubs "$dir" "$mode"
  PATH="$dir:$tools" MEDIA_TOOLS_SUDO="" MEDIA_TOOLS_INSTALL_TIMEOUT_SECONDS=2 \
    MEDIA_TOOLS_INSTALL_RETRY_DELAY_SECONDS=0 "$script" >"$dir/out" 2>&1 || status=$?
  if [[ "$status" != "$expected" ]]; then
    echo "case $mode: exit $status, want $expected" >&2
    cat "$dir/out" >&2
    exit 1
  fi
  echo "$dir/calls"
}

calls="$(run_case success 0)"
[[ "$(grep -c ' install ' "$calls")" == 1 ]] || { echo "success case retried" >&2; exit 1; }

calls="$(run_case hang-once 0)"
[[ "$(grep -c ' install ' "$calls")" == 2 ]] || { echo "hung attempt was not retried once" >&2; exit 1; }
grep -q '^dpkg --configure -a' "$calls" || { echo "dpkg was not repaired before retry" >&2; exit 1; }

calls="$(run_case always-fail 1)"
[[ "$(grep -c ' install ' "$calls")" == 3 ]] || { echo "failing install was not bounded to 3 attempts" >&2; exit 1; }

# Pre-installed tools skip apt entirely.
pre="$work/preinstalled"; mkdir -p "$pre"
printf '#!/usr/bin/env bash\nexit 0\n' >"$pre/ffmpeg"; cp "$pre/ffmpeg" "$pre/ffprobe"; chmod +x "$pre/ffmpeg" "$pre/ffprobe"
printf '#!/usr/bin/env bash\necho called >>"%s/calls"\nexit 1\n' "$pre" >"$pre/apt-get"; chmod +x "$pre/apt-get"
PATH="$pre:$tools" MEDIA_TOOLS_SUDO="" "$script" >/dev/null
[[ ! -e "$pre/calls" ]] || { echo "apt-get ran although tools exist" >&2; exit 1; }

echo "install-media-test-tools: retry and timeout cases passed"
