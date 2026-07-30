targetScope = 'resourceGroup'

param location string = resourceGroup().location
param containerAppEnvironmentName string = 'alive-env'
param containerRegistryName string = 'alive'
param storageAccountName string
param image string

@secure()
param databaseUrl string

param publicBaseUrl string = 'https://www.alive.org.tw/api/assets'
param clamavHost string = '172.16.65.5'
param clamavPort int = 3310
param clamavNetworkSecurityGroupName string = 'bastionnsg235'
param acaSubnetPrefix string = '172.16.66.0/23'
param uploadAllowedOrigins array = [
  'https://admin.alive.org.tw'
  'https://admin-test.alive.org.tw'
]

resource environment 'Microsoft.App/managedEnvironments@2024-03-01' existing = {
  name: containerAppEnvironmentName
}

resource registry 'Microsoft.ContainerRegistry/registries@2023-07-01' existing = {
  name: containerRegistryName
}

resource pullIdentity 'Microsoft.ManagedIdentity/userAssignedIdentities@2023-01-31' = {
  name: 'asset-api-acr-pull'
  location: location
}

resource acrPull 'Microsoft.Authorization/roleAssignments@2022-04-01' = {
  name: guid(registry.id, pullIdentity.id, 'acr-pull')
  scope: registry
  properties: {
    principalId: pullIdentity.properties.principalId
    principalType: 'ServicePrincipal'
    roleDefinitionId: subscriptionResourceId('Microsoft.Authorization/roleDefinitions', '7f951dda-4ed3-4680-a7ca-43fe172d538d')
  }
}

resource storageAccount 'Microsoft.Storage/storageAccounts@2023-05-01' existing = {
  name: storageAccountName
}

resource clamavNetworkSecurityGroup 'Microsoft.Network/networkSecurityGroups@2024-05-01' existing = {
  name: clamavNetworkSecurityGroupName
}

resource allowACAtoClamAV 'Microsoft.Network/networkSecurityGroups/securityRules@2024-05-01' = {
  parent: clamavNetworkSecurityGroup
  name: 'AllowACAtoClamAV'
  properties: {
    priority: 330
    access: 'Allow'
    direction: 'Inbound'
    protocol: 'Tcp'
    sourcePortRange: '*'
    destinationPortRange: string(clamavPort)
    sourceAddressPrefix: acaSubnetPrefix
    destinationAddressPrefix: clamavHost
  }
}

resource blobService 'Microsoft.Storage/storageAccounts/blobServices@2023-05-01' = {
  parent: storageAccount
  name: 'default'
  properties: {
    deleteRetentionPolicy: {
      enabled: false
      allowPermanentDelete: false
    }
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

resource assetContainer 'Microsoft.Storage/storageAccounts/blobServices/containers@2023-05-01' = {
  parent: blobService
  name: 'assets'
  properties: {
    publicAccess: 'None'
    defaultEncryptionScope: '$account-encryption-key'
    denyEncryptionScopeOverride: false
  }
}

resource defenderForStorageDisabled 'Microsoft.Security/defenderForStorageSettings@2025-01-01' = {
  scope: storageAccount
  name: 'current'
  properties: {
    isEnabled: false
    overrideSubscriptionLevelSettings: true
    malwareScanning: {
      onUpload: {
        isEnabled: false
        capGBPerMonth: -1
      }
    }
    sensitiveDataDiscovery: {
      isEnabled: false
    }
  }
}

resource app 'Microsoft.App/containerApps@2024-03-01' = {
  name: 'asset-api'
  location: location
  identity: {
    type: 'SystemAssigned, UserAssigned'
    userAssignedIdentities: {
      '${pullIdentity.id}': {}
    }
  }
  properties: {
    managedEnvironmentId: environment.id
    workloadProfileName: 'Consumption'
    configuration: {
      activeRevisionsMode: 'Single'
      maxInactiveRevisions: 100
      dapr: {
        enabled: true
        appId: 'asset-api'
        appPort: 8080
        appProtocol: 'http'
        logLevel: 'warn'
      }
      registries: [
        {
          server: registry.properties.loginServer
          identity: pullIdentity.id
        }
      ]
      secrets: [
        {
          name: 'database-url'
          value: databaseUrl
        }
      ]
    }
    template: {
      containers: [
        {
          name: 'asset-api'
          image: image
          env: [
            { name: 'PORT', value: '8080' }
            { name: 'DATABASE_URL', secretRef: 'database-url' }
            { name: 'DB_MAX_OPEN_CONNS', value: '4' }
            { name: 'DB_MAX_IDLE_CONNS', value: '2' }
            { name: 'DB_CONN_MAX_LIFETIME', value: '30m' }
            { name: 'ASSET_PUBLIC_BASE_URL', value: publicBaseUrl }
            { name: 'ASSET_STORAGE_BACKEND', value: 'azure' }
            { name: 'ASSET_AZURE_ACCOUNT_URL', value: 'https://${storageAccount.name}.blob.${az.environment().suffixes.storage}' }
            { name: 'ASSET_AZURE_CONTAINER', value: assetContainer.name }
            { name: 'ASSET_ALLOWED_CALLERS', value: 'account-api,hhc-web-api,hhc-line-function-bot' }
            { name: 'ASSET_ALLOW_DEV_CALLER_HEADER', value: 'false' }
            { name: 'CLAMAV_HOST', value: clamavHost }
            { name: 'CLAMAV_PORT', value: string(clamavPort) }
            { name: 'CLAMAV_TIMEOUT_SECONDS', value: '120' }
            { name: 'CLAMAV_MAX_FILE_SIZE_BYTES', value: '26214400' }
            { name: 'CLAMAV_MAX_RETRIES', value: '5' }
          ]
          resources: {
            cpu: json('0.5')
            memory: '1Gi'
          }
          probes: [
            {
              type: 'Liveness'
              httpGet: { path: '/health', port: 8080 }
              initialDelaySeconds: 5
              periodSeconds: 30
            }
            {
              type: 'Readiness'
              httpGet: { path: '/ready', port: 8080 }
              initialDelaySeconds: 5
              periodSeconds: 10
            }
          ]
        }
      ]
      scale: {
        minReplicas: 1
        maxReplicas: 3
      }
    }
  }
  dependsOn: [
    acrPull
  ]
}

resource assetBlobContributor 'Microsoft.Authorization/roleAssignments@2022-04-01' = {
  name: guid(assetContainer.id, app.id, 'storage-blob-data-contributor')
  scope: assetContainer
  properties: {
    principalId: app.identity.principalId
    principalType: 'ServicePrincipal'
    roleDefinitionId: subscriptionResourceId('Microsoft.Authorization/roleDefinitions', 'ba92f5b4-2d11-453d-a403-e96b0029c9fe')
  }
}

resource assetBlobDelegator 'Microsoft.Authorization/roleAssignments@2022-04-01' = {
  name: guid(storageAccount.id, app.id, 'storage-blob-delegator')
  scope: storageAccount
  properties: {
    principalId: app.identity.principalId
    principalType: 'ServicePrincipal'
    roleDefinitionId: subscriptionResourceId('Microsoft.Authorization/roleDefinitions', 'db58b8e5-c6ad-4a2a-8342-4190687cbf4a')
  }
}

output containerAppName string = app.name
output principalId string = app.identity.principalId
output assetContainerName string = assetContainer.name
