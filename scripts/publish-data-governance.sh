#!/usr/bin/env bash
set -euo pipefail
export LC_ALL=C

fail() { echo "$1" >&2; exit 1; }
for input in SERVICE RELEASE_COMMIT RELEASE_IMAGE RELEASE_URL GOVERNANCE_DIR STORAGE_ACCOUNT CONTAINER; do
  [[ -n "${!input:-}" ]] || fail "Missing $input"
done
[[ "$SERVICE" == asset-api && "$CONTAINER" == api-docs-asset-api ]] || fail 'Invalid service/container pairing'
[[ "$STORAGE_ACCOUNT" =~ ^[a-z0-9]{3,24}$ ]] || fail 'Invalid storage account'
[[ "$RELEASE_COMMIT" =~ ^[a-f0-9]{40}$ ]] || fail 'Invalid release commit'
image_pattern='^[a-zA-Z0-9][a-zA-Z0-9._:/-]*@sha256:[a-f0-9]{64}$'
[[ "$RELEASE_IMAGE" =~ $image_pattern ]] || fail 'Expected an immutable image digest'
release_prefix='https://github.com/HallelujahHomeChurch/asset-api/actions/runs/'
[[ "$RELEASE_URL" == "$release_prefix"* && "${RELEASE_URL#"$release_prefix"}" =~ ^[1-9][0-9]*$ ]] || fail 'Expected canonical GitHub release run URL'
for payload in data-governance.yaml data-governance.json; do
  [[ -f "$GOVERNANCE_DIR/$payload" && -s "$GOVERNANCE_DIR/$payload" && ! -L "$GOVERNANCE_DIR/$payload" ]] || fail "Invalid source $payload"
done
jq -e -s 'length == 1 and (.[0] | type == "object" and .schema_version == 1 and .service == "asset-api" and (.datasets | type == "array"))' "$GOVERNANCE_DIR/data-governance.json" >/dev/null || fail 'Invalid normalized manifest'

hash() { sha256sum "$1" | cut -d ' ' -f 1; }
yaml_hash="$(hash "$GOVERNANCE_DIR/data-governance.yaml")"
json_hash="$(hash "$GOVERNANCE_DIR/data-governance.json")"
blob_prefix="governance/manifests/$RELEASE_COMMIT"
scratch="$(mktemp -d)"
trap 'rm -rf "$scratch"' EXIT
storage=(--auth-mode login --account-name "$STORAGE_ACCOUNT" --container-name "$CONTAINER" --only-show-errors)

exists() {
  local result
  result="$(az storage blob exists "${storage[@]}" --name "$1" --query exists --output tsv)" || return 1
  [[ "$result" == true || "$result" == false ]] || fail 'Invalid blob existence response'
  printf '%s' "$result"
}
download() { az storage blob download "${storage[@]}" --name "$1" --file "$2" --overwrite true --no-progress --output none; }
validate_metadata() {
  jq -e -s --arg image_pattern "${image_pattern%$}\\z" '
    length == 1 and (.[0] |
    keys == ["commit","image","jsonSha256","manifestSha256","publishedAt","releaseUrl","schemaVersion","service"] and
    .schemaVersion == 1 and .service == "asset-api" and
    (.commit | type == "string" and test("^[a-f0-9]{40}\\z")) and
    (.manifestSha256 | type == "string" and test("^[a-f0-9]{64}\\z")) and
    (.jsonSha256 | type == "string" and test("^[a-f0-9]{64}\\z")) and
    (.image | type == "string" and test($image_pattern)) and
    (.releaseUrl | type == "string" and test("^https://github\\.com/HallelujahHomeChurch/asset-api/actions/runs/[1-9][0-9]*\\z")) and
    (.publishedAt | type == "string" and test("^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}Z\\z") and (. as $date | fromdateiso8601 | todateiso8601 == $date)))
  ' "$1" >/dev/null || fail 'Invalid governance metadata'
}

# Commit-addressed blobs are immutable. A retry verifies stored bytes before any pointer update.
for payload in data-governance.yaml data-governance.json; do
  blob="$blob_prefix/$payload"
  present="$(exists "$blob")"
  if [[ "$present" == false ]]; then
    az storage blob upload "${storage[@]}" --name "$blob" --file "$GOVERNANCE_DIR/$payload" --overwrite false --output none
  fi
  download "$blob" "$scratch/$payload"
  [[ "$(hash "$scratch/$payload")" == "$(hash "$GOVERNANCE_DIR/$payload")" ]] || fail "Immutable $payload checksum conflict"
done

provenance_blob="$blob_prefix/provenance.json"
present="$(exists "$provenance_blob")"
if [[ "$present" == false ]]; then
  jq -n --arg service "$SERVICE" --arg commit "$RELEASE_COMMIT" \
    --arg manifestSha256 "$yaml_hash" --arg jsonSha256 "$json_hash" \
    --arg image "$RELEASE_IMAGE" --arg releaseUrl "$RELEASE_URL" \
    --arg publishedAt "$(date -u +%Y-%m-%dT%H:%M:%SZ)" \
    '{schemaVersion:1,service:$service,commit:$commit,manifestSha256:$manifestSha256,jsonSha256:$jsonSha256,image:$image,releaseUrl:$releaseUrl,publishedAt:$publishedAt}' > "$scratch/new-provenance.json"
  az storage blob upload "${storage[@]}" --name "$provenance_blob" --file "$scratch/new-provenance.json" --overwrite false --output none
fi
download "$provenance_blob" "$scratch/provenance.json"
if [[ "$present" == false ]]; then cmp "$scratch/new-provenance.json" "$scratch/provenance.json" || fail 'Stored provenance differs from upload'; fi
validate_metadata "$scratch/provenance.json"
jq -e --arg commit "$RELEASE_COMMIT" --arg yaml "$yaml_hash" --arg json "$json_hash" --arg image "$RELEASE_IMAGE" \
  '.commit == $commit and .manifestSha256 == $yaml and .jsonSha256 == $json and .image == $image' \
  "$scratch/provenance.json" >/dev/null || fail 'Immutable provenance conflict'

# The immutable provenance carries the original run ID; only publish a newer pointer after all immutable checks pass.
candidate_run="$(jq -r '.releaseUrl | split("/")[-1]' "$scratch/provenance.json")"
present="$(exists governance/current.json)"
if [[ "$present" == true ]]; then
  download governance/current.json "$scratch/current.json"
  validate_metadata "$scratch/current.json"
  current_run="$(jq -r '.releaseUrl | split("/")[-1]' "$scratch/current.json")"
  if [[ ${#current_run} -gt ${#candidate_run} ]] || { [[ ${#current_run} -eq ${#candidate_run} ]] && [[ "$current_run" > "$candidate_run" ]]; }; then
    fail 'A newer governance release is already current'
  fi
  if [[ "$current_run" == "$candidate_run" ]]; then cmp "$scratch/current.json" "$scratch/provenance.json" || fail 'Conflicting governance pointer for the same release run'; fi
fi
cp "$scratch/provenance.json" "$GOVERNANCE_DIR/provenance.json"
if [[ "${FAIL_GOVERNANCE_BEFORE_POINTER:-false}" == true ]]; then fail 'Requested failure before governance pointer'; fi
az storage blob upload "${storage[@]}" --name governance/current.json --file "$GOVERNANCE_DIR/provenance.json" --overwrite true --output none
