# Asset infrastructure

`main.bicep` configures the existing asset storage account with:

- a private `assets` Blob container
- CORS restricted to Admin origins and `PUT`/`OPTIONS`
- least-privilege Blob contributor and user-delegation roles for asset-api

Malware scanning is performed by the private ClamAV service configured through `CLAMAV_HOST` and `CLAMAV_PORT`. Event Grid and the scan-result Service Bus queue are not provisioned. The template explicitly disables Defender for this storage account and overrides a subscription-level plan so on-upload scanning is not billed.

The asset-api runtime network must be able to reach clamd on private TCP port `3310`; do not add a public ingress rule for clamd.

Validate and deploy:

```sh
az bicep build --file infra/main.bicep
az deployment group what-if \
  --resource-group "$RESOURCE_GROUP" \
  --template-file infra/main.bicep \
  --parameters infra/main.bicepparam
```
