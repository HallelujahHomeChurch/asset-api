#!/bin/sh
set -eu

workflow=.github/workflows/release.yml

grep -q 'Resolve immutable image digest' "$workflow"
grep -q 'IMAGE_REF=${ACR_LOGIN_SERVER}/${IMAGE_REPOSITORY}@${digest}' "$workflow"
grep -q -- '--image "${IMAGE_REF}"' "$workflow"
grep -q -- '--image "$PREVIOUS_IMAGE_REF"' "$workflow"
grep -q 'deployed_image}" == "${IMAGE_REF}' "$workflow"

if grep -q 'image_ref="${ACR_LOGIN_SERVER}/${IMAGE_REPOSITORY}:${IMAGE_TAG}"' "$workflow"; then
  echo "release still deploys a mutable tag" >&2
  exit 1
fi
