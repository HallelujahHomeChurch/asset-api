# Asset API Account-artifact governance scope

Automated Account-artifact scope is only the two account.avatar and account.dsr-export namespaces and the rows mechanically linked through their asset_id values. The artifact subject is assets.owner_id; grant subject_id is a reader and is not the artifact owner. Grant expiry or revocation denies access but does not delete a Blob or a grant row.

The automated inventory covers Account asset metadata, upload sessions, grants, scan and derivative state, poison-event metadata, and the staged purge lifecycle. FK-linked scan, derivative, and outbox rows cascade when parent asset metadata is hard-deleted. Poison rows retain textual asset_id lookup only: they have no parent FK, so no cascade is claimed. Their attribution and retention remain manual and pending_legal.

## Adjacent shared/manual scope

asset_collections, asset_collection_items, asset_collection_acl, asset_collection_mutations, asset_content_tickets, and asset_collection_acl_audit are adjacent shared/manual scope. ACL subject_id identifies a reader; ticket user_id identifies a ticket user; mutation payloads are shared operation records; and audit actor_user_id identifies an operator. None makes every collection or media row an Account-owned artifact, so collection lifecycle and collection retention remain separate from the Account-artifact purge lifecycle.

Both collection-retention enablement flags remain false: retentionScheduleEnabled=false and retentionApplyEnabled=false.

Checksums, hashes, and public visibility do not anonymize an artifact or remove its Account-artifact classification.

schema_migrations.version, schema_migrations.checksum, and schema_migrations.applied_at are excluded operational metadata.
