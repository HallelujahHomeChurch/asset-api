# Repository Instructions

## Delivery Policy

- Treat `main` as the production branch. Do not commit or push directly to `main`.
- Create a focused branch for every task and submit changes through a pull request.
- CI must pass before merge. Do not bypass, ignore, or merge around a failing required check.
- Keep changes small and reviewable, and use squash merge unless the repository explicitly requires another strategy.
- For deployable applications and services, a merge to `main` is the release boundary: build an immutable artifact, deploy it through CI/CD, then verify health and the production route.
- Never deploy an unmerged local commit or replace the CI/CD path with an ad hoc production command.
- If deployment fails, preserve the last healthy revision, report the failure, and use the repository rollback path.

<!-- BEGIN HHC DELIVERY V1 -->
## HHC delivery coordination

Before retained implementation, use `.agents/skills/hhc-delivery/SKILL.md` and read the [pinned shared workflow](https://github.com/HallelujahHomeChurch/hhc-planning/blob/9329942c91cc4d8d93104c666e2752024ee4e319/workflow/v1/WORKFLOW.md).
Reuse/create the owning-repository engineering issue, link its private canonical Feature and Project, verify current head/dependencies, and record coordinator acknowledgement plus run ID/environment/branch/HEAD. Read-only discovery needs no engineering ticket.
Assignees/comments/timeouts are not locks or takeover authority. Separate CI, merge, publication, deployment, flags and live/device acceptance; keep accepted Feature baselines visible and track improvements separately. Closed duplicates/cancellations are not delivery.
Progress authority: [private HHC all-product Project](https://github.com/orgs/HallelujahHomeChurch/projects/1). Read its current item/status and the canonical Feature before implementation; report specific access blockers instead of guessing or using the old PENDING bootstrap. Do not expand OAuth permissions implicitly. External requests require Ray triage. Keep private/member/note content and raw sensitive evidence out of public repositories.
Preserve repository-specific rules. This adoption uses draft PRs and does not authorize engineering merge, deployment, permission changes or new full-suite CI.
<!-- END HHC DELIVERY V1 -->
