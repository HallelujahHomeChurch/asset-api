#!/usr/bin/env bash
set -euo pipefail
container="$(docker create "$1")" || exit 1
trap 'docker rm "$container" >/dev/null' EXIT
# Consume the entire export once so pipefail retains export/tar errors.
files="$(docker export "$container" | tar -tf -)" || exit 1
grep -Fx 'asset-derivative-worker' <<<"$files" >/dev/null || exit 1
