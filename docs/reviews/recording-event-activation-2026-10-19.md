# Recording Event Job activation checkpoint

This checkpoint records the approved Event cutover. It changes no runtime code,
wire, inventory, media validation, public boundary, key or existing recording.
Use the release procedure in `recording-event-cutover-2026-10-19.md`.

## Provisioning evidence and publication failure

Release 39188190451 used previously released commit
`9720edd4a883a5c595a5b9ffe74fea0ad2e63dfb`.
Its verify, media deployment and runtime deployment jobs succeeded. API
`asset-api--0000113` is serving with Event mode false; the legacy 4 CPU/8 GiB
minute Schedule remains active. Four new recording Jobs exist as Manual,
with six dedicated queues. Maintenance is 0.5 CPU/1 GiB; other new Jobs are 4/8.

The final governance publication correctly failed `Immutable provenance conflict`:
rebuilding a previously published commit produced a different immutable image.
The publisher binds one commit to its original image and refuses replacement.
Do not overwrite provenance, weaken the guard, or reclassify that run as passed.

This newly merged checkpoint supplies a fresh release identity for activation.
After scoped permission/bootstrap and quiet-window checks, set enabled=true just
before the approved merge with final-head CI green. This checkpoint also changes
the preflight script, so the merge automatically starts Production Release once;
do not manually dispatch another release for the same commit. Do not rebuild a published commit
for another configuration rollout; use a fresh reviewed checkpoint. Ordinary
runtime deployment stays in CI/CD; the approved IAM bootstrap alone is an operator
procedure from merged code.

The first real permission readback exposed an exact-comparison defect: ARM
returned `resourcegroups` while the Storage account ID used `resourceGroups`.
Compare the complete scope IDs case-insensitively, preserving the exact queue
path, principal and role requirements. A regression reproduces the mixed-case
response; a sibling scope, missing role and query failure still fail closed.

Routine legacy executions last tens of seconds even with zero claims. Snapshot
and pre-pause admission now reuse the existing bounded five-minute idle wait,
so a short scheduled execution can finish naturally. Persistent work still fails
before mutation; nothing is killed, skipped or classified as idle while running.
The existing post-pause drain and rollback responsibility remain unchanged.

## Activation gate

- Fresh full Provider preview and existing difference-policy guard must pass.
- Apply/read back exactly twelve queue-scoped API/runtime grants from the merged
  `recording-event-permissions.bicep`; grant no IAM-write privilege to CI.
- Set provisioned=true, enabled=true, change-approved=true, processing CPU=4 and
  preview CPU=4 for the first trigger-only acceptance. Maintenance is hourly at
  UTC minute 17. Keep API min replicas >=1 and the two global media slots.
- CI captures the non-secret old topology, checks idle, pauses and safely drains
  legacy triggers immediately before activation. Never force-stop valid media.
- Runtime failure uses the existing fenced rollback and previous healthy API;
  preserve queues, receipts, inventory and media. Drain timeout fails visibly.

## Acceptance and protected data

Local race regressions cover Event admission/recovery, two-slot exclusion, lock
ordering, live/optional claims, sources, covers, queue and configuration behavior.
Production acceptance must read back revision/images/health/traffic/flags/scalers,
then exercise a synthetic source upload through natural ready and staff preview.
Keep it a private draft to avoid sending a synthetic publication notification.
Observe an existing published recording without changing it.

Windows needs one short non-meeting S10 session after confirmed deployment:
cold first-ready, cross-minute progress, first/five-minute auto cover, timeline,
normal Stop/seal/ready/natural publication and same-URL decoded VOD. No plugin or
wire change, key reset, old capture replay or repeated accepted long suite.

Before cutover, legacy starts at 2026-10-19 09:56-09:58 Asia/Taipei had SQLSTATE
53300. Later starts recovered with waiting=0 and active_slots=0. Retain this
baseline separately and monitor actual database/worker errors after activation.

Final deployment and Windows results are separate evidence. >=24h idle execution
and actual cost meters, and representative downsizing A/B, remain distinct gates.
Do not infer savings from CPU allocation or declare real OBS acceptance from CI.
