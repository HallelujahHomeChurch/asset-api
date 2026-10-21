# Repository Instructions

## Delivery Policy

- Treat `main` as the production branch. Do not commit or push directly to `main`.
- Create a focused branch for every task and submit changes through a pull request.
- CI must pass before merge. Do not bypass, ignore, or merge around a failing required check.
- Keep changes small and reviewable, and use squash merge unless the repository explicitly requires another strategy.
- For deployable applications and services, a merge to `main` is the release boundary: build an immutable artifact, deploy it through CI/CD, then verify health and the production route.
- Never deploy an unmerged local commit or replace the CI/CD path with an ad hoc production command.
- If deployment fails, preserve the last healthy revision, report the failure, and use the repository rollback path.

## Architecture and local verification

- Owns file mechanics: upload sessions, Blob object naming, completion/scan state, grants, downloads and recording media processing. CMS/business records remain with consumers, not this repository.
- `cmd/server` wires the HTTP routes and middleware.
  Domain logic is in `internal/assets`; persistence is in `internal/postgres`.
  Separate scan/derivative/recording/retention command workers use their named
  queue/processing packages; do not collapse their acceptance into HTTP startup.
- Read [README.md](README.md), [go.mod](go.mod), `internal/config`, [Dockerfile](Dockerfile) and [.github/workflows/ci.yml](.github/workflows/ci.yml). Export task-local configuration (the binary does not automatically load .env), migrate a disposable PostgreSQL DB with `go run ./cmd/migrate`, then `go run ./cmd/server`. Local uploads use .data/assets.
- `go test -race -json ./... -count=1 -p=1` and `go vet ./...` are CI's Go checks; TEST_DATABASE_URL/TEST_POSTGRES_DSN and required external media/scan tools determine integration coverage. ClamAV, media processing, Azure Blob/R2 and real Dapr acceptance are separate dependencies.
- ASSET_ALLOW_DEV_CALLER_HEADER=true is local-only. Preserve app-token/caller admission, restricted grants, scan completion and immutable media contracts. Do not change retention, enable workers, run backfills or mutate cloud storage without the task's explicit scope.

The commands above are source-defined entrypoints, not a record of successful execution. Report installation, local tests, Linux startup, CI and live acceptance separately.

## Multi-repository Codex tasks

Read the checked-out `hhc-planning/AGENTS.md` and
`hhc-planning/docs/cloud-environments.md` first and record its revision.
Sibling repositories do not inherit it automatically. This repository owns its
setup/tests; use the task’s smallest runtime set and preserve active writers in
isolated worktrees. Follow explicit review/merge/deployment authority. Pure human
documentation uses lightweight checks; source, workflow/configuration, executable
skills and mixed changes retain full verification.

If no sibling planning checkout exists, use the [single-repository fallback](https://github.com/HallelujahHomeChurch/hhc-planning/blob/73ee1af2a1ff828d72421a73626f45ff4ce04a6f/docs/cloud-environments.md#single-repository-checkout)
to read the same entry documents through authorized GitHub access at the task’s
immutable planning revision. Record that revision; report the exact inaccessible
target/error without expanding permissions or copying private text here.
