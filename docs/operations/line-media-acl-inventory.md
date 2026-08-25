# LINE media ACL inventory

This procedure is a production stop gate, not a migration. Run it only after
explicit approval with two approved read-only PostgreSQL service definitions:
one for Account and one for Asset. The inventory never changes data. A later
reset requires a separately reviewed replacement file and uses only the
existing management API.

The exports contain only opaque IDs:

- Account: active user UUIDs, role UUIDs, and current active-user membership
  pairs.
- Asset temporary export: live collection IDs, active ACL IDs, subject types,
  and subject IDs.
- Candidate manifest on standard output: `collection_id` and `acl_id` only for
  active read ACLs whose subject type is `role` and whose subject ID is not a
  UUID.

They contain no email, display name, content metadata, credential, or token.
The four inventory counts go to standard error so standard output remains the
exact opaque candidate manifest.

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
   separate shell sessions or source it into an interactive shell. Capture
   standard output to a private `.partial` candidate file, and rename it to the
   reviewed filename only when the process exits zero. Remove it on any error.

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


def valid_opaque_id(value):
    return (
        value == value.strip()
        and 0 < len(value.encode('utf-8')) <= 255
        and not any(ord(character) < 32 or ord(character) == 127 for character in value)
    )


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
candidates = []
candidate_keys = set()

for acl in rows(
    'asset-active-acls.tsv',
    ['collection_id', 'acl_id', 'subject_type', 'subject_id'],
):
    collection_id = acl['collection_id']
    acl_id = acl['acl_id']
    subject_type = acl['subject_type']
    subject_id = acl['subject_id']
    if not all(valid_opaque_id(value) for value in (collection_id, acl_id, subject_id)):
        raise SystemExit('invalid active ACL identifier')
    if subject_type == 'user':
        active_direct_acls += 1
        dangling_subjects += subject_id not in account_users
    elif subject_type == 'role':
        if has_uuid_syntax(subject_id):
            uuid_role_acls += 1
            dangling_subjects += subject_id not in account_roles
        else:
            legacy_role_name_acls += 1
            key = (collection_id, acl_id)
            if key in candidate_keys:
                raise SystemExit('duplicate legacy ACL candidate')
            candidate_keys.add(key)
            candidates.append(key)
    else:
        raise SystemExit('unexpected active ACL subject type')

print(f'active_direct_acls={active_direct_acls}', file=sys.stderr)
print(f'uuid_role_acls={uuid_role_acls}', file=sys.stderr)
print(f'legacy_role_name_acls={legacy_role_name_acls}', file=sys.stderr)
print(f'dangling_subjects={dangling_subjects}', file=sys.stderr)

writer = csv.writer(sys.stdout, delimiter='\t', lineterminator='\n')
writer.writerow(['collection_id', 'acl_id'])
writer.writerows(candidates)
PY
```

The four reported classes are:

- `active_direct_acls`: every active `user` ACL on a live collection.
- `uuid_role_acls`: every active `role` ACL whose subject has UUID syntax.
- `legacy_role_name_acls`: every active `role` ACL whose subject is not a UUID.
- `dangling_subjects`: direct ACLs that do not match an active Account user,
  plus UUID role ACLs that do not match an Account role. Legacy role-name ACLs
  are reported separately and are not double-counted as dangling.

The local join validates current membership pairs and every candidate key
before emitting any row. If Account membership or ACL state changes during the
run, discard the counts and candidate manifest and rerun. The raw subject IDs
exist only in the temporary export removed by the cleanup trap; they never
enter the candidate manifest.

## Review and validate replacement UUIDs

Create a separate private TSV file with this exact header:

```text
collection_id	acl_id	replacement_role_id	review_status
```

There must be exactly one row for every candidate and no other row.
`replacement_role_id` must be the current Account role UUID explicitly chosen
through the Account ACL-subject search. Record `approved` only after the
candidate and UUID are reviewed in the approved operational channel. Never
derive or auto-fill a replacement UUID from the legacy role name. Immediately
before mutation, repeat the exact-ID Account lookup and stop if it no longer
returns that role.

Run this complete validation block with the two approved private file paths.
It emits only a validated row count and exits nonzero before emitting output
for a malformed file, duplicate, missing or extra candidate mapping,
non-canonical role UUID, or a row not marked `approved`.

```bash
#!/usr/bin/env bash
set -euo pipefail
set +x

CANDIDATE_MANIFEST="${CANDIDATE_MANIFEST:-APPROVED_CANDIDATE_MANIFEST_PATH}"
REVIEWED_REPLACEMENTS="${REVIEWED_REPLACEMENTS:-APPROVED_REPLACEMENTS_PATH}"

[[ "$CANDIDATE_MANIFEST" != 'APPROVED_CANDIDATE_MANIFEST_PATH' ]]
[[ "$REVIEWED_REPLACEMENTS" != 'APPROVED_REPLACEMENTS_PATH' ]]

python3 - "$CANDIDATE_MANIFEST" "$REVIEWED_REPLACEMENTS" <<'PY'
import csv
import sys
import uuid
from pathlib import Path

candidate_path = Path(sys.argv[1])
replacement_path = Path(sys.argv[2])


def rows(path, fields):
    if not path.is_file() or path.is_symlink():
        raise SystemExit(f'missing regular input: {path.name}')
    with path.open(newline='', encoding='utf-8') as handle:
        reader = csv.DictReader(handle, delimiter='\t')
        if reader.fieldnames != fields:
            raise SystemExit(f'invalid header in {path.name}')
        yield from reader


def valid_opaque_id(value):
    return (
        isinstance(value, str)
        and value == value.strip()
        and 0 < len(value.encode('utf-8')) <= 255
        and not any(ord(character) < 32 or ord(character) == 127 for character in value)
    )


def canonical_uuid(value):
    try:
        return str(uuid.UUID(value)) == value
    except (AttributeError, ValueError):
        return False


candidate_fields = ['collection_id', 'acl_id']
replacement_fields = [
    'collection_id',
    'acl_id',
    'replacement_role_id',
    'review_status',
]

candidates = set()
for row in rows(candidate_path, candidate_fields):
    key = (row['collection_id'], row['acl_id'])
    if not all(valid_opaque_id(value) for value in key) or key in candidates:
        raise SystemExit('invalid or duplicate candidate')
    candidates.add(key)

replacements = set()
for row in rows(replacement_path, replacement_fields):
    key = (row['collection_id'], row['acl_id'])
    if (
        not all(valid_opaque_id(value) for value in key)
        or key in replacements
        or not canonical_uuid(row['replacement_role_id'])
        or row['review_status'] != 'approved'
    ):
        raise SystemExit('invalid, duplicate, or unreviewed replacement')
    replacements.add(key)

if replacements != candidates:
    raise SystemExit('replacement rows do not exactly match candidates')

print(f'validated_reset_rows={len(candidates)}')
PY
```

## Audited reset through the management API

Proceed only after the validator exits zero and the exact-ID Account lookup is
still current. For each validated row, use an approved authenticated client for
the existing Admin management API:

1. `POST /api/line/media-sync/collections/{collectionID}/acl` with a unique
   idempotency key and body
   `{"subjectType":"role","subjectId":"<replacement_role_id>"}`. Require a
   `201` response whose active ACL has the reviewed collection and subject UUID.
2. After the replacement is confirmed active, revoke the legacy ACL with
   `DELETE /api/line/media-sync/collections/{collectionID}/acl/{aclID}`, a new
   idempotency key, and the candidate `acl_id`. Require a `200` response for
   that ACL record.
3. Re-read the collection and require the replacement ACL to be active and the
   candidate ACL to be revoked. Stop on any drift or response mismatch.

Adding first avoids an access gap; if revoke fails, leave the replacement in
place and retry only the idempotent revoke after review. These API mutations
record the manager actor, request, and idempotency context in ACL audit history.
Never update `subject_id`, physically delete ACL or audit rows, run `TRUNCATE`,
or reuse a revoked ACL row. Revocation is a soft state change, and all prior
audit history remains retained.

## Maintainer synthetic checks

Before approving changes to this procedure, extract the complete Bash block to
a temporary script and use fake `psql` commands with dummy service names; never
use a live database for these checks. Verify all of the following:

1. A successful fake Account and Asset export produces the exact two-column
   legacy candidate manifest and four expected counts, exits zero, and leaves
   no inventory temporary directory.
2. A fake `psql` that writes a partial export and exits nonzero causes the unit
   to exit nonzero, prints no count, never invokes the local join, and removes
   the inventory temporary directory.
3. A fake `psql` that interrupts the unit after writing a partial export causes
   a nonzero signal exit, prints no count, never invokes the local join, and
   removes the inventory temporary directory.
4. Run `scripts/test-line-media-acl-inventory.sh`; its real validator fixtures
   must accept the exact reviewed mapping and reject malformed, dangling, and
   unreviewed mappings without output.

Stop here unless the inventory, separate mapping review, exact-ID Account
lookup, and API reset are each explicitly approved. Do not run a production
migration, physical delete, `TRUNCATE`, push, branch merge, or deployment from
this procedure.
