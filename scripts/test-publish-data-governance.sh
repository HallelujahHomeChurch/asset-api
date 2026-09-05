#!/usr/bin/env bash
set -euo pipefail

publisher="$PWD/scripts/publish-data-governance.sh"
fixture_root="$(mktemp -d)"
trap 'rm -rf "$fixture_root"' EXIT
mkdir "$fixture_root/bin"
cat > "$fixture_root/bin/az" <<'SHIM'
#!/usr/bin/env bash
set -euo pipefail
[[ "$1 $2" == 'storage blob' ]] || exit 90
operation="$3"
shift 3
name= file= auth= overwrite= account= container=
while (($#)); do
  case "$1" in
    --name) name="$2"; shift 2 ;;
    --file) file="$2"; shift 2 ;;
    --auth-mode) auth="$2"; shift 2 ;;
    --account-name) account="$2"; shift 2 ;;
    --container-name) container="$2"; shift 2 ;;
    --overwrite) overwrite="$2"; shift 2 ;;
    --query) [[ "$2" == exists ]] || exit 90; shift 2 ;;
    --output) [[ "$2" == tsv || "$2" == none ]] || exit 90; shift 2 ;;
    --only-show-errors|--no-progress) shift ;;
    *) exit 90 ;;
  esac
done
[[ "$auth" == login && "$account" == fixturestorage && "$container" == api-docs-asset-api ]] || exit 90
[[ "$name" == governance/* && "$name" != *..* ]] || exit 90
printf '%s %s\n' "$operation" "$name" >> "$AZ_FIXTURE/calls"
blob="$AZ_FIXTURE/blobs/$name"
case "$operation" in
  exists)
    if [[ -f "$blob" ]]; then printf 'true\n'; else printf 'false\n'; fi ;;
  download)
    [[ "${AZ_FAIL_DOWNLOAD:-}" != "$name" ]] || exit 1
    cp "$blob" "$file"
    if [[ "${AZ_CORRUPT_DOWNLOAD:-}" == "$name" ]]; then printf 'corrupt\n' > "$file"; fi ;;
  upload)
    if [[ "$name" == governance/current.json ]]; then
      [[ "$overwrite" == true ]] || exit 90
      for payload in data-governance.yaml data-governance.json provenance.json; do
        [[ -f "$AZ_FIXTURE/blobs/governance/manifests/$RELEASE_COMMIT/$payload" ]] || exit 90
      done
    else
      [[ "$overwrite" == false ]] || exit 90
    fi
    [[ "${AZ_FAIL_UPLOAD:-}" != "$name" ]] || exit 1
    if [[ -e "$blob" && "$overwrite" == false ]]; then exit 1; fi
    mkdir -p "$(dirname "$blob")"
    cp "$file" "$blob"
    if [[ "${AZ_CORRUPT_UPLOAD:-}" == "$name" ]]; then printf 'corrupt\n' > "$blob"; fi
    printf '%s\n' "$name" >> "$AZ_FIXTURE/uploads" ;;
  *) exit 91 ;;
esac
SHIM
chmod +x "$fixture_root/bin/az"
export PATH="$fixture_root/bin:$PATH"
export SERVICE=asset-api STORAGE_ACCOUNT=fixturestorage CONTAINER=api-docs-asset-api
export RELEASE_COMMIT=aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
export RELEASE_IMAGE=example.invalid/asset-api@sha256:dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd
export RELEASE_URL=https://github.com/HallelujahHomeChurch/asset-api/actions/runs/20
prefix="governance/manifests/$RELEASE_COMMIT"

fresh_case() {
  AZ_FIXTURE="$(mktemp -d "$fixture_root/case.XXXXXX")"
  GOVERNANCE_DIR="$AZ_FIXTURE/export"
  export AZ_FIXTURE GOVERNANCE_DIR
  unset AZ_FAIL_UPLOAD AZ_FAIL_DOWNLOAD AZ_CORRUPT_DOWNLOAD AZ_CORRUPT_UPLOAD FAIL_GOVERNANCE_BEFORE_POINTER
  mkdir -p "$GOVERNANCE_DIR" "$AZ_FIXTURE/blobs/governance" "$AZ_FIXTURE/blobs/specs"
  printf 'schema_version: 1\nservice: asset-api\ndatasets: []\n' > "$GOVERNANCE_DIR/data-governance.yaml"
  printf '{"schema_version":1,"service":"asset-api","datasets":[]}\n' > "$GOVERNANCE_DIR/data-governance.json"
  printf 'untouched OpenAPI pointer\n' > "$AZ_FIXTURE/blobs/current.json"
  printf 'untouched OpenAPI spec\n' > "$AZ_FIXTURE/blobs/specs/sentinel.yaml"
  cp "$AZ_FIXTURE/blobs/current.json" "$AZ_FIXTURE/root-sentinel"
  cp "$AZ_FIXTURE/blobs/specs/sentinel.yaml" "$AZ_FIXTURE/spec-sentinel"
  jq -n --arg image "$RELEASE_IMAGE" '{schemaVersion:1,service:"asset-api",commit:"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",manifestSha256:("b"*64),jsonSha256:("c"*64),image:$image,releaseUrl:"https://github.com/HallelujahHomeChurch/asset-api/actions/runs/19",publishedAt:"2026-09-03T00:00:00Z"}' > "$AZ_FIXTURE/blobs/governance/current.json"
  cp "$AZ_FIXTURE/blobs/governance/current.json" "$AZ_FIXTURE/sentinel"
}

run_publisher() {
  local status=0
  bash "$publisher" > "$AZ_FIXTURE/output" 2>&1 || status=$?
  cmp "$AZ_FIXTURE/root-sentinel" "$AZ_FIXTURE/blobs/current.json" || exit 1
  cmp "$AZ_FIXTURE/spec-sentinel" "$AZ_FIXTURE/blobs/specs/sentinel.yaml" || exit 1
  return "$status"
}

expect_failure() {
  if run_publisher; then cat "$AZ_FIXTURE/output" >&2; echo 'publisher unexpectedly succeeded' >&2; exit 1; fi
  cmp "$AZ_FIXTURE/sentinel" "$AZ_FIXTURE/blobs/governance/current.json"
}

fresh_case
if ! run_publisher; then cat "$AZ_FIXTURE/output" >&2; exit 1; fi
for payload in data-governance.yaml data-governance.json; do cmp "$GOVERNANCE_DIR/$payload" "$AZ_FIXTURE/blobs/$prefix/$payload"; done
cmp "$AZ_FIXTURE/blobs/$prefix/provenance.json" "$AZ_FIXTURE/blobs/governance/current.json"
test "$(cat "$AZ_FIXTURE/uploads")" = "$(printf '%s\n' "$prefix/data-governance.yaml" "$prefix/data-governance.json" "$prefix/provenance.json" governance/current.json)"
jq -e --arg yaml "$(sha256sum "$GOVERNANCE_DIR/data-governance.yaml" | cut -d ' ' -f 1)" --arg json "$(sha256sum "$GOVERNANCE_DIR/data-governance.json" | cut -d ' ' -f 1)" --arg image "$RELEASE_IMAGE" '
  keys == ["commit","image","jsonSha256","manifestSha256","publishedAt","releaseUrl","schemaVersion","service"] and
  .schemaVersion == 1 and .service == "asset-api" and .commit == "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" and
  .manifestSha256 == $yaml and .jsonSha256 == $json and .image == $image and
  .releaseUrl == "https://github.com/HallelujahHomeChurch/asset-api/actions/runs/20" and
  (.publishedAt | fromdateiso8601 | type) == "number"
' "$AZ_FIXTURE/blobs/governance/current.json" >/dev/null

# Same-commit retry reuses the originally stored provenance rather than a retry's run URL.
cp "$AZ_FIXTURE/blobs/$prefix/provenance.json" "$AZ_FIXTURE/original-provenance"
RELEASE_URL=https://github.com/HallelujahHomeChurch/asset-api/actions/runs/21 run_publisher
cmp "$AZ_FIXTURE/original-provenance" "$AZ_FIXTURE/blobs/$prefix/provenance.json"
cmp "$AZ_FIXTURE/original-provenance" "$AZ_FIXTURE/blobs/governance/current.json"
test "$(grep -c '^governance/manifests/' "$AZ_FIXTURE/uploads")" -eq 3
cp "$AZ_FIXTURE/blobs/governance/current.json" "$AZ_FIXTURE/sentinel"
printf 'changed\n' >> "$GOVERNANCE_DIR/data-governance.yaml"
expect_failure
cmp "$AZ_FIXTURE/original-provenance" "$AZ_FIXTURE/blobs/$prefix/provenance.json"

fresh_case
rm "$AZ_FIXTURE/blobs/governance/current.json"
run_publisher
cmp "$AZ_FIXTURE/blobs/$prefix/provenance.json" "$AZ_FIXTURE/blobs/governance/current.json"
jq '.releaseUrl="https://github.com/HallelujahHomeChurch/asset-api/actions/runs/21"' "$AZ_FIXTURE/blobs/governance/current.json" > "$AZ_FIXTURE/sentinel"
cp "$AZ_FIXTURE/sentinel" "$AZ_FIXTURE/blobs/governance/current.json"
RELEASE_URL=https://github.com/HallelujahHomeChurch/asset-api/actions/runs/22 expect_failure

fresh_case
FAIL_GOVERNANCE_BEFORE_POINTER=true expect_failure
test -f "$AZ_FIXTURE/blobs/$prefix/provenance.json"
cp "$AZ_FIXTURE/blobs/$prefix/provenance.json" "$AZ_FIXTURE/original-provenance"
run_publisher
cmp "$AZ_FIXTURE/original-provenance" "$AZ_FIXTURE/blobs/governance/current.json"
cp "$AZ_FIXTURE/blobs/governance/current.json" "$AZ_FIXTURE/sentinel"
RELEASE_IMAGE=example.invalid/asset-api@sha256:eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee expect_failure
cmp "$AZ_FIXTURE/original-provenance" "$AZ_FIXTURE/blobs/$prefix/provenance.json"

for payload in data-governance.yaml data-governance.json provenance.json; do
  fresh_case; AZ_FAIL_UPLOAD="$prefix/$payload" expect_failure
  fresh_case; AZ_FAIL_DOWNLOAD="$prefix/$payload" expect_failure
  fresh_case; AZ_CORRUPT_DOWNLOAD="$prefix/$payload" expect_failure
  fresh_case; AZ_CORRUPT_UPLOAD="$prefix/$payload" expect_failure
done
fresh_case; AZ_FAIL_UPLOAD=governance/current.json expect_failure

for payload in data-governance.yaml data-governance.json provenance.json; do
  fresh_case
  mkdir -p "$AZ_FIXTURE/blobs/$prefix"
  printf 'conflicting immutable bytes\n' > "$AZ_FIXTURE/blobs/$prefix/$payload"
  cp "$AZ_FIXTURE/blobs/$prefix/$payload" "$AZ_FIXTURE/conflict"
  expect_failure
  cmp "$AZ_FIXTURE/conflict" "$AZ_FIXTURE/blobs/$prefix/$payload"
done

for mutation in '.commit="bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"' '.manifestSha256=("e"*64)' '.jsonSha256=("e"*64)'; do
  fresh_case
  FAIL_GOVERNANCE_BEFORE_POINTER=true expect_failure
  jq "$mutation" "$AZ_FIXTURE/blobs/$prefix/provenance.json" > "$AZ_FIXTURE/conflict"
  cp "$AZ_FIXTURE/conflict" "$AZ_FIXTURE/blobs/$prefix/provenance.json"
  expect_failure
  cmp "$AZ_FIXTURE/conflict" "$AZ_FIXTURE/blobs/$prefix/provenance.json"
done

for pair in '21 20 failure' '20 20 failure' '100000000000000000000 99999999999999999999 failure' '99999999999999999999 100000000000000000000 success' '9007199254740993 9007199254740992 failure'; do
  read -r previous candidate result <<< "$pair"
  fresh_case
  jq --arg url "https://github.com/HallelujahHomeChurch/asset-api/actions/runs/$previous" '.releaseUrl=$url' "$AZ_FIXTURE/sentinel" > "$AZ_FIXTURE/blobs/governance/current.json"
  cp "$AZ_FIXTURE/blobs/governance/current.json" "$AZ_FIXTURE/sentinel"
  if [[ "$result" == failure ]]; then RELEASE_URL="https://github.com/HallelujahHomeChurch/asset-api/actions/runs/$candidate" expect_failure; else RELEASE_URL="https://github.com/HallelujahHomeChurch/asset-api/actions/runs/$candidate" run_publisher; fi
done

for mutation in '.schemaVersion=2' '.service="notification-api"' '.commit="../bad"' '.manifestSha256="bad"' '.jsonSha256="bad"' '.image="mutable:tag"' '.releaseUrl="https://evil.invalid/actions/runs/30"' '.releaseUrl="https://github.com/HallelujahHomeChurch/asset-api/actions/runs/01"' '.publishedAt="2026-02-30T00:00:00Z"' '.extra=true' '.commit += "\n"' '.manifestSha256 += "\n"' '.jsonSha256 += "\n"' '.image += "\n"' '.releaseUrl += "\n"'; do
  fresh_case
  jq "$mutation" "$AZ_FIXTURE/sentinel" > "$AZ_FIXTURE/blobs/governance/current.json"
  cp "$AZ_FIXTURE/blobs/governance/current.json" "$AZ_FIXTURE/sentinel"
  expect_failure
done
fresh_case; printf '{' > "$AZ_FIXTURE/blobs/governance/current.json"; cp "$AZ_FIXTURE/blobs/governance/current.json" "$AZ_FIXTURE/sentinel"; expect_failure
fresh_case; jq '.releaseUrl="https://githubXcom/HallelujahHomeChurch/asset-api/actions/runs/19"' "$AZ_FIXTURE/sentinel" > "$AZ_FIXTURE/blobs/governance/current.json"; cp "$AZ_FIXTURE/blobs/governance/current.json" "$AZ_FIXTURE/sentinel"; expect_failure

for input in 'SERVICE=notification-api' 'CONTAINER=api-docs-notification-api' 'RELEASE_COMMIT=../bad' 'RELEASE_IMAGE=mutable:tag' 'RELEASE_URL=http://github.com/HallelujahHomeChurch/asset-api/actions/runs/20' 'RELEASE_URL=https://github.com/HallelujahHomeChurch/notification-api/actions/runs/20' 'RELEASE_URL=https://github.com/HallelujahHomeChurch/asset-api/actions/runs/0'; do
  fresh_case
  if env "$input" bash "$publisher" > "$AZ_FIXTURE/output" 2>&1; then exit 1; fi
  test ! -e "$AZ_FIXTURE/calls"
done
fresh_case; printf 'invalid json' > "$GOVERNANCE_DIR/data-governance.json"; expect_failure; test ! -e "$AZ_FIXTURE/calls"
fresh_case; rm "$GOVERNANCE_DIR/data-governance.yaml"; expect_failure; test ! -e "$AZ_FIXTURE/calls"
echo 'governance publisher: success, immutable retry/conflicts, failures, ordering and input guards passed'
