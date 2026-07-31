#!/usr/bin/env bash
set -euo pipefail

env_file="${HHC_ENV_FILE:-/Users/rayselfs/Projects/hhc/.env.json}"
host="${ASSET_DB_HOST:-172.16.68.4}"
port="${ASSET_DB_PORT:-5432}"
database="${ASSET_DB_NAME:-asset}"
admin_user="${ASSET_DB_ADMIN_USER:-HHCAdmin}"
sslmode="${ASSET_DB_SSLMODE:-require}"
runtime_vault="${ASSET_RUNTIME_KEY_VAULT:-alive-asset-runtime-kv}"
migration_vault="${ASSET_MIGRATION_KEY_VAULT:-alive-asset-migrate-kv}"
secret_file=""
trap 'rm -f "${secret_file:-}"' EXIT

for command in jq openssl; do
  command -v "$command" >/dev/null || { echo "$command is required" >&2; exit 1; }
done
[[ -f "$env_file" ]] || { echo "environment file not found: $env_file" >&2; exit 1; }
chmod 0600 "$env_file"

echo "host=$host"
echo "database=$database"
echo "migration-role=asset_migrate"
echo "runtime-role=asset"
echo "runtime-key-vault=$runtime_vault"
echo "migration-key-vault=$migration_vault"

if [[ "${ASSET_BOOTSTRAP_DRY_RUN:-0}" == "1" ]]; then
  exit 0
fi

psql_bin="$(command -v psql || true)"
[[ -n "$psql_bin" ]] || psql_bin="/opt/homebrew/opt/libpq/bin/psql"
[[ -x "$psql_bin" ]] || { echo "psql is required" >&2; exit 1; }

admin_password="$(jq -er '.PG_ADMIN_PASSWORD' "$env_file")"
runtime_password="$(jq -er '.ASSET_DB_PASSWORD' "$env_file")"
runtime_dsn="postgres://asset:${runtime_password}@${host}:${port}/${database}?sslmode=${sslmode}"
migration_password=""
if [[ "${ASSET_SKIP_KEY_VAULT:-0}" == "1" ]]; then
  migration_password="$(jq -er '.ASSET_MIGRATE_DB_PASSWORD' "$env_file")"
else
  command -v az >/dev/null || { echo "az is required" >&2; exit 1; }
  secret_file="$(mktemp)"
  chmod 0600 "$secret_file"
  printf '%s' "$runtime_dsn" >"$secret_file"
  az keyvault secret set --vault-name "$runtime_vault" --name database-url --file "$secret_file" --content-type text/plain --only-show-errors --output none

  migration_dsn="$(az keyvault secret show --vault-name "$migration_vault" --name database-url --query value -o tsv --only-show-errors 2>/dev/null || true)"
  if [[ -n "$migration_dsn" ]]; then
    prefix="postgres://asset_migrate:"
    suffix="@${host}:${port}/${database}?sslmode=${sslmode}"
    migration_password="${migration_dsn#"$prefix"}"
    migration_password="${migration_password%"$suffix"}"
    [[ "$migration_dsn" == "$prefix"*"$suffix" && "$migration_password" =~ ^[0-9a-f]{48}$ ]] || {
      echo "migration database URL has an unexpected format" >&2
      exit 1
    }
  else
    migration_password="$(openssl rand -hex 24)"
    migration_dsn="postgres://asset_migrate:${migration_password}@${host}:${port}/${database}?sslmode=${sslmode}"
    printf '%s' "$migration_dsn" >"$secret_file"
    az keyvault secret set --vault-name "$migration_vault" --name database-url --file "$secret_file" --content-type text/plain --only-show-errors --output none
  fi
fi

export PGPASSWORD="$admin_password"
export ASSET_MIGRATE_DB_PASSWORD="$migration_password"
"$psql_bin" "host=$host port=$port dbname=$database user=$admin_user sslmode=$sslmode" \
  --set=ON_ERROR_STOP=1 \
  --set=admin_user="$admin_user" \
  --set=database="$database" <<'SQL'
\getenv migration_password ASSET_MIGRATE_DB_PASSWORD
BEGIN;
SET LOCAL lock_timeout = '5s';
SET LOCAL statement_timeout = '60s';
SELECT format('CREATE ROLE asset_migrate LOGIN PASSWORD %L', :'migration_password')
WHERE NOT EXISTS (SELECT FROM pg_catalog.pg_roles WHERE rolname = 'asset_migrate')
\gexec
ALTER ROLE asset_migrate LOGIN PASSWORD :'migration_password';
GRANT asset, asset_migrate TO :"admin_user";
REASSIGN OWNED BY asset TO asset_migrate;
ALTER DATABASE :"database" OWNER TO asset_migrate;
ALTER SCHEMA public OWNER TO asset_migrate;
CREATE TABLE IF NOT EXISTS public.schema_migrations (
  version text PRIMARY KEY,
  checksum text NOT NULL DEFAULT '',
  applied_at timestamptz NOT NULL DEFAULT now()
);
REVOKE CREATE ON SCHEMA public FROM PUBLIC, asset;
GRANT CONNECT ON DATABASE :"database" TO asset;
GRANT USAGE ON SCHEMA public TO asset;
GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA public TO asset;
GRANT USAGE, SELECT, UPDATE ON ALL SEQUENCES IN SCHEMA public TO asset;
ALTER DEFAULT PRIVILEGES FOR ROLE asset_migrate IN SCHEMA public
  GRANT SELECT, INSERT, UPDATE, DELETE ON TABLES TO asset;
ALTER DEFAULT PRIVILEGES FOR ROLE asset_migrate IN SCHEMA public
  GRANT USAGE, SELECT, UPDATE ON SEQUENCES TO asset;
REVOKE ALL PRIVILEGES ON TABLE public.schema_migrations FROM asset;
COMMIT;
SQL
unset PGPASSWORD ASSET_MIGRATE_DB_PASSWORD admin_password migration_password runtime_password

if [[ "${ASSET_SKIP_KEY_VAULT:-0}" != "1" ]]; then
  "$psql_bin" "$runtime_dsn" --set=ON_ERROR_STOP=1 -Atc 'SELECT 1' >/dev/null
  "$psql_bin" "$migration_dsn" --set=ON_ERROR_STOP=1 -Atc 'SELECT 1' >/dev/null
fi

echo "asset migration role and isolated Key Vault secrets are ready"
