# LINE Media ACL Authority Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (- [ ]) syntax for tracking.

**Goal:** Replace the global LINE media viewer entitlement with authenticated collection ACL authorization using immutable user and role UUIDs.

**Architecture:** Account produces immutable role_ids; Gateway validates and forwards them; Asset is the only reader authorization authority through collection ACLs. The LINE management facade validates ACL recipients and propagates the manager actor, while LibrePresenter treats scoped 404 as remote collection unavailability. Expand/contract commits keep every intermediate deployed combination deny-closed.

**Tech Stack:** Go 1.25, Gin, GORM, PostgreSQL, Nginx, TypeScript, Fastify, React, Vitest, Electron

**Spec:** docs/superpowers/specs/2026-08-24-line-media-acl-authority-design.md

## Global Constraints

- Work in a separate worktree and codex/* branch for every repository; never modify or commit directly to main.
- Do not add a new service, authorization library, online JWT introspection, or dual-read role-name compatibility layer.
- Preserve Gateway JWT verification, trusted-header stripping/injection, exact Dapr caller checks, private-route rejection, short ticket TTL, and live ticket ACL recheck.
- Keep media_sync_manager -> media-sync:manage; manager does not inherit reader access.
- Final reader authority is active collection ACL only; final runtime must not require media_sync_user or media-sync:read.
- Role ACL subjects are immutable Account role UUIDs; role names are display metadata only.
- List-without-ACL is 200 plus an empty array; scoped missing and unauthorized resources are both 404.
- No production data mutation, push, PR, merge, or deployment is authorized by this local execution plan.
- Existing test data is not automatically deleted or rebound. Produce an inventory command and reviewed output before any later data reset.
- Use TDD for every behavior change: add the smallest failing test, observe the expected failure, implement the minimum change, then rerun focused and repository gates.

---

### Task 1: Account producer contract - immutable role IDs

**Repository:** website/account-api

**Branch:** codex/media-acl-role-ids

**Files:**
- Modify: internal/services/token_service.go
- Modify: internal/services/token_service_test.go
- Modify: internal/repository/rbac_repo.go
- Modify: internal/repository/rbac_repo_integration_test.go
- Modify: internal/services/rbac_service_integration_test.go
- Modify: docs/openapi.yaml

**Interfaces:**
- Produces JWT claim role_ids: string[] containing exactly the token's role memberships; claim order is not an authorization contract.
- Produces ACL subject DTO { id: role UUID, type: "role", displayName: role name }.
- Retains the roles claim and viewer role/permission during this expand step.

- [ ] **Step 1: Add a failing access-token claim test**

Create a user with two literal role UUIDs. Parse the signed access token and assert both role names and role IDs are present:

~~~go
assert.Equal(t, []any{"media_sync_manager", "worship"}, claims["roles"])
assert.Equal(t, []any{
    "018f0000-0000-7000-8000-000000000001",
    "018f0000-0000-7000-8000-000000000002",
}, claims["role_ids"])
~~~

- [ ] **Step 2: Run the token test and observe RED**

Run: go test ./internal/services -run 'Test.*AccessToken.*RoleIDs' -count=1

Expected: FAIL because role_ids is absent.

- [ ] **Step 3: Emit role IDs**

In signAccessToken, collect role.ID.String() beside the existing role-name collection and add role_ids to jwt.MapClaims. Do not remove or reorder the existing roles claim.

- [ ] **Step 4: Add a failing role-subject search test**

Change the integration expectation so a role result uses its UUID as ID and its name as DisplayName; search must match either name or exact UUID.

~~~go
assert.Equal(t, []ACLSubjectRecord{{
    ID: roles[2].ID.String(), Type: "role", DisplayName: "media-sync-manager",
}}, matching)
~~~

Run: go test ./internal/repository -run TestSearchMediaSyncACLSubjectsUsesImmutableRoleIDs -count=1

Expected: FAIL because the query returns the mutable role name as id.

- [ ] **Step 5: Return immutable role IDs**

Project roles.id::text AS id, retain roles.name AS display_name, and search with role name or roles.id::text using the existing escaped pattern.

- [ ] **Step 6: Verify Account producer gates**

Run:

~~~bash
go test ./internal/repository ./internal/services -count=1
go test ./... -count=1
go vet ./...
go build ./cmd/...
git diff --check
~~~

Expected: all pass.

- [ ] **Step 7: Update OpenAPI and commit**

Document role_ids and UUID role subjects. Commit:

~~~bash
git add internal/services/token_service.go internal/services/token_service_test.go \
  internal/repository/rbac_repo.go internal/repository/rbac_repo_integration_test.go \
  internal/services/rbac_service_integration_test.go docs/openapi.yaml
git commit -m "feat: expose immutable role ids for media ACLs"
~~~

---

### Task 2: Gateway producer contract - trusted role-ID headers

**Repository:** website/api-gateway

**Branch:** codex/media-acl-role-ids

**Files:**
- Modify: internal/verifier/token.go
- Modify: internal/verifier/handler.go
- Modify: internal/verifier/verifier_test.go
- Modify: conf.d/common/proxy.conf
- Modify: conf.d/common/protected.conf
- Modify: docs/openapi.yaml

**Interfaces:**
- Consumes JWT role_ids from Task 1.
- Produces X-HHC-Role-IDs as comma-separated verified UUIDs.
- Retains all five media_sync_user reader requirements in this expand commit.

- [ ] **Step 1: Add failing verifier and handler tests**

Use a signed token with unsorted role UUIDs. Assert VerifyToken returns sorted Claims.RoleIDs and the handler emits X-HHC-Role-IDs. Add a config test proving a client-supplied header is cleared.

- [ ] **Step 2: Run tests and observe RED**

Run: go test ./internal/verifier -run 'RoleIDs|TrustedHeaders' -count=1

Expected: FAIL because the verifier ignores role_ids.

- [ ] **Step 3: Parse and forward role IDs**

Add RoleIDs []string to Claims and RoleIDs any with json role_ids to raw claims. Parse with stringList, sort beside roles/scopes, emit X-HHC-Role-IDs, clear it in proxy.conf, and capture/forward only the auth subrequest value in protected.conf.

- [ ] **Step 4: Verify Gateway producer gates**

Run:

~~~bash
go test ./internal/verifier -count=1
go test ./... -count=1
bash scripts/test-media-sync-routing.sh
bash scripts/test-config.sh
git diff --check
~~~

Expected: all pass; routing still proves the old viewer gate exists.

- [ ] **Step 5: Update OpenAPI and commit**

~~~bash
git add internal/verifier conf.d/common/proxy.conf conf.d/common/protected.conf docs/openapi.yaml
git commit -m "feat: forward verified role ids"
~~~

---

### Task 3: Asset reader authority - ACL-only, UUID roles, scoped 404, audit

**Repository:** website/asset-api

**Branch:** codex/media-acl-terminal

**Files:**
- Modify: internal/assets/types.go
- Modify: internal/assets/service.go
- Modify: internal/assets/service_test.go
- Modify: internal/httpapi/handler.go
- Modify: internal/httpapi/handler_test.go
- Modify: internal/postgres/store.go
- Modify: internal/postgres/store_integration_test.go
- Create: internal/migrations/sql/015_collection_acl_authority.sql
- Modify: internal/migrations/migrations.go
- Modify: docs/openapi.yaml
- Retain: docs/superpowers/specs/2026-08-24-line-media-acl-authority-design.md
- Retain: docs/superpowers/plans/2026-08-24-line-media-acl-authority.md

**Interfaces:**
- Consumes trusted X-HHC-User-ID, optional X-HHC-Role-IDs, token expiry, and exact Gateway Dapr identity.
- CollectionSubject becomes { UserID string; RoleIDs []string }.
- ACL mutation inputs add ActorUserID and RequestID.
- Migration additively adds the short-lived ticket `role_ids` snapshot and creates `asset_collection_acl_audit`. It retains legacy `roles` only so the pre-015 runtime SQL remains deployable during expansion; legacy names are never copied into or dual-read with UUID authorization, and a later contract migration removes `roles`.

- [ ] **Step 1: Write failing service tests**

Prove a non-empty user UUID is valid with no role IDs, while an empty user remains forbidden. Add direct item and reader ticket cases without a viewer role.

Run: go test ./internal/assets -run 'Collection.*ACLOnly|Collection.*WithoutGlobalRole' -count=1

Expected: FAIL with ErrForbidden from validCollectionSubject.

- [ ] **Step 2: Remove the global reader validator**

Delete CollectionReaderRole. Validate only non-empty UserID. Rename collection reader and reader-ticket Roles fields to RoleIDs; leave unrelated asset grant subject types unchanged.

- [ ] **Step 3: Add failing HTTP identity tests**

Assert reader middleware accepts X-HHC-User-ID with empty role IDs, parses deduplicated X-HHC-Role-IDs, and still rejects missing Gateway caller/token, missing user, and invalid token expiry.

Run: go test ./internal/httpapi -run 'CollectionReader.*RoleIDs|CollectionReader.*NoGlobalRole' -count=1

Expected: FAIL because the handler reads X-HHC-Roles.

- [ ] **Step 4: Parse trusted role IDs**

Read X-HHC-Role-IDs with the existing trim/deduplicate pattern and store them in CollectionSubject.RoleIDs.

- [ ] **Step 5: Add failing PostgreSQL authorization tests**

Cover this literal matrix with real PostgreSQL:

~~~text
direct user ACL, no role IDs         -> list/item/ticket/redeem success
matching role UUID ACL              -> list/item/ticket/redeem success
no ACL                              -> list empty; scoped get ErrNotFound
wrong role UUID                     -> scoped get ErrNotFound
ACL revoke after ticket issuance    -> redemption ErrNotFound
~~~

Assert unauthorized and missing IDs return the same repository error.

Run: go test ./internal/postgres -run 'TestCollection.*ACLAuthority|TestCollectionContentTicket.*RoleIDs' -count=1

Expected: FAIL because SQL still requires media_sync_user, compares names, and returns ErrForbidden.

- [ ] **Step 6: Make ACL the only SQL authority**

Remove every viewer-role predicate from list, collection, item, ticket creation, and redemption. Compare role ACL subject_id to RoleIDs. Return ErrNotFound for scoped resources without matching ACL.

- [ ] **Step 7: Add failing ACL audit tests**

Prove one add and one revoke produce two immutable audit rows with actor UUID, request ID, action, ACL ID, subject type/ID, and timestamp. An idempotent replay must not append another event.

Run: go test ./internal/postgres -run TestCollectionACLAuditIsAtomicAndIdempotent -count=1

Expected: FAIL because the audit table and actor fields do not exist.

- [ ] **Step 8: Implement atomic successful-mutation audit**

Create asset_collection_acl_audit in migration 015. Add `role_ids text[] NOT NULL DEFAULT '{}'` while retaining `roles`; do not copy legacy names or read them for UUID authorization. New runtime inserts write empty `roles` and the verified UUID snapshot to `role_ids`. Remove `roles` only in a later contract migration after old runtimes are retired. Thread actor and request ID through handler/service inputs. Insert audit inside the existing add/revoke transaction only for a first claimed mutation.

- [ ] **Step 9: Verify Asset gates**

Run:

~~~bash
go test ./internal/assets ./internal/httpapi -count=1
go test ./internal/postgres -count=1
go test ./... -count=1
go vet ./...
go build ./cmd/...
git diff --check
~~~

Expected: all pass.

- [ ] **Step 10: Update OpenAPI and commit**

Document ACL-only semantics, X-HHC-Role-IDs, scoped 404, UUID role subjects, and audit actor headers.

~~~bash
git add internal docs
git commit -m "fix: make collection ACLs authoritative"
~~~

---

### Task 4: LINE management facade - validate UUID subjects and propagate actor

**Repository:** hhc-line-function-bot

**Branch:** codex/media-acl-authority

**Files:**
- Modify: src/account/account-admin-client.ts
- Modify: src/clients/asset-api.ts
- Modify: src/media-sync/http-routes.ts
- Modify: src/media-sync/service.ts
- Modify: src/__tests__/account-admin-client.test.ts
- Modify: src/__tests__/asset-api.test.ts
- Modify: src/__tests__/media-sync-http.test.ts
- Modify: docs/openapi.yaml

**Interfaces:**
- Consumes Account role subjects with immutable UUID id.
- Re-resolves the exact subject before grant.
- Sends X-HHC-Actor-User-ID and X-HHC-Request-ID on Asset ACL add/revoke.

- [ ] **Step 1: Add failing subject-validation tests**

Assert ACL add rejects a non-UUID user or role ID, an Account result without an exact type/ID match, and maps Account failure to 503. A verified exact subject is forwarded.

Run: pnpm test -- --run src/__tests__/media-sync-http.test.ts

Expected: FAIL because role IDs accept arbitrary names and grants are not re-resolved.

- [ ] **Step 2: Validate IDs and recipients**

Use the existing UUID regex for both types. Before addCollectionAcl, search Account with query=subjectId, page=1, perPage=20 and require one exact id/type match. Return 400 when absent and 503 when Account is unavailable.

- [ ] **Step 3: Add failing actor-propagation tests**

Assert Asset ACL add/revoke contain the manager UUID only in X-HHC-Actor-User-ID and preserve the request ID. Unrelated requests must not gain the actor header.

Run: pnpm test -- --run src/__tests__/asset-api.test.ts

Expected: FAIL because the Asset client has no actor option.

- [ ] **Step 4: Propagate actor minimally**

Add actorUserId only to ACL mutation options and thread auth.userId through the two service methods. Do not place Account identity into tickets, binding codes, or logs.

- [ ] **Step 5: Verify LINE bot gates**

Run:

~~~bash
pnpm format:check
pnpm typecheck
pnpm lint
pnpm test
pnpm build
git diff --check
~~~

Expected: all pass. Controlled-agent evals are not required because these are Admin HTTP routes.

- [ ] **Step 6: Update OpenAPI and commit**

~~~bash
git add src docs/openapi.yaml
git commit -m "fix: validate media ACL subjects"
~~~

---

### Task 5: Admin Console - lock role UUID DTO behavior

**Repository:** website/admin-fe

**Branch:** codex/media-acl-authority

**Files:**
- Modify: src/lib/media-sync-api.test.ts
- Modify: src/pages/MediaSyncPage.test.tsx
- Modify only on observed RED: src/lib/media-sync-api.ts
- Modify only on observed RED: src/pages/MediaSyncPage.tsx

**Interfaces:**
- Consumes { id: UUID, type: "role", displayName: role name }.
- Sends id as subjectId and displays displayName.

- [ ] **Step 1: Add role UUID behavior tests**

Use a literal role whose ID differs from display name:

~~~ts
const role = {
  id: '018f0000-0000-7000-8000-000000000002',
  type: 'role' as const,
  displayName: 'Worship Team'
}
~~~

Assert the picker renders Worship Team and the POST body sends the UUID.

- [ ] **Step 2: Run focused tests**

Run: corepack pnpm test:run -- src/pages/MediaSyncPage.test.tsx src/lib/media-sync-api.test.ts

Expected: PASS if current separation is already correct. If RED, it must demonstrate displayName is incorrectly substituted for id.

- [ ] **Step 3: Apply only a proven production correction**

Keep the existing DTO. Ensure subject.id reaches addAcl and displayName is rendering-only. Add no role-specific UI model.

- [ ] **Step 4: Verify Admin gates and commit**

~~~bash
corepack pnpm test:run
corepack pnpm lint
corepack pnpm build
git diff --check
git add src
git commit -m "test: lock media ACL role ids"
~~~

---

### Task 6: LibrePresenter - scoped 404 cleanup compatibility

**Repository:** hhc-client-v2

**Branch:** fix/media-acl-authority

**Files:**
- Modify: src/main/__tests__/ipc/hhc-assets.test.ts
- Modify: src/main/ipc/hhc-assets.ts
- Modify: src/renderer/src/lib/__tests__/hhc-asset-api.test.ts
- Modify: src/renderer/src/lib/hhc-asset-api-browser.ts
- Modify: src/renderer/src/lib/hhc-asset-api-electron.ts
- Modify: src/renderer/src/lib/__tests__/hhc-line-access.test.ts
- Modify: src/renderer/src/lib/hhc-line-access.ts
- Modify: src/renderer/src/lib/__tests__/sync-runtime.test.ts
- Modify: src/renderer/src/lib/__tests__/media-projection-sync.test.ts
- Modify only on observed RED: src/renderer/src/lib/sync-runtime.ts
- Modify only on observed RED: src/renderer/src/lib/media-projection-sync.ts

**Interfaces:**
- Legacy 403 remains access-revoked during rollout.
- Scoped Asset 404 becomes the same safe cleanup signal for known HHC LINE roots.
- Successful empty collection list never triggers global cleanup.

- [ ] **Step 1: Add failing 404 classification tests**

Add browser and Electron cases proving HTTP 404 maps to access-revoked with status 404, without changing 401, 429, or 5xx behavior.

Run:

~~~bash
npx vitest run src/main/__tests__/ipc/hhc-assets.test.ts \
  src/renderer/src/lib/__tests__/hhc-asset-api.test.ts
~~~

Expected: FAIL because 404 is currently fatal/not found.

- [ ] **Step 2: Classify scoped 404 minimally**

Map Asset 403 and 404 to the existing safe classification while preserving numeric status. Add no new store state or error hierarchy.

- [ ] **Step 3: Add failing cleanup tests**

Duplicate only the smallest existing 403 cases with status 404: one known root is unlinked, siblings continue, and a projected source stops. Assert an empty list does not call global cleanup.

Run:

~~~bash
npx vitest run src/renderer/src/lib/__tests__/hhc-line-access.test.ts \
  src/renderer/src/lib/__tests__/sync-runtime.test.ts \
  src/renderer/src/lib/__tests__/media-projection-sync.test.ts
~~~

Expected: FAIL where cleanup requires exact status 403.

- [ ] **Step 4: Accept 403 or 404 at existing guards**

Change only HHC LINE scoped cleanup guards. Keep account mismatch and authentication cleanup separate.

- [ ] **Step 5: Verify LibrePresenter gates and commit**

~~~bash
npm run lint
npm run typecheck
npx vitest run
npm run build
git diff --check
git add src
git commit -m "fix: handle unavailable LINE media collections"
~~~

Packaging is not required because native runtime and packaging contracts are unchanged.

---

### Task 7: Gateway contract switch - remove viewer gate

**Repository:** website/api-gateway

**Stacked Branch:** codex/media-acl-remove-reader-gate, based on Task 2

**Files:**
- Modify: conf.d/default.conf
- Modify: scripts/test-media-sync-routing.sh
- Modify: docs/openapi.yaml

**Interfaces:**
- Keeps bearer authentication and trusted identity headers.
- Sets required roles empty on list, changes, item metadata, ticket issue, and direct content.

- [ ] **Step 1: Change executable routing expectations first**

Require empty required-role values while retaining protected.conf, exact methods, route IDs, and CORS.

Run: bash scripts/test-media-sync-routing.sh

Expected: FAIL because default.conf still requires media_sync_user.

- [ ] **Step 2: Remove only the five viewer requirements**

Set hhc_required_roles to empty in the five reader locations. Do not remove JWT auth, rate limiting, method policy, feature flags, Dapr routing, or CORS.

- [ ] **Step 3: Verify Gateway switch and commit**

~~~bash
bash scripts/test-media-sync-routing.sh
bash scripts/test-config.sh
go test ./... -count=1
! rg -n 'media_sync_user' conf.d scripts docs/openapi.yaml
git diff --check
git add conf.d/default.conf scripts/test-media-sync-routing.sh docs/openapi.yaml
git commit -m "fix: authorize media readers by collection ACL"
~~~

Expected: all pass.

---

### Task 8: Account contract cleanup - remove viewer role and permission

**Repository:** website/account-api

**Stacked Branch:** codex/media-acl-retire-viewer, based on Task 1

**Files:**
- Create: migrations/000017_retire_media_sync_viewer.up.sql
- Create: migrations/000017_retire_media_sync_viewer.down.sql
- Modify: internal/database/db.go
- Modify: internal/database/migration_integration_test.go
- Modify: docs/openapi.yaml

**Interfaces:**
- Removes canonical media_sync_user and media-sync:read only.
- Retains media_sync_manager with exactly media-sync:manage.
- Down migration recreates the viewer bundle without assigning users.

- [ ] **Step 1: Add failing migration and seed tests**

Assert fully migrated/seeded DB has no viewer role/read permission and retains exact manager bundle. Assert down recreates only the viewer role/permission association.

Run: go test ./internal/database -run 'Test.*RetireMediaSyncViewer|TestMigrations' -count=1

Expected: FAIL because seed and migrations create the viewer bundle.

- [ ] **Step 2: Implement migration**

Up deletes user_permissions and role_permissions associations for media-sync:read, user_roles associations for media_sync_user, then deletes that role and permission. Preserve unrelated custom roles/permissions. Down idempotently recreates permission, role, and their association without user assignments.

- [ ] **Step 3: Remove viewer seed definitions**

Delete viewer role/bundle and read permission. Keep manager unchanged.

- [ ] **Step 4: Verify Account cleanup and commit**

~~~bash
go test ./internal/database -count=1
go test ./... -count=1
go vet ./...
go build ./cmd/...
! rg -n 'media_sync_user|media-sync:read' internal docs/openapi.yaml
rg -n 'media_sync_user|media-sync:read' \
  migrations/000017_retire_media_sync_viewer.up.sql \
  migrations/000017_retire_media_sync_viewer.down.sql
git diff --check
git add migrations internal/database docs/openapi.yaml
git commit -m "refactor: retire media sync viewer entitlement"
~~~

Expected: all pass.

---

### Task 9: Cross-repository review, inventory preview, and stop gate

- [ ] **Step 1: Review every diff**

Run git diff origin/main...HEAD --check, inspect stats and full diffs. Reject unrelated refactors, dependencies, duplicate auth helpers, token logging, and client-side authorization.

- [ ] **Step 2: Verify stage boundaries**

~~~text
Account producer: viewer retained; role_ids present.
Gateway producer: viewer gate retained; X-HHC-Role-IDs present.
Asset: no CollectionReaderRole or viewer predicate.
Gateway switch: no viewer gate in config/tests/OpenAPI.
Account cleanup: no viewer role/read permission except rollback down migration.
LibrePresenter: no viewer checks; 403 and scoped 404 cleanup both pass.
~~~

- [ ] **Step 3: Prepare but do not run production inventory**

Create an exact read-only procedure that exports opaque Account user/role IDs and opaque Asset collection/ACL IDs separately, joins locally, and reports counts for active direct ACLs, UUID role ACLs, legacy role-name ACLs, and dangling subjects. Do not include email, display names, content metadata, credentials, or tokens.

- [ ] **Step 4: Rerun final repository gates**

Capture command, exit status, commit SHA, and test counts without secrets.

- [ ] **Step 5: Stop before external delivery**

Do not push, open PRs, merge, deploy, run production migrations, or reset ACL data. Report local branches/commits, gates, required PR order, and pending production approvals.

## Required later PR and release order

1. Account producer: role_ids and UUID role subjects.
2. Gateway producer: X-HHC-Role-IDs with viewer gate retained.
3. LINE management facade and Admin role-ID contract.
4. Asset ACL authority.
5. LibrePresenter scoped-404 compatibility.
6. Reviewed test-data ACL reset/recreation with explicit approval.
7. Gateway reader-gate removal as the behavior switch.
8. Live reader/manager/ticket smoke and observation.
9. Account viewer entitlement cleanup.

Each repository needs its own PR, required CI, merge-triggered immutable release, deployed revision check, readiness check, and relevant live smoke. A failure stops the sequence. Rollback restores the Gateway viewer gate before any destructive response.
