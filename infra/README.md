# Asset infrastructure

`main.bicep` configures the existing asset storage account and creates the scan-result transport:

- private `assets` Blob container
- CORS restricted to the Admin origins and `PUT`/`OPTIONS`
- Defender for Storage on-upload malware scanning with a 10 GB monthly cap
- Blob index-tag scan results
- Event Grid custom topic with managed identity
- Service Bus queue with duplicate detection and dead-lettering
- least-privilege storage, user-delegation, sender, and receiver role assignments

The asset-api managed identity needs both Storage Blob Data Contributor and Storage Blob Delegator. Shared Key and Service Bus local authentication are not used. The Event Grid topic must retain public network access because Defender scan-result delivery does not currently support a private Event Grid topic.

The stable Defender API version used here does not enable automated malicious-blob deletion. `asset-api` denies infected assets immediately while retaining metadata and scan evidence for incident review.

Validate and deploy:

```sh
az bicep build --file infra/main.bicep
az deployment group what-if \
  --resource-group "$RESOURCE_GROUP" \
  --template-file infra/main.bicep \
  --parameters infra/main.bicepparam
```

The monthly cap controls scanning spend, not storage capacity. When the cap is reached, new uploads remain unavailable because asset-api requires a persisted `clean` result before any download.
