targetScope = 'resourceGroup'

@description('Existing StorageV2 account used exclusively through asset-api.')
param storageAccountName string

@description('Object id of the asset-api managed identity.')
param assetApiPrincipalId string

@description('Allowed browser origins for one-blob direct uploads.')
param uploadAllowedOrigins array = [
  'https://admin.alive.org.tw'
]

resource storageAccount 'Microsoft.Storage/storageAccounts@2025-06-01' existing = {
  name: storageAccountName
}

resource blobService 'Microsoft.Storage/storageAccounts/blobServices@2025-06-01' = {
  parent: storageAccount
  name: 'default'
  properties: {
    cors: {
      corsRules: [
        {
          allowedHeaders: [
            'content-type'
            'x-ms-blob-type'
            'x-ms-version'
          ]
          allowedMethods: [
            'PUT'
            'OPTIONS'
          ]
          allowedOrigins: uploadAllowedOrigins
          exposedHeaders: [
            'etag'
            'x-ms-request-id'
          ]
          maxAgeInSeconds: 600
        }
      ]
    }
  }
}

resource assetContainer 'Microsoft.Storage/storageAccounts/blobServices/containers@2025-06-01' = {
  parent: blobService
  name: 'assets'
  properties: {
    publicAccess: 'None'
  }
}

// Explicitly override any subscription-level plan so this storage account is
// not billed for Defender malware scanning.
resource defenderForStorageDisabled 'Microsoft.Security/defenderForStorageSettings@2025-01-01' = {
  scope: storageAccount
  name: 'current'
  properties: {
    isEnabled: false
    overrideSubscriptionLevelSettings: true
  }
}

resource assetBlobContributor 'Microsoft.Authorization/roleAssignments@2022-04-01' = {
  name: guid(storageAccount.id, assetApiPrincipalId, 'storage-blob-data-contributor')
  scope: storageAccount
  properties: {
    principalId: assetApiPrincipalId
    principalType: 'ServicePrincipal'
    roleDefinitionId: subscriptionResourceId('Microsoft.Authorization/roleDefinitions', 'ba92f5b4-2d11-453d-a403-e96b0029c9fe')
  }
}

resource assetBlobDelegator 'Microsoft.Authorization/roleAssignments@2022-04-01' = {
  name: guid(storageAccount.id, assetApiPrincipalId, 'storage-blob-delegator')
  scope: storageAccount
  properties: {
    principalId: assetApiPrincipalId
    principalType: 'ServicePrincipal'
    roleDefinitionId: subscriptionResourceId('Microsoft.Authorization/roleDefinitions', 'db58b8e5-c6ad-4a2a-8342-4190687cbf4a')
  }
}

output assetContainerName string = assetContainer.name
