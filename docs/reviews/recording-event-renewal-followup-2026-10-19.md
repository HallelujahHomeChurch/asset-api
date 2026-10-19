# Recording Event message-renewal follow-up

## Confirmed failure and rollback

PR126/commit57c57e4536776efc15093152f2eb8470b7b75712, CI39199223771 and
Release39200299760 attempt1 succeeded. Actual API116 was Healthy/Running100%,
Event true, legacyManual, three Event lanes4CPU8Gi and hourlymaintenance0.5/1.
Worker: sha256:917711c429843f9d446b6a8a2330bed40b7fd02f1a829b6ccf1850b7c6c1c0c4.

Private synthetic source a86a3ac566cec8e798a82a0e2ded3a00 / recording
05d5fd45-c9d2-4233-aa0a-eb9abd596683 automatically started processing-fnhtl at
2026-10-19T04:59:29Z. At05:00:19.305Z its first30-second queue renewal failed:
UpdateMessage403 AuthorizationPermissionMismatch; worker elapsed30875ms,
claims1,renewals0,failedtrue,process exit1. This is an Event acceptance failure,
not proof of ready, not a Windows defect and not a decoder/resource failure.
The twelve-grant preflight passed but never checked UpdateMessage authorization.
The earlier initial-source failure remains separate; do not retroactively assert
this403 uniquely explains its unrecorded provider error.

User-approved rollback: repository Event flag false, fresh actual116-image full
Provider preview13Modify/noCreate/Delete and unchanged policyPASS. Re-run only
the existing deployment job117894755762 of Release39200299760, reusing its verified
image archive and imageIDs; do not re-run verify/build or overwrite provenance.
Read actual rollback revision, trigger topology and identical digests before
claiming restoration. Keep source, attempts, all formal recordings and queues.

## Narrow fix

Azure requires `Microsoft.Storage/storageAccounts/queueServices/queues/messages/write`
for UpdateMessage. Existing MessageProcessor has only read/process; Sender has
add/action. Retain the existing12assignments, add a custom role with exactly that
one DataAction and empty management Actions/NotActions/NotDataActions. Assign it
only to the existing worker on the three exact work queues: total15assignments.
It also allows message content writes/addition at that Azure permission granularity;
do not call it a visibility-only permission. No queue management, wildcard,
API message-write, poison-write, CI privilege or shared-key/SAS workaround.

Custom role ID: bf5dee66-37ae-4ef4-b34a-9f135e10d804.
Assignable scope: existing resource group; actual grants remain queue-scoped.
Operator bootstrap requires roleDefinitions/write plus roleAssignments/write,
from merged code and an exact reviewed standalone Provider what-if. Ordinary
runtime deployments continue to contain no IAM writes.

Preflight verifies the fixed role, full permission blocks, assignable scope and
exact queue/principal assignments. Missing old12-only grants, wrong scope or
principal, broad/extra permission blocks and query errors fail closed. Regression
was red on the former checker; updated fixtures and compiled Bicep checks pass.
This changes no runtime producer contract, digest, media rule or global slots2.

## Reactivation acceptance

Only after rollback health, final CI/review, merged bootstrap, exact4-create
preview (one custom role, three queue assignments), operator apply and actual
strict preflight: activate through CI/CD using verified immutable artifacts.
Prove a real processing execution beyond30seconds has successful renewal(s),
finishes the same fenced work and final ack. Then verify private source ready,
automatic covers/preview and existing member playback. Azure IAM propagation and
role readback alone are insufficient; keep this failure as failed.

Windows Event acceptance remains blocked until formal activation smoke passes.
No plugin/key/wire change, no formal recording mutation or reupload. The eventual
Windows scope remains one8–10min S10 scene; accepted long suites stay accepted.
>=24h total execution/standby/retry/Queue/API/DB and billed-cost acceptance, plus
candidate2CPU4Gi/1CPU2Gi resource A/B, remain separate unpassed gates.

Sources: [Azure operation permissions](https://learn.microsoft.com/en-us/rest/api/storageservices/authorize-with-azure-active-directory#permissions-for-queue-service-operations),
[role definitions](https://learn.microsoft.com/en-us/azure/role-based-access-control/built-in-roles/storage#storage-queue-data-message-processor).
