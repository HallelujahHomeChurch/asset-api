#!/bin/sh
set -eu

tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

printf '%s\n' 'DROP INDEX IF EXISTS old_index;' >"$tmp/safe.sql"
./scripts/test-migration-policy.sh "$tmp/safe.sql"

for statement in \
  'DROP TABLE users;' \
  'ALTER TABLE users ALTER COLUMN name SET NOT NULL;' \
  'DROP /* bypass */ TABLE users;' \
  'DROP VIEW current_assets;' \
  'ALTER TABLE users DROP CONSTRAINT users_name_key;'
do
  printf '%s\n' "$statement" >"$tmp/destructive.sql"
  if ./scripts/test-migration-policy.sh "$tmp/destructive.sql" 2>/dev/null; then
    echo "destructive migration was not rejected: $statement" >&2
    exit 1
  fi
done
