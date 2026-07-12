targetScope = 'resourceGroup'

@description('Existing StorageV2 account used exclusively through asset-api.')
param storageAccountName string

@description('Object id of the asset-api managed identity.')
param assetApiPrincipalId string

@description('Allowed browser origins for one-blob direct uploads.')
param uploadAllowedOrigins array = [
  'https://admin.alive.org.tw'
]

param location string = resourceGroup().location
param serviceBusNamespaceName string = 'hhc-asset-${uniqueString(resourceGroup().id)}'
param scanResultsTopicName string = 'hhc-asset-scan-results'
param scanResultsQueueName string = 'asset-malware-scan-results'
param defenderScanCapGBPerMonth int = 10

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

resource serviceBus 'Microsoft.ServiceBus/namespaces@2024-01-01' = {
  name: serviceBusNamespaceName
  location: location
  sku: {
    name: 'Standard'
    tier: 'Standard'
  }
  properties: {
    disableLocalAuth: true
    minimumTlsVersion: '1.2'
    publicNetworkAccess: 'Enabled'
    zoneRedundant: false
  }
}

resource scanQueue 'Microsoft.ServiceBus/namespaces/queues@2024-01-01' = {
  parent: serviceBus
  name: scanResultsQueueName
  properties: {
    deadLetteringOnMessageExpiration: true
    defaultMessageTimeToLive: 'P7D'
    lockDuration: 'PT1M'
    maxDeliveryCount: 10
    maxSizeInMegabytes: 1024
    requiresDuplicateDetection: true
    duplicateDetectionHistoryTimeWindow: 'PT10M'
  }
}

resource scanResultsTopic 'Microsoft.EventGrid/topics@2025-02-15' = {
  name: scanResultsTopicName
  location: location
  identity: {
    type: 'SystemAssigned'
  }
  properties: {
    disableLocalAuth: true
    inputSchema: 'EventGridSchema'
    publicNetworkAccess: 'Enabled'
  }
}

resource scanTopicSender 'Microsoft.Authorization/roleAssignments@2022-04-01' = {
  name: guid(scanQueue.id, scanResultsTopic.id, 'service-bus-data-sender')
  scope: scanQueue
  properties: {
    principalId: scanResultsTopic.identity.principalId
    principalType: 'ServicePrincipal'
    roleDefinitionId: subscriptionResourceId('Microsoft.Authorization/roleDefinitions', '69a216fc-b8fb-44d8-bc22-1f3c2cd27a39')
  }
}

resource scanResultSubscription 'Microsoft.EventGrid/topics/eventSubscriptions@2025-02-15' = {
  parent: scanResultsTopic
  name: 'asset-api-scan-results'
  properties: {
    deliveryWithResourceIdentity: {
      identity: {
        type: 'SystemAssigned'
      }
      destination: {
        endpointType: 'ServiceBusQueue'
        properties: {
          resourceId: scanQueue.id
        }
      }
    }
    eventDeliverySchema: 'EventGridSchema'
    retryPolicy: {
      eventTimeToLiveInMinutes: 1440
      maxDeliveryAttempts: 30
    }
  }
  dependsOn: [
    scanTopicSender
  ]
}

resource defenderForStorage 'Microsoft.Security/defenderForStorageSettings@2025-06-01' = {
  scope: storageAccount
  name: 'current'
  properties: {
    isEnabled: true
    overrideSubscriptionLevelSettings: true
    malwareScanning: {
      blobScanResultsOptions: 'BlobIndexTags'
      onUpload: {
        capGBPerMonth: defenderScanCapGBPerMonth
        isEnabled: true
      }
      scanResultsEventGridTopicResourceId: scanResultsTopic.id
    }
    sensitiveDataDiscovery: {
      isEnabled: false
    }
  }
  dependsOn: [
    scanResultSubscription
  ]
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

resource assetScanReceiver 'Microsoft.Authorization/roleAssignments@2022-04-01' = {
  name: guid(scanQueue.id, assetApiPrincipalId, 'service-bus-data-receiver')
  scope: scanQueue
  properties: {
    principalId: assetApiPrincipalId
    principalType: 'ServicePrincipal'
    roleDefinitionId: subscriptionResourceId('Microsoft.Authorization/roleDefinitions', '090c5cfd-751d-490a-894a-3ce6f1109419')
  }
}

output assetContainerName string = assetContainer.name
output serviceBusNamespace string = '${serviceBus.name}.servicebus.windows.net'
output serviceBusQueue string = scanQueue.name
output scanCapGBPerMonth int = defenderScanCapGBPerMonth
