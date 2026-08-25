#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd -- "$(dirname -- "$0")/.." && pwd -P)"
runbook="$repo_root/docs/operations/line-media-acl-inventory.md"
test_dir="$(mktemp -d)"
trap 'rm -rf -- "$test_dir"' EXIT

extract_bash_block() {
  awk -v target="$1" '
    $0 == "```bash" { block += 1; next }
    block == target && $0 == "```" { exit }
    block == target { print }
  ' "$runbook"
}

extract_bash_block 1 > "$test_dir/inventory.sh"
extract_bash_block 2 > "$test_dir/validate-reset.sh"
test -s "$test_dir/inventory.sh"
test -s "$test_dir/validate-reset.sh"

sed \
  -e "s/ACCOUNT_INVENTORY_SERVICE='APPROVED_ACCOUNT_READ_ONLY_PGSERVICE'/ACCOUNT_INVENTORY_SERVICE='test-account'/" \
  -e "s/ASSET_INVENTORY_SERVICE='APPROVED_ASSET_READ_ONLY_PGSERVICE'/ASSET_INVENTORY_SERVICE='test-asset'/" \
  "$test_dir/inventory.sh" > "$test_dir/inventory-test.sh"

mkdir "$test_dir/bin" "$test_dir/tmp"
cat > "$test_dir/bin/psql" <<'FAKE_PSQL'
#!/usr/bin/env bash
set -euo pipefail
case "${PGSERVICE:-}" in
  test-account)
    printf 'user_id\n11111111-1111-1111-1111-111111111111\n' > account-users.tsv.partial
    printf 'role_id\n22222222-2222-2222-2222-222222222222\n' > account-roles.tsv.partial
    printf 'user_id\trole_id\n11111111-1111-1111-1111-111111111111\t22222222-2222-2222-2222-222222222222\n' > account-user-roles.tsv.partial
    ;;
  test-asset)
    printf 'collection_id\tacl_id\tsubject_type\tsubject_id\ncollection-direct\tacl-direct\tuser\t11111111-1111-1111-1111-111111111111\ncollection-role\tacl-role\trole\t22222222-2222-2222-2222-222222222222\ncollection-legacy\tacl-legacy\trole\tlegacy_reader\n' > asset-active-acls.tsv.partial
    ;;
  *)
    exit 1
    ;;
esac
FAKE_PSQL
chmod +x "$test_dir/bin/psql"

PATH="$test_dir/bin:$PATH" TMPDIR="$test_dir/tmp" \
  bash "$test_dir/inventory-test.sh" \
  > "$test_dir/candidates.tsv" 2> "$test_dir/counts.txt"

printf 'collection_id\tacl_id\ncollection-legacy\tacl-legacy\n' > "$test_dir/want-candidates.tsv"
cmp "$test_dir/want-candidates.tsv" "$test_dir/candidates.tsv"
grep -Fxq 'active_direct_acls=1' "$test_dir/counts.txt"
grep -Fxq 'uuid_role_acls=1' "$test_dir/counts.txt"
grep -Fxq 'legacy_role_name_acls=1' "$test_dir/counts.txt"
grep -Fxq 'dangling_subjects=0' "$test_dir/counts.txt"
test -z "$(find "$test_dir/tmp" -mindepth 1 -maxdepth 1 -print -quit)"

printf 'collection_id\tacl_id\treplacement_role_id\treview_status\ncollection-legacy\tacl-legacy\t33333333-3333-3333-3333-333333333333\tapproved\n' > "$test_dir/replacements.tsv"
CANDIDATE_MANIFEST="$test_dir/candidates.tsv" \
  REVIEWED_REPLACEMENTS="$test_dir/replacements.tsv" \
  bash "$test_dir/validate-reset.sh" > "$test_dir/validated.txt"
grep -Fxq 'validated_reset_rows=1' "$test_dir/validated.txt"

expect_rejected() {
  local name=$1
  if CANDIDATE_MANIFEST="$test_dir/candidates.tsv" \
    REVIEWED_REPLACEMENTS="$test_dir/replacements.tsv" \
    bash "$test_dir/validate-reset.sh" \
    > "$test_dir/$name.out" 2> "$test_dir/$name.err"; then
    echo "$name fixture was not rejected" >&2
    exit 1
  fi
  test ! -s "$test_dir/$name.out"
}

printf 'collection_id\tacl_id\treplacement_role_id\treview_status\ncollection-legacy\tacl-legacy\tnot-a-uuid\tapproved\n' > "$test_dir/replacements.tsv"
expect_rejected malformed

printf 'collection_id\tacl_id\treplacement_role_id\treview_status\ncollection-other\tacl-other\t33333333-3333-3333-3333-333333333333\tapproved\n' > "$test_dir/replacements.tsv"
expect_rejected dangling

printf 'collection_id\tacl_id\treplacement_role_id\treview_status\ncollection-legacy\tacl-legacy\t33333333-3333-3333-3333-333333333333\tpending\n' > "$test_dir/replacements.tsv"
expect_rejected unreviewed
