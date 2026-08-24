#!/bin/sh
set -eu

pattern='DROP[[:space:]]+(SCHEMA|TABLE|COLUMN|VIEW|MATERIALIZED[[:space:]]+VIEW|TYPE|FUNCTION|CONSTRAINT)|RENAME[[:space:]]+(TABLE|COLUMN)|ALTER[[:space:]]+TABLE[^;]*(ALTER[[:space:]]+COLUMN[^;]*TYPE|DROP[[:space:]]+CONSTRAINT|SET[[:space:]]+NOT[[:space:]]+NULL)'
legacy_pattern='DROP[[:space:]]+(SCHEMA|TABLE|COLUMN|VIEW|MATERIALIZED[[:space:]]+VIEW|TYPE|FUNCTION)|RENAME[[:space:]]+(TABLE|COLUMN)|ALTER[[:space:]]+TABLE[^;]*(ALTER[[:space:]]+COLUMN[^;]*TYPE|SET[[:space:]]+NOT[[:space:]]+NULL)'

if [ "$#" -eq 0 ]; then
  set -- internal/migrations/sql/*.sql
fi

for file in "$@"; do
  current_pattern="$pattern"
  legacy_hash=''
  case "$file" in
    internal/migrations/sql/004_policy_and_idempotency.sql)
      legacy_hash='70538d31bad777f9bac3cc563e241198e0624ac291f66f79bfb5d9046390452f'
      ;;
    internal/migrations/sql/007_processing_retry_and_retention.sql)
      legacy_hash='a4916f5d3c4d0799f45d3ba410fd79cb49c902e3f433febfaf5a110a8771788f'
      ;;
  esac
  if [ -n "$legacy_hash" ]; then
    if command -v sha256sum >/dev/null 2>&1; then
      current_hash="$(sha256sum "$file" | cut -d ' ' -f 1)"
    else
      current_hash="$(shasum -a 256 "$file" | cut -d ' ' -f 1)"
    fi
    [ "$current_hash" = "$legacy_hash" ] || {
      echo "$file is immutable; add a new migration" >&2
      exit 1
    }
    current_pattern="$legacy_pattern"
  fi
  normalized="$(perl -0777 -pe 's{/\*.*?\*/}{ }gs; s/--[^\n]*//g' "$file" | tr '\n' ' ')"
  if printf '%s' "$normalized" | grep -Eiq "$current_pattern" || \
    printf '%s' "$normalized" | perl -0777 -e '$sql = <>; exit($sql =~ /\bTRUNCATE\s+(?:TABLE\s+)?(?!ON\b)/i ? 0 : 1)'; then
    echo 'migrations must use expand/contract; destructive DDL requires a later release' >&2
    exit 1
  fi
done
