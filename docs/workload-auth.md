# Private caller authentication and trusted bulletin derivatives

Asset API private routes (`/priv/*`) accept exactly two production caller
proofs. `internal()` resolves the caller's own service identity in this order
(`authenticatedService`):

1. **Dapr.** A request whose `dapr-api-token` matches the revision-scoped
   `APP_API_TOKEN` came from the local Dapr sidecar, which sets
   `Dapr-Caller-App-Id`. `sameToken` rejects empty values, and configuration
   refuses to start without `APP_API_TOKEN` outside development.
2. **Entra workload identity.** Otherwise the request entered through internal
   ingress, where Container Apps Easy Auth (AAD, `Return401`, allowed
   applications/principals pinned in `infra/main.bicep`) validated the bearer
   token and replaced `X-MS-CLIENT-PRINCIPAL`. The principal must still carry
   the configured tenant, issuer, audience, `Asset.Invoke` role, and an exact
   configured client ID **and** object ID pair.

The resolved service is kept in the request context (`requestService`). Only
for ownership is `hhc-web-bulletin-worker` treated as `hhc-web-api`: the worker
creates and reads CMS-owned assets on CMS's behalf.

## Trusted bulletin derivative completion

`POST /priv/assets/bulletin-derivatives/{assetID}/complete` marks a watermark
worker output clean without a second ClamAV scan, because the bytes are
derived inside the worker from an already-scanned source whose checksum is
re-verified here. It requires `requestService == "hhc-web-bulletin-worker"`,
reached by either proof:

| Path | Proof | Configuration |
|---|---|---|
| Dapr (current App) | app-channel token + `Dapr-Caller-App-Id: hhc-web-bulletin-worker` | worker App Dapr app ID |
| Workload (Event Job, no Dapr) | Easy Auth principal of `hhc-web-bulletin-worker-identity` | `ASSET_WATERMARK_WORKLOAD_CLIENT_ID` / `_OBJECT_ID`, authConfig allow-lists, `Asset.Invoke` app-role assignment |

The extractor identity (`hhc-web-api-identity`, service `hhc-web-api`) and the
LINE identity cannot use this route, and configuration rejects a watermark
identity that shares a client or object ID with either.

## Threat analysis: can the principal header be forged?

| Path to the app | Easy Auth in front? | Who controls `X-MS-CLIENT-PRINCIPAL` | Outcome |
|---|---|---|---|
| Internal ingress (other apps in `alive-env`) | Yes | Easy Auth replaces it after validating the token; unauthenticated requests get 401 before the app | Trusted only for configured client+object+role |
| Dapr sidecar → app over localhost | **No** | The calling app | Dapr proof wins first; the header is never read when the token is valid (locked by `TestDerivedBulletinDaprIdentityTakesPrecedenceOverPrincipalHeader`) |
| Public internet | n/a | — | Asset API has internal ingress only; public routes go through api-gateway, which strips identity headers |

Residual risk: a request that reaches the app port with neither a valid Dapr
token nor Easy Auth in front would be judged by its principal header. In
Container Apps the only such entry is the Dapr sidecar, whose requests always
carry the valid token. This is the open "single-point-of-trust header model"
audit item; moving to an in-app JWT validation of the bearer token would remove
the dependency on Easy Auth header replacement.

## Rollout and rollback

- Both paths are accepted until the worker App is retired; the Dapr branch is
  removed in a follow-up after the Event Job has been stable for two weeks.
- Rollback of the workload path: clear the `ASSET_WATERMARK_WORKLOAD_*`
  repository variables and release. The authConfig allow-lists and the
  environment variables are then empty for the worker, and the Dapr path keeps
  working.
