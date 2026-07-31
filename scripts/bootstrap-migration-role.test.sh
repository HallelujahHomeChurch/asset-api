#!/usr/bin/env bash
set -euo pipefail

fixture="$(mktemp)"
psql_bin="$(command -v psql || true)"
[[ -n "$psql_bin" ]] || psql_bin="/opt/homebrew/opt/libpq/bin/psql"
admin_dsn="${TEST_POSTGRES_DSN:-}"
test_port="${TEST_POSTGRES_PORT:-5432}"
cleanup() {
  rm -f "$fixture"
  if [[ -n "$admin_dsn" ]]; then
    "$psql_bin" "$admin_dsn" --set=ON_ERROR_STOP=1 <<'SQL' >/dev/null
DROP DATABASE IF EXISTS asset_bootstrap_test WITH (FORCE);
DROP ROLE IF EXISTS asset_migrate;
DROP ROLE IF EXISTS asset;
DROP ROLE IF EXISTS bootstrap_admin;
SQL
  fi
}
trap cleanup EXIT
printf '{"PG_ADMIN_PASSWORD":"admin-secret"}\n' >"$fixture"

output="$(HHC_ENV_FILE="$fixture" ASSET_BOOTSTRAP_DRY_RUN=1 ./scripts/bootstrap-migration-role.sh)"
grep -q '^database=asset$' <<<"$output"
grep -q '^migration-role=asset_migrate$' <<<"$output"
grep -q '^runtime-role=asset$' <<<"$output"
grep -q '^runtime-key-vault=alive-asset-runtime-kv$' <<<"$output"
grep -q '^migration-key-vault=alive-asset-migrate-kv$' <<<"$output"

if [[ -z "$admin_dsn" ]]; then
  exit 0
fi
[[ "${ASSET_ALLOW_DESTRUCTIVE_TEST:-0}" == "1" ]] || {
  echo "set ASSET_ALLOW_DESTRUCTIVE_TEST=1 for the disposable PostgreSQL test" >&2
  exit 1
}
case "$admin_dsn" in
  postgres://*@localhost:*/*|postgres://*@127.0.0.1:*/*) ;;
  *) echo "TEST_POSTGRES_DSN must target disposable localhost PostgreSQL" >&2; exit 1 ;;
esac

"$psql_bin" "$admin_dsn" --set=ON_ERROR_STOP=1 <<'SQL' >/dev/null
DROP DATABASE IF EXISTS asset_bootstrap_test WITH (FORCE);
DROP ROLE IF EXISTS asset_migrate;
DROP ROLE IF EXISTS asset;
DROP ROLE IF EXISTS bootstrap_admin;
CREATE ROLE bootstrap_admin LOGIN CREATEROLE CREATEDB PASSWORD 'bootstrap-password';
CREATE ROLE asset LOGIN PASSWORD 'runtime-password';
GRANT asset TO bootstrap_admin WITH ADMIN OPTION;
CREATE DATABASE asset_bootstrap_test OWNER asset;
SQL

"$psql_bin" "$admin_dsn" --set=ON_ERROR_STOP=1 <<'SQL' >/dev/null
\connect asset_bootstrap_test
ALTER SCHEMA public OWNER TO asset;
SET ROLE asset;
CREATE TABLE public.owned_by_runtime(id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY);
RESET ROLE;
SQL

printf '{"PG_ADMIN_PASSWORD":"bootstrap-password","ASSET_DB_PASSWORD":"runtime-password","ASSET_MIGRATE_DB_PASSWORD":"migration-password"}\n' >"$fixture"
for _ in 1 2; do
  HHC_ENV_FILE="$fixture" \
  ASSET_DB_HOST=127.0.0.1 \
  ASSET_DB_PORT="$test_port" \
  ASSET_DB_NAME=asset_bootstrap_test \
  ASSET_DB_ADMIN_USER=bootstrap_admin \
  ASSET_DB_SSLMODE=disable \
  ASSET_SKIP_KEY_VAULT=1 \
  ./scripts/bootstrap-migration-role.sh >/dev/null
done

verification="$("$psql_bin" "$admin_dsn" -At <<'SQL'
\connect asset_bootstrap_test
SELECT schema_owner FROM information_schema.schemata WHERE schema_name='public';
SELECT tableowner FROM pg_tables WHERE schemaname='public' AND tablename='owned_by_runtime';
SELECT has_table_privilege('asset','public.owned_by_runtime','INSERT');
SELECT has_schema_privilege('asset','public','CREATE');
SELECT has_table_privilege('asset','public.schema_migrations','UPDATE');
SQL
)"
verification="$(tail -n 5 <<<"$verification")"
[[ "$verification" == $'asset_migrate\nasset_migrate\nt\nf\nf' ]]

PGPASSWORD=runtime-password "$psql_bin" \
  "postgres://asset@127.0.0.1:${test_port}/asset_bootstrap_test?sslmode=disable" \
  --set=ON_ERROR_STOP=1 -c 'INSERT INTO public.owned_by_runtime DEFAULT VALUES' >/dev/null
if PGPASSWORD=runtime-password "$psql_bin" \
  "postgres://asset@127.0.0.1:${test_port}/asset_bootstrap_test?sslmode=disable" \
  --set=ON_ERROR_STOP=1 -c 'CREATE TABLE public.forbidden(id bigint)' >/dev/null 2>&1; then
  echo "runtime role unexpectedly created a table" >&2
  exit 1
fi
if PGPASSWORD=runtime-password "$psql_bin" \
  "postgres://asset@127.0.0.1:${test_port}/asset_bootstrap_test?sslmode=disable" \
  --set=ON_ERROR_STOP=1 -c "INSERT INTO public.schema_migrations(version) VALUES('forbidden')" >/dev/null 2>&1; then
  echo "runtime role unexpectedly modified migration history" >&2
  exit 1
fi
