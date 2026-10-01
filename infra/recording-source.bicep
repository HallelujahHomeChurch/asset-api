targetScope = 'resourceGroup'

// Deliberately separate from ordinary asset storage, scanning and retention.
param location string = resourceGroup().location
param storageAccountName string = 'aliverecordingsprod'

resource runtimeIdentity 'Microsoft.ManagedIdentity/userAssignedIdentities@2023-01-31' existing = {
  name: 'asset-api-runtime-identity'
}

resource app 'Microsoft.App/containerApps@2025-01-01' existing = {
  name: 'asset-api'
}

// API keeps its existing system-assigned identity for ordinary assets. Only
// the dedicated recording Job selects the runtime user-assigned identity.
var sourcePrincipals = [app.identity.principalId, runtimeIdentity.properties.principalId]

resource sourceStorage 'Microsoft.Storage/storageAccounts@2023-05-01' = {
  name: storageAccountName
  location: location
  kind: 'StorageV2'
  sku: { name: 'Standard_LRS' }
  properties: {
    accessTier: 'Hot'
    allowBlobPublicAccess: false
    allowSharedKeyAccess: false
    defaultToOAuthAuthentication: true
    supportsHttpsTrafficOnly: true
    minimumTlsVersion: 'TLS1_2'
    publicNetworkAccess: 'Enabled'
    networkAcls: { bypass: 'None', defaultAction: 'Allow', ipRules: [], virtualNetworkRules: [] }
    encryption: { keySource: 'Microsoft.Storage', services: { blob: { enabled: true, keyType: 'Account' } } }
  }
}

resource blob 'Microsoft.Storage/storageAccounts/blobServices@2023-05-01' = {
  parent: sourceStorage
  name: 'default'
  properties: {
    isVersioningEnabled: false
    deleteRetentionPolicy: { enabled: false }
    containerDeleteRetentionPolicy: { enabled: false }
    cors: {
      corsRules: [{
        allowedOrigins: ['https://admin.alive.org.tw']
        allowedMethods: ['PUT', 'OPTIONS']
        allowedHeaders: ['content-type', 'content-md5', 'x-ms-*', 'if-match']
        exposedHeaders: ['ETag', 'x-ms-request-id']
        maxAgeInSeconds: 300
      }]
    }
  }
}

resource sources 'Microsoft.Storage/storageAccounts/blobServices/containers@2023-05-01' = {
  parent: blob
  name: 'recording-sources'
  properties: { publicAccess: 'None' }
}

resource lifecycle 'Microsoft.Storage/storageAccounts/managementPolicies@2023-05-01' = {
  parent: sourceStorage
  name: 'default'
  properties: {
    policy: {
      rules: [{
        enabled: true
        name: 'recording-source-safety-net'
        type: 'Lifecycle'
        definition: {
          filters: { blobTypes: ['blockBlob'], prefixMatch: ['recording-sources/'] }
          // Application cleanup is authoritative: ready <=24h, failed 7d
          // after first completion. Nine days also accommodates a 24h upload.
          actions: { baseBlob: { delete: { daysAfterModificationGreaterThan: 9 } } }
        }
      }]
    }
  }
}

resource noSourceScanning 'Microsoft.Security/defenderForStorageSettings@2022-12-01-preview' = {
  name: 'current'
  scope: sourceStorage
  properties: {
    isEnabled: false
    overrideSubscriptionLevelSettings: true
    malwareScanning: { onUpload: { isEnabled: false } }
    sensitiveDataDiscovery: { isEnabled: false }
  }
}

resource sourceAccess 'Microsoft.Authorization/roleAssignments@2022-04-01' = [for (principalName, i) in ['api', 'job']: {
  name: guid(sources.id, principalName, 'source-blob-contributor')
  scope: sources
  properties: {
    principalId: sourcePrincipals[i]
    principalType: 'ServicePrincipal'
    roleDefinitionId: subscriptionResourceId('Microsoft.Authorization/roleDefinitions', 'ba92f5b4-2d11-453d-a403-e96b0029c9fe')
  }
}]

resource sourceDelegation 'Microsoft.Authorization/roleAssignments@2022-04-01' = [for (principalName, i) in ['api', 'job']: {
  name: guid(sourceStorage.id, principalName, 'source-blob-delegator')
  scope: sourceStorage
  properties: {
    principalId: sourcePrincipals[i]
    principalType: 'ServicePrincipal'
    roleDefinitionId: subscriptionResourceId('Microsoft.Authorization/roleDefinitions', 'db58b8e5-c6ad-4a2a-8342-4190687cbf4a')
  }
}]

output sourceAccountURL string = sourceStorage.properties.primaryEndpoints.blob
output sourceContainer string = sources.name
