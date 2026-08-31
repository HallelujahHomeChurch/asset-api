#!/bin/sh
set -eu

scanner='ghcr.io/gitleaks/gitleaks@sha256:c00b6bd0aeb3071cbcb79009cb16a60dd9e0a7c60e2be9ab65d25e6bc8abbb7f'

fixture_dir=$(mktemp -d)
cleanup() {
  rm -rf "$fixture_dir"
}
trap cleanup EXIT INT TERM

git clone --quiet --no-checkout "$PWD" "$fixture_dir/history"
git -C "$fixture_dir/history" checkout --quiet --detach "$(git rev-parse HEAD)"
docker run --rm -v "$fixture_dir/history:/history:ro" -v "$PWD/.gitleaks.toml:/gitleaks.toml:ro" \
  "$scanner" git /history \
  --config /gitleaks.toml --redact=100 --exit-code 1 --no-banner

mkdir "$fixture_dir/control"
printf 'api_key = "%s%s%s%s"\n' 6f8a2e9c 4d7b1a3f 5e8c0d2b 4a6f9c1e > "$fixture_dir/control/secret.txt"
set +e
docker run --rm -v "$fixture_dir/control:/fixture:ro" -v "$PWD/.gitleaks.toml:/gitleaks.toml:ro" \
  "$scanner" dir /fixture \
  --config /gitleaks.toml --redact=100 --exit-code 1 --no-banner >/dev/null 2>&1
fixture_status=$?
set -e

if [ "$fixture_status" -ne 1 ]; then
  echo "gitleaks negative control returned $fixture_status, want 1" >&2
  exit 1
fi

echo GITLEAKS_CONFIG_OK
