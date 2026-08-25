#!/bin/sh
set -eu

tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

printf '%s\n' 'DROP INDEX IF EXISTS old_index;' >"$tmp/safe.sql"
./scripts/test-migration-policy.sh "$tmp/safe.sql"
printf '%s\n' "ALTER TABLE asset_content_tickets ADD COLUMN role_ids text[] NOT NULL DEFAULT '{}'::text[];" >"$tmp/expand.sql"
./scripts/test-migration-policy.sh "$tmp/expand.sql"
printf '%s\n' 'REVOKE UPDATE, DELETE, TRUNCATE ON asset_collection_acl_audit FROM asset;' >"$tmp/revoke.sql"
./scripts/test-migration-policy.sh "$tmp/revoke.sql"
./scripts/test-migration-policy.sh internal/migrations/sql/016_drop_legacy_ticket_roles.sql
printf '%s\n' 'REVOKE TRUNCATE, UPDATE, DELETE ON asset_collection_acl_audit FROM asset;' >"$tmp/revoke.sql"
./scripts/test-migration-policy.sh "$tmp/revoke.sql"

for statement in \
  'DROP TABLE users;' \
  'TRUNCATE TABLE asset_collection_acl_audit;' \
  'DO $$ BEGIN TRUNCATE TABLE asset_collection_acl_audit; END $$;' \
  'TRUNCATE/* bypass */TABLE asset_collection_acl_audit;' \
  'ALTER TABLE users ALTER COLUMN name SET NOT NULL;' \
  'ALTER TABLE asset_content_tickets RENAME COLUMN roles TO role_ids;' \
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
