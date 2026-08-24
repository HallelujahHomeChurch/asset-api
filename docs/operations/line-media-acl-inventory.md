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
3. Replace only these non-secret placeholders with the approved service names:

   ```sh
   ACCOUNT_INVENTORY_SERVICE='<APPROVED_ACCOUNT_READ_ONLY_PGSERVICE>'
   ASSET_INVENTORY_SERVICE='<APPROVED_ASSET_READ_ONLY_PGSERVICE>'
   ```

4. Disable shell tracing and create a private temporary directory:

   ```sh
   set +x
   umask 077
   inventory_dir="$(mktemp -d "${TMPDIR:-/tmp}/line-media-acl-inventory.XXXXXX")"
   ```

Do not continue if either service is not the approved read-only production
target. The commands also force every database session and transaction to be
read-only.

## Export Account IDs

Run from the private temporary directory. The three queries share one
repeatable-read snapshot.

```sh
(
  cd "$inventory_dir"
  PGSERVICE="$ACCOUNT_INVENTORY_SERVICE" \
    PGOPTIONS='-c default_transaction_read_only=on' \
    psql -X --no-psqlrc --set=ON_ERROR_STOP=1 <<'SQL'
BEGIN TRANSACTION ISOLATION LEVEL REPEATABLE READ READ ONLY;
SET LOCAL statement_timeout = '30s';
SET LOCAL lock_timeout = '5s';

\copy (SELECT id::text AS user_id FROM users WHERE is_active AND deleted_at IS NULL ORDER BY id) TO 'account-users.tsv' WITH (FORMAT csv, HEADER true, DELIMITER E'\t')

\copy (SELECT id::text AS role_id FROM roles ORDER BY id) TO 'account-roles.tsv' WITH (FORMAT csv, HEADER true, DELIMITER E'\t')

\copy (SELECT ur.user_id::text AS user_id, ur.role_id::text AS role_id FROM user_roles ur JOIN users u ON u.id = ur.user_id AND u.is_active AND u.deleted_at IS NULL JOIN roles r ON r.id = ur.role_id ORDER BY ur.user_id, ur.role_id) TO 'account-user-roles.tsv' WITH (FORMAT csv, HEADER true, DELIMITER E'\t')

COMMIT;
SQL
)
```

## Export Asset ACL IDs

This is a separate repeatable-read snapshot. It includes only active read ACLs
on live collections.

```sh
(
  cd "$inventory_dir"
  PGSERVICE="$ASSET_INVENTORY_SERVICE" \
    PGOPTIONS='-c default_transaction_read_only=on' \
    psql -X --no-psqlrc --set=ON_ERROR_STOP=1 <<'SQL'
BEGIN TRANSACTION ISOLATION LEVEL REPEATABLE READ READ ONLY;
SET LOCAL statement_timeout = '30s';
SET LOCAL lock_timeout = '5s';

\copy (SELECT acl.collection_id, acl.id AS acl_id, acl.subject_type, acl.subject_id FROM asset_collection_acl acl JOIN asset_collections c ON c.id = acl.collection_id AND c.deleted_at IS NULL WHERE acl.permission = 'read' AND acl.revoked_at IS NULL ORDER BY acl.collection_id, acl.id) TO 'asset-active-acls.tsv' WITH (FORMAT csv, HEADER true, DELIMITER E'\t')

COMMIT;
SQL
)
```

## Join locally and print counts

The four reported classes are defined as follows:

- `active_direct_acls`: every active `user` ACL on a live collection.
- `uuid_role_acls`: every active `role` ACL whose subject has UUID syntax.
- `legacy_role_name_acls`: every active `role` ACL whose subject is not a UUID.
- `dangling_subjects`: direct ACLs that do not match an active Account user,
  plus UUID role ACLs that do not match an Account role. Legacy role-name ACLs
  are reported separately and are not double-counted as dangling.

Run the join locally with the Python standard library. It validates the exact
export headers and current membership pairs, reads no other fields, and prints
only the four counts.

```sh
python3 - "$inventory_dir" <<'PY'
import csv
import sys
import uuid
from pathlib import Path

root = Path(sys.argv[1])


def rows(name, fields):
    with (root / name).open(newline='', encoding='utf-8') as handle:
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


account_users = ids('account-users.tsv', 'user_id')
account_roles = ids('account-roles.tsv', 'role_id')

for membership in rows('account-user-roles.tsv', ['user_id', 'role_id']):
    if membership['user_id'] not in account_users or membership['role_id'] not in account_roles:
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

Account and Asset are separate databases, so the two snapshots are not atomic
together. If Account membership or ACL state changes during the run, discard
the output and rerun. Counts and opaque exports require review before any later
test-data reset or ACL recreation approval.

## Cleanup

After the reviewed counts are recorded in the approved operational channel,
remove the four local exports and the empty temporary directory:

```sh
test -n "$inventory_dir" && test "$inventory_dir" != '/'
rm -- \
  "$inventory_dir/account-users.tsv" \
  "$inventory_dir/account-roles.tsv" \
  "$inventory_dir/account-user-roles.tsv" \
  "$inventory_dir/asset-active-acls.tsv"
rmdir -- "$inventory_dir"
unset ACCOUNT_INVENTORY_SERVICE ASSET_INVENTORY_SERVICE inventory_dir
```

Stop here. Do not run a production migration, delete or reactivate ACLs, push a
branch, open or merge a PR, or deploy from this procedure.
