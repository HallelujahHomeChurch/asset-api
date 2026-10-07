# OBS platform activation preview

This is a review artifact, not evidence of deployed settings. Do not apply it
until the dependent PRs are approved, released and verified through their existing
workflows. Windows capture and provider acceptance are separate gates.

## Existing resource envelope

Repository `infra/main.bicep` currently defines the HLS recording Job as 4 vCPU /
8 GiB, cron `*/1 * * * *`, parallelism 1 per execution, and a six-hour replica
limit. Database processing slots bound media work to two claims across executions.
The new live batch loop uses the beginning of the existing Job execution; this PR
does not increase those resource settings or create a new cloud service.

These source defaults have not been compared with live Azure configuration.
The one-minute trigger plus provider startup, copying and decoding must be measured
before promising the 60–120 second viewing target. If two existing long VOD/source
claims occupy both slots, live work waits. Do not increase replicas or slots as a
substitute for measuring the contention and memory bound.

## Release and activation order

1. Release Account exact publication-permission verification and the two new
   internal audit action contracts.
2. Release Asset migrations/API, recording Job image and media Worker through
   their existing release paths; verify they refer to the approved artifact.
3. Release CMS durable capture/automatic publication/member live authorization,
   then the Gateway exact protected routes. Keep OBS live opt-in off while checking
   producer and consumer compatibility.
4. Publish the approved generated shared client version, pin that exact version
   in Website/Admin and require consumer CI. A locally packed client is only an
   integration test; it does not satisfy the package release gate.
5. Provision the reviewed native `hhc-obs` public OAuth client using Account's
   preview. Verify scopes/PKCE/loopback binding without collecting credentials.
6. Deliver the immutable C1 schema/fixture hash manifest to Windows and receive
   its implementation acknowledgement. Run real F1/F1-L fixtures and the G2 live
   acceptance before enabling member live viewing for operational use.

Retain current YouTube and local recording outputs during device acceptance.
Stopping YouTube remains an operator decision after consecutive successful events.

## Rollback boundary

Disable new capture/live entry points through approved application configuration
and the operator's existing close-live control. Preserve upload queues, recording
rows, capture receipts, processing work and private objects for recovery. Already
issued live grants can remain valid for up to five minutes; closing an entry point
is not an immediate token revocation mechanism. Roll back application artifacts
only through the existing release/rollback workflow, with additive migrations left
in place. Do not delete staging or completed VOD as a rollback shortcut.

## Acceptance evidence to collect

- Exact deployed revisions, private caller enforcement, public route denial and
  authenticated own-scope viewing/renewal; do not put credentials in evidence.
- Real Windows H.264/AAC fMP4 fixtures: all three profiles, full 30-second segments,
  short normal tail, missing/discontinuous/changed-codec rejection.
- Real R2 conditional create/update and publication retry races, no future sequence
  visibility, private authorization before cached objects.
- A 2.5-hour run with and without YouTube, end-to-end timecode p50/p95/max latency,
  five-minute upload interruption and replay from minute one near event end.
- Browser full-event seekability after buffer eviction; 15-minute sleeping tab
  renews its old scope with paused/rate/quality/position intact. Test iPhone Safari
  separately from desktop HLS.js; native buffering has no promised 120-second cap.
- Background completion/publication after OBS exits, original authorizer revocation,
  manual edit/cancel/unpublish races, one audit/outbox/notification and failure
  cleanup without user status polling.

Use isolated test resources and bounded load. These checks have not been executed
against production by this change.
