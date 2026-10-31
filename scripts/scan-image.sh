#!/bin/sh
set -eu
image=${1:?image required}
cache=${RUNNER_TEMP:-/tmp}/trivy-cache
report=$(mktemp)
trap 'rm -f "$report"' EXIT
mkdir -p "$cache"
docker run --rm -v /var/run/docker.sock:/var/run/docker.sock -v "$cache:/root/.cache/trivy" \
  ghcr.io/aquasecurity/trivy@sha256:62b1e65e8869bc4b4c6aa4fa2b21595256c7c2f6018a9d9ad61caf87187c1969 \
  image --scanners vuln,secret --severity HIGH,CRITICAL --format json "$image" > "$report"
python3 scripts/trivy-report.py "$report" "$image" >> "${GITHUB_STEP_SUMMARY:-/dev/stdout}"
