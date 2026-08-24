# LINE media ACL inventory

This procedure is a production stop gate, not a migration. Run it only after
explicit approval with two approved read-only PostgreSQL service definitions:
one for Account and one for Asset. It never changes data and must not be used to
reset or recreate ACLs.

The exports contain only opaque IDs:

- Account: active user UUIDs, role UUIDs, and current active-user membership
  pairs.
- Asset: live collection IDs, active ACL IDs, subject types, and subject IDs.

They contain no email, display name, content metadata, credential, or token.
The final command prints counts only.

## Preconditions

1. Obtain approval for the inventory run and for the exact Account and Asset
   targets.
2. Configure approved read-only libpq services outside the repository. The
   service definitions may refer to an approved password file; do not paste a
   DSN or password into this document, the shell history, or an output file.
3. Replace only `APPROVED_ACCOUNT_READ_ONLY_PGSERVICE` and
   `APPROVED_ASSET_READ_ONLY_PGSERVICE` below with the approved non-secret
   service names. Do not add a DSN, credential, or token.
4. Run the complete block in a dedicated Bash process. Do not split it into
   separate shell sessions or source it into an interactive shell.

Do not continue if either service is not the approved read-only production
target. The commands also force every database session and transaction to be
read-only.

## Export, validate, join, and clean up

The unit below is fail-closed. Any temporary-directory, directory-change,
export, validation, rename, or join failure stops later steps. Exports use
partial names until both database commands succeed, and the local join requires
all four validated final inputs plus a completion marker. The `EXIT`, hangup,
interrupt, and termination paths remove only the exact validated temporary
directory and fixed inventory filenames created by this unit.

Account and Asset use separate repeatable-read snapshots. The Account queries
share one snapshot; Asset uses a second snapshot. All transactions are
explicitly read-only.

```bash
#!/usr/bin/env bash
set -euo pipefail
set +x
umask 077

ACCOUNT_INVENTORY_SERVICE='APPROVED_ACCOUNT_READ_ONLY_PGSERVICE'
ASSET_INVENTORY_SERVICE='APPROVED_ASSET_READ_ONLY_PGSERVICE'

: "${ACCOUNT_INVENTORY_SERVICE:?set the approved Account read-only PGSERVICE}"
: "${ASSET_INVENTORY_SERVICE:?set the approved Asset read-only PGSERVICE}"
[[ "$ACCOUNT_INVENTORY_SERVICE" != 'APPROVED_ACCOUNT_READ_ONLY_PGSERVICE' ]]
[[ "$ASSET_INVENTORY_SERVICE" != 'APPROVED_ASSET_READ_ONLY_PGSERVICE' ]]

inventory_dir=''
inventory_created=0
inventory_parent=''
inventory_prefix=''

is_valid_inventory_dir() {
  [[ "$inventory_created" -eq 1 ]]
  [[ -n "$inventory_dir" && -d "$inventory_dir" && ! -L "$inventory_dir" ]]
  [[ "$inventory_dir" == "$inventory_prefix".* ]]
}

cleanup() {
  local status=$?
  local cleanup_status=0
  trap - EXIT HUP INT TERM

  if [[ "$inventory_created" -eq 1 ]]; then
    if is_valid_inventory_dir; then
      rm -f -- \
        "$inventory_dir/account-users.tsv.partial" \
        "$inventory_dir/account-roles.tsv.partial" \
        "$inventory_dir/account-user-roles.tsv.partial" \
        "$inventory_dir/asset-active-acls.tsv.partial" \
        "$inventory_dir/account-users.tsv" \
        "$inventory_dir/account-roles.tsv" \
        "$inventory_dir/account-user-roles.tsv" \
        "$inventory_dir/asset-active-acls.tsv" \
        "$inventory_dir/.exports-complete" || cleanup_status=1
      rmdir -- "$inventory_dir" || cleanup_status=1
    else
      cleanup_status=1
    fi
  fi

  if [[ "$status" -eq 0 && "$cleanup_status" -ne 0 ]]; then
    status=$cleanup_status
  fi
  exit "$status"
}

trap cleanup EXIT
trap 'exit 129' HUP
trap 'exit 130' INT
trap 'exit 143' TERM

inventory_parent="$(cd -- "${TMPDIR:-/tmp}" && pwd -P)"
inventory_prefix="${inventory_parent%/}/line-media-acl-inventory"
inventory_dir="$(mktemp -d "${inventory_prefix}.XXXXXX")"
[[ -n "$inventory_dir" && -d "$inventory_dir" && ! -L "$inventory_dir" ]]
[[ "$inventory_dir" == "$inventory_prefix".* ]]
inventory_created=1
cd -- "$inventory_dir"
[[ "$(pwd -P)" == "$inventory_dir" ]]

PGSERVICE="$ACCOUNT_INVENTORY_SERVICE" \
  PGOPTIONS='-c default_transaction_read_only=on' \
  psql -X --no-psqlrc --quiet --set=ON_ERROR_STOP=1 <<'ACCOUNT_SQL'
BEGIN TRANSACTION ISOLATION LEVEL REPEATABLE READ READ ONLY;
SET LOCAL statement_timeout = '30s';
SET LOCAL lock_timeout = '5s';

\copy (SELECT id::text AS user_id FROM users WHERE is_active AND deleted_at IS NULL ORDER BY id) TO 'account-users.tsv.partial' WITH (FORMAT csv, HEADER true, DELIMITER E'\t')

\copy (SELECT id::text AS role_id FROM roles ORDER BY id) TO 'account-roles.tsv.partial' WITH (FORMAT csv, HEADER true, DELIMITER E'\t')

\copy (SELECT ur.user_id::text AS user_id, ur.role_id::text AS role_id FROM user_roles ur JOIN users u ON u.id = ur.user_id AND u.is_active AND u.deleted_at IS NULL JOIN roles r ON r.id = ur.role_id ORDER BY ur.user_id, ur.role_id) TO 'account-user-roles.tsv.partial' WITH (FORMAT csv, HEADER true, DELIMITER E'\t')

COMMIT;
ACCOUNT_SQL

PGSERVICE="$ASSET_INVENTORY_SERVICE" \
  PGOPTIONS='-c default_transaction_read_only=on' \
  psql -X --no-psqlrc --quiet --set=ON_ERROR_STOP=1 <<'ASSET_SQL'
BEGIN TRANSACTION ISOLATION LEVEL REPEATABLE READ READ ONLY;
SET LOCAL statement_timeout = '30s';
SET LOCAL lock_timeout = '5s';

\copy (SELECT acl.collection_id, acl.id AS acl_id, acl.subject_type, acl.subject_id FROM asset_collection_acl acl JOIN asset_collections c ON c.id = acl.collection_id AND c.deleted_at IS NULL WHERE acl.permission = 'read' AND acl.revoked_at IS NULL ORDER BY acl.collection_id, acl.id) TO 'asset-active-acls.tsv.partial' WITH (FORMAT csv, HEADER true, DELIMITER E'\t')

COMMIT;
ASSET_SQL

validate_export() {
  local file=$1
  local expected_header=$2
  local actual_header=''

  [[ -f "$file" && ! -L "$file" ]]
  IFS= read -r actual_header < "$file"
  [[ "$actual_header" == "$expected_header" ]]
}

validate_export 'account-users.tsv.partial' 'user_id'
validate_export 'account-roles.tsv.partial' 'role_id'
validate_export 'account-user-roles.tsv.partial' $'user_id\trole_id'
validate_export 'asset-active-acls.tsv.partial' \
  $'collection_id\tacl_id\tsubject_type\tsubject_id'

mv -- 'account-users.tsv.partial' 'account-users.tsv'
mv -- 'account-roles.tsv.partial' 'account-roles.tsv'
mv -- 'account-user-roles.tsv.partial' 'account-user-roles.tsv'
mv -- 'asset-active-acls.tsv.partial' 'asset-active-acls.tsv'
: > '.exports-complete'

[[ -f '.exports-complete' && ! -L '.exports-complete' ]]
validate_export 'account-users.tsv' 'user_id'
validate_export 'account-roles.tsv' 'role_id'
validate_export 'account-user-roles.tsv' $'user_id\trole_id'
validate_export 'asset-active-acls.tsv' \
  $'collection_id\tacl_id\tsubject_type\tsubject_id'

python3 - "$inventory_dir" <<'PY'
import csv
import sys
import uuid
from pathlib import Path

root = Path(sys.argv[1])


def rows(name, fields):
    path = root / name
    if not path.is_file() or path.is_symlink():
        raise SystemExit(f'missing validated export: {name}')
    with path.open(newline='', encoding='utf-8') as handle:
        reader = csv.DictReader(handle, delimiter='\t')
        if reader.fieldnames != fields:
            raise SystemExit(f'invalid header in {name}')
        yield from reader


def ids(name, field):
    return {row[field] for row in rows(name, [field])}


def has_uuid_syntax(value):
    try:
        return str(uuid.UUID(value)) == value.lower()
    except ValueError:
        return False


if not (root / '.exports-complete').is_file():
    raise SystemExit('exports are incomplete')

account_users = ids('account-users.tsv', 'user_id')
account_roles = ids('account-roles.tsv', 'role_id')

for membership in rows('account-user-roles.tsv', ['user_id', 'role_id']):
    if (
        membership['user_id'] not in account_users
        or membership['role_id'] not in account_roles
    ):
        raise SystemExit('invalid current Account membership export')

active_direct_acls = 0
uuid_role_acls = 0
legacy_role_name_acls = 0
dangling_subjects = 0

for acl in rows(
    'asset-active-acls.tsv',
    ['collection_id', 'acl_id', 'subject_type', 'subject_id'],
):
    subject_type = acl['subject_type']
    subject_id = acl['subject_id']
    if subject_type == 'user':
        active_direct_acls += 1
        dangling_subjects += subject_id not in account_users
    elif subject_type == 'role':
        if has_uuid_syntax(subject_id):
            uuid_role_acls += 1
            dangling_subjects += subject_id not in account_roles
        else:
            legacy_role_name_acls += 1
    else:
        raise SystemExit('unexpected active ACL subject type')

print(f'active_direct_acls={active_direct_acls}')
print(f'uuid_role_acls={uuid_role_acls}')
print(f'legacy_role_name_acls={legacy_role_name_acls}')
print(f'dangling_subjects={dangling_subjects}')
PY
```

The four reported classes are:

- `active_direct_acls`: every active `user` ACL on a live collection.
- `uuid_role_acls`: every active `role` ACL whose subject has UUID syntax.
- `legacy_role_name_acls`: every active `role` ACL whose subject is not a UUID.
- `dangling_subjects`: direct ACLs that do not match an active Account user,
  plus UUID role ACLs that do not match an Account role. Legacy role-name ACLs
  are reported separately and are not double-counted as dangling.

The local join validates current membership pairs but prints only these four
counts. If Account membership or ACL state changes during the run, discard the
output and rerun. Counts and opaque exports require review before any later
test-data reset or ACL recreation approval.

## Maintainer synthetic checks

Before approving changes to this procedure, extract the complete Bash block to
a temporary script and use fake `psql` commands with dummy service names; never
use a live database for these checks. Verify all of the following:

1. A successful fake Account and Asset export produces the four expected
   counts, exits zero, and leaves no inventory temporary directory.
2. A fake `psql` that writes a partial export and exits nonzero causes the unit
   to exit nonzero, prints no count, never invokes the local join, and removes
   the inventory temporary directory.
3. A fake `psql` that interrupts the unit after writing a partial export causes
   a nonzero signal exit, prints no count, never invokes the local join, and
   removes the inventory temporary directory.

Stop here. Do not run a production migration, delete or reactivate ACLs, push a
branch, open or merge a PR, or deploy from this procedure.
