# Recording retention and editor implementation plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task. Keep one final independent review and the existing isolated worktrees.

**Goal:** Dynamically configure recording retention from upload completion and make the cover/editor workflow understandable.

**Architecture:** Asset owns the persisted policy and authoritative media expiry. CMS owns authorized management and content projections; Gateway, SDK and clients expose the same contract. Existing cleanup workers and locks remain responsible for object deletion.

**Tech Stack:** Go, PostgreSQL, TypeScript/React, existing shared UI, Cloudflare Worker/R2.

**Spec:** [Approved design scope](recording-retention-editor-design.md). Implementation and independent review are recorded in the task ledger; merge, release and production activation remain separate gates.

## Global constraints

- Default 30 days; integer 1–365; UTC `uploadedAt + days * 24 hours`.
- Upload completion is immutable. No reset on encode, resume, publish or cover changes.
- Only human `cms:recordings:write` may change global policy; service principals cannot.
- Preview destructive effects; revision and idempotency protect mutations; audit fails closed.
- Existing issued grants may survive up to one hour. Never delete their objects early.
- No new service/container, scanner, public cache, CLI flags or per-recording policy.
- Deleted/cleanup-started content cannot revive. Trace receipts still retain their independent 365-day policy.
- No production data cleanup or policy activation without reviewed impact and explicit approval.

## Review focus

1. A shorter policy races with grant issuance or cleanup: use current policy and never delete beneath an unexpired grant.
2. A longer policy cannot recover a partially deleted package: terminal cleanup state wins over age.
3. Old CMS expiry filters hide valid recordings after extension: policy applies before pagination/filtering.
4. Browser source completion and CLI HLS completion have different producers: preserve their original timestamps.
5. A delayed cover upload response arrives after navigation or another selection: never replace the new selection or leak object URLs.

### Task 1: Authoritative Asset policy and lifecycle

**Files:** create `internal/assets/recording_retention.go`, `internal/postgres/recording_retention.go`, `internal/postgres/recording_retention_integration_test.go` and the next ordered migration under `internal/migrations/sql/`; modify `internal/postgres/recording_package.go`, `recording_source_job.go`, `recording_package_cleanup.go` and existing grant/cover/preview expiry callers; update `docs/openapi.yaml` and `internal/httpapi` route handlers/tests.

**Interfaces:** `RecordingRetentionPolicy{retentionDays, revision, activatedAt}`; `RecordingRetentionPreview{retentionDays, revision, evaluatedAt, affectedCount, affectedBytes}`. Add private GET `/priv/recordings/retention-policy`, POST `/priv/recordings/retention-policy/preview`, PUT `/priv/recordings/retention-policy`. Package status adds immutable `uploadedAt` and `retentionRevision`, retaining `mediaExpiresAt` as the effective projection.

- [ ] Write failing live-Postgres tests for 30→14/60, invalid days, immutable completion, revision conflicts, audit failure rollback, preexisting packages, missing completion, cleanup races, and grant grace. Cover both browser-source and CLI package producers.
- [ ] Run focused tests; confirm failure before implementing policy behavior.
- [ ] Persist one policy row. Reuse existing mutation/audit/actor patterns. Initial migration preserves old expiry behavior until explicit policy activation; activation uses the same preview and confirmation path, not a separate public feature flag.
- [ ] Centralize effective expiry; replace every fixed-30-day predicate across readiness, grant, covers, previews and cleanup. Policy unavailable/missing completion fails closed without destructive fallback.
- [ ] Maintain consistent policy→package lock order. Record irreversible cleanup before provider writes; deletion grace must account for grants issued before a retroactive shortening, not merely new expiry＋one hour in the past.
- [ ] Return effective status for CMS. Preserve legacy callers until consumer rollout completes. Do not change upload-session/Blob retry deadlines.
- [ ] Run `go test -race ./... -count=1 -p=1` with isolated PostgreSQL, `go vet ./...`, migration/contract checks and Worker suite; commit only on green.

### Task 2: CMS, Gateway, audit and SDK contract

**Files:** CMS `internal/recordings/package.go`, `model.go`, existing list/publication/cover handlers and recording postgres projections; add `internal/recordings/retention.go` and tests, extend `internal/assetclient`, `openapi.yaml`, `openapi_test.go`; Gateway existing recording route policy/tests; Audit existing recording catalog/producer fixtures; SDK `packages/hhc-web-client` source, canonical schema and generated types.

**Interfaces:** GET/PUT `/admin/recordings/retention-policy`, POST `/admin/recordings/retention-policy/preview`; same policy/preview types as Task 1. PUT consumes expected revision plus confirmed preview and existing mutation identity conventions. SDK exposes `getRecordingRetentionPolicy`, `previewRecordingRetentionPolicy`, `updateRecordingRetentionPolicy`.

- [ ] Write failing tests: human/read/write distinctions, service denial, policy conflict, stale preview, timeout/fail-closed, audit atomicity; updated Asset timestamps accepted without requiring readyAt＋720h.
- [ ] Implement management through the existing Asset client; never accept arbitrary actor headers or caller-selected owner. Limit preview validity to five minutes; counts explicitly represent preview time, not a promise of an exact later deletion set.
- [ ] Hydrate immutable upload completion for existing content and remove fixed local expiry predicates before list filtering/pagination. Fetch authoritative policy once per request; fail closed when unavailable. No independent editable CMS policy copy.
- [ ] Recheck policy and publication/deletion state across grant issuance. Bound issued expiry by both latest policy and the one-hour grant maximum.
- [ ] Add exact Gateway routes, human-only tests, canonical OpenAPI and audit catalog actions. Regenerate SDK, update mocks/tests and exact package versions only at release.
- [ ] Run full CMS PostgreSQL tests/vet, Gateway route/auth tests, Audit contract tests, SDK tests/lint/build/packed-consumer checks; commit per repository.

### Task 3: Admin settings, editor and cover processing UI

**Files:** Admin `src/pages/recordings/RecordingListPage.tsx`, `RecordingDetailPage.tsx`, `RecordingCoverEditor.tsx`, their tests, `editor-labels.ts`, `cover-labels.ts`, existing recording styles/client adapter; create `RecordingRetentionSettings.tsx` and its test. Web member-video locale copy/tests.

**Interfaces:** SDK methods from Task 2; existing `onChange`/`onBusyChange` cover contract remains. No additional upload endpoint.

- [ ] Write failing UI tests for all requested copy/layout changes, immediate custom-cover item, upload/processing spinner, failure/retry, stale responses, unmount cleanup, policy preview/confirmation and revision conflict.
- [ ] Add a shared-style settings dialog on recording list, integer input 1–365, current policy, previewed impact and explicit destructive confirmation. Do not save on input change.
- [ ] Remove the two specified editor hints; title hint exactly `最多 180 字` in Traditional Chinese with equivalent existing locales. Move unpublish to the information-card heading row, preserving its confirmation/permissions; leave delete outside the card.
- [ ] Remove redundant restore-auto button. On crop confirmation, close crop dialog and add a pending custom-cover tile with local preview and accessible spinner. Track upload versus processing; block conflicting replacement while busy, retain current saved cover on failure, and release the object URL on replacement/unmount.
- [ ] Keep auto-cover selection and save-to-apply semantics. Change public list heading to `最近錄影` so retention changes need no copy release.
- [ ] Run Admin/Web full bounded tests, lint and production builds; visually check narrow/mobile and desktop layout. Commit changes without temporary SDK workspace overrides.

### Task 4: Consolidated verification and PR integration

**Files:** existing task acceptance ledger and affected repository release documentation.

- [ ] Verify all spec requirements against tests; dispatch one fresh-context review, fix significant findings with regression tests.
- [ ] Resolve existing failing CI security findings with minimal patched dependencies and full checks; do not suppress CVEs or bypass required checks. Asset's observed blocker is scan-image perl-base 5.36.0-7+deb12u3, scanner-reported fixed version deb12u4; verify actual package availability before changing pins.
- [ ] Update the existing per-repository PRs; Admin PR follows published SDK and exact registry lock refresh. All required CI must pass before merge.
- [ ] Sequence compatible producers, CMS/Gateway, SDK and consumers. Keep retention activation disabled until old consumers are gone and the real-data dry-run is approved. No ad hoc production deployment.
  - Review correction: 480p-compatible Web and Worker must precede the Asset encoder and CLI. Asset's workflow gates runtime deployment on successful media deployment when member video is enabled. CLI also consumes additive retention timestamps; it has no policy-editing flags.
- [ ] Verify deployed health and relevant routes, then present activation impact separately. Physical Windows video and mobile/watermark acceptance remain distinct; recording privacy-notice publication still requires its pending approval.
- [ ] Remove only clean task-owned temporary worktrees after release/smoke and no remaining work; otherwise preserve them.
