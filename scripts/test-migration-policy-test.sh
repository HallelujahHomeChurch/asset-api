#!/bin/sh
set -eu

tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT
policy_script="$PWD/scripts/test-migration-policy.sh"
retention_migration="internal/migrations/sql/034_recording_retention.sql"
mkdir -p "$tmp/internal/migrations/sql"
cp "$retention_migration" "$tmp/$retention_migration"
(cd "$tmp" && "$policy_script" "$retention_migration")
printf '%s\n' '-- test mutation' >>"$tmp/$retention_migration"
if (cd "$tmp" && "$policy_script" "$retention_migration") 2>/dev/null; then
  echo 'recording retention CHECK replacement was not immutable' >&2
  exit 1
fi

live_cover_migration="internal/migrations/sql/038_recording_live_covers.sql"
cp "$live_cover_migration" "$tmp/$live_cover_migration"
(cd "$tmp" && "$policy_script" "$live_cover_migration")
printf '%s\n' '-- test mutation' >>"$tmp/$live_cover_migration"
if (cd "$tmp" && "$policy_script" "$live_cover_migration") 2>/dev/null; then
  echo 'live cover additive CHECK replacement was not immutable' >&2
  exit 1
fi

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
