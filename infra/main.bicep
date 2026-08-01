targetScope = 'resourceGroup'

param location string = resourceGroup().location
param containerAppEnvironmentName string = 'alive-env'
param containerRegistryName string = 'alive'
param storageAccountName string
param runtimeKeyVaultName string = 'alive-asset-runtime-kv'
param migrationKeyVaultName string = 'alive-asset-migrate-kv'
@minLength(1)
param runtimeImage string
@minLength(1)
param migrationImage string
param deployRuntime bool = true
param deployMigrationJob bool = true
param provisionPermissions bool = true
param manageSharedInfrastructure bool = true
param scanDispatchEnabled bool = false

param publicBaseUrl string = 'https://www.alive.org.tw/api/assets'
param clamavHost string = '172.16.65.5'
param clamavPort int = 3310
param clamavNetworkSecurityGroupName string = 'bastionnsg235'
param acaSubnetPrefix string = '172.16.66.0/23'
param uploadAllowedOrigins array = [
  'https://admin.alive.org.tw'
  'https://admin-test.alive.org.tw'
]

var keyVaultSecretsUserRole = subscriptionResourceId('Microsoft.Authorization/roleDefinitions', '4633458b-17de-408a-b874-0445c86b69e6')

resource environment 'Microsoft.App/managedEnvironments@2024-03-01' existing = {
  name: containerAppEnvironmentName
}

resource registry 'Microsoft.ContainerRegistry/registries@2023-07-01' existing = {
  name: containerRegistryName
}

resource runtimeVault 'Microsoft.KeyVault/vaults@2023-07-01' = {
  name: runtimeKeyVaultName
  location: location
  properties: {
    tenantId: subscription().tenantId
    sku: {
      family: 'A'
      name: 'standard'
    }
    accessPolicies: []
    enablePurgeProtection: true
    enableRbacAuthorization: true
    enableSoftDelete: true
    softDeleteRetentionInDays: 90
    publicNetworkAccess: 'Enabled'
  }
}

resource migrationVault 'Microsoft.KeyVault/vaults@2023-07-01' = {
  name: migrationKeyVaultName
  location: location
  properties: {
    tenantId: subscription().tenantId
    sku: {
      family: 'A'
      name: 'standard'
    }
    accessPolicies: []
    enablePurgeProtection: true
    enableRbacAuthorization: true
    enableSoftDelete: true
    softDeleteRetentionInDays: 90
    publicNetworkAccess: 'Enabled'
  }
}

resource pullIdentity 'Microsoft.ManagedIdentity/userAssignedIdentities@2023-01-31' = {
  name: 'asset-api-acr-pull'
  location: location
}

resource runtimeIdentity 'Microsoft.ManagedIdentity/userAssignedIdentities@2023-01-31' = {
  name: 'asset-api-runtime-identity'
  location: location
}

resource migrationIdentity 'Microsoft.ManagedIdentity/userAssignedIdentities@2023-01-31' = {
  name: 'asset-migrate-identity'
  location: location
}

resource acrPull 'Microsoft.Authorization/roleAssignments@2022-04-01' = if (provisionPermissions) {
  name: guid(registry.id, pullIdentity.id, 'acr-pull')
  scope: registry
  properties: {
    principalId: pullIdentity.properties.principalId
    principalType: 'ServicePrincipal'
    roleDefinitionId: subscriptionResourceId('Microsoft.Authorization/roleDefinitions', '7f951dda-4ed3-4680-a7ca-43fe172d538d')
  }
}

resource runtimeSecretAccess 'Microsoft.Authorization/roleAssignments@2022-04-01' = if (provisionPermissions) {
  name: guid(runtimeVault.id, runtimeIdentity.id, 'key-vault-secrets-user')
  scope: runtimeVault
  properties: {
    principalId: runtimeIdentity.properties.principalId
    principalType: 'ServicePrincipal'
    roleDefinitionId: keyVaultSecretsUserRole
  }
}

resource migrationSecretAccess 'Microsoft.Authorization/roleAssignments@2022-04-01' = if (provisionPermissions) {
  name: guid(migrationVault.id, migrationIdentity.id, 'key-vault-secrets-user')
  scope: migrationVault
  properties: {
    principalId: migrationIdentity.properties.principalId
    principalType: 'ServicePrincipal'
    roleDefinitionId: keyVaultSecretsUserRole
  }
}

resource storageAccount 'Microsoft.Storage/storageAccounts@2023-05-01' existing = {
  name: storageAccountName
}

resource queueService 'Microsoft.Storage/storageAccounts/queueServices@2023-05-01' = if (manageSharedInfrastructure) {
  parent: storageAccount
  name: 'default'
}

resource scanQueue 'Microsoft.Storage/storageAccounts/queueServices/queues@2023-05-01' = if (manageSharedInfrastructure) {
  parent: queueService
  name: 'asset-scan'
}

resource clamavNetworkSecurityGroup 'Microsoft.Network/networkSecurityGroups@2024-05-01' existing = {
  name: clamavNetworkSecurityGroupName
}

resource allowACAtoClamAV 'Microsoft.Network/networkSecurityGroups/securityRules@2024-05-01' = if (manageSharedInfrastructure) {
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

resource denyOtherVNetClamAV 'Microsoft.Network/networkSecurityGroups/securityRules@2024-05-01' = if (manageSharedInfrastructure) {
  parent: clamavNetworkSecurityGroup
  name: 'DenyOtherVNetToClamAV'
  properties: {
    priority: 340
    access: 'Deny'
    direction: 'Inbound'
    protocol: 'Tcp'
    sourcePortRange: '*'
    destinationPortRange: string(clamavPort)
    sourceAddressPrefix: 'VirtualNetwork'
    destinationAddressPrefix: clamavHost
  }
}

resource blobService 'Microsoft.Storage/storageAccounts/blobServices@2023-05-01' = if (manageSharedInfrastructure) {
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

resource assetContainer 'Microsoft.Storage/storageAccounts/blobServices/containers@2023-05-01' = if (manageSharedInfrastructure) {
  parent: blobService
  name: 'assets'
  properties: {
    publicAccess: 'None'
    defaultEncryptionScope: '$account-encryption-key'
    denyEncryptionScopeOverride: false
  }
}

resource defenderForStorageDisabled 'Microsoft.Security/defenderForStorageSettings@2025-01-01' = if (manageSharedInfrastructure) {
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

resource app 'Microsoft.App/containerApps@2024-03-01' = if (deployRuntime) {
  name: 'asset-api'
  location: location
  identity: {
    type: 'SystemAssigned, UserAssigned'
    userAssignedIdentities: {
      '${pullIdentity.id}': {}
      '${runtimeIdentity.id}': {}
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
          keyVaultUrl: '${runtimeVault.properties.vaultUri}secrets/database-url'
          identity: runtimeIdentity.id
        }
      ]
    }
    template: {
      containers: [
        {
          name: 'asset-api'
          image: runtimeImage
          env: [
            { name: 'PORT', value: '8080' }
            { name: 'DATABASE_URL', secretRef: 'database-url' }
            { name: 'DB_MAX_OPEN_CONNS', value: '4' }
            { name: 'DB_MAX_IDLE_CONNS', value: '2' }
            { name: 'DB_CONN_MAX_LIFETIME', value: '30m' }
            { name: 'ASSET_PUBLIC_BASE_URL', value: publicBaseUrl }
            { name: 'ASSET_STORAGE_BACKEND', value: 'azure' }
            { name: 'ASSET_AZURE_ACCOUNT_URL', value: 'https://${storageAccount.name}.blob.${az.environment().suffixes.storage}' }
            { name: 'ASSET_AZURE_CONTAINER', value: 'assets' }
            { name: 'ASSET_SCAN_QUEUE_URL', value: 'https://${storageAccount.name}.queue.${az.environment().suffixes.storage}/asset-scan' }
            { name: 'ASSET_SCAN_DISPATCH_ENABLED', value: string(scanDispatchEnabled) }
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
    runtimeSecretAccess
  ]
}

resource existingApp 'Microsoft.App/containerApps@2024-03-01' existing = if (!deployRuntime) {
  name: 'asset-api'
}

resource assetBlobContributor 'Microsoft.Authorization/roleAssignments@2022-04-01' = if (deployRuntime && manageSharedInfrastructure) {
  name: guid(assetContainer!.id, app!.id, 'storage-blob-data-contributor')
  scope: assetContainer!
  properties: {
    principalId: app!.identity.principalId
    principalType: 'ServicePrincipal'
    roleDefinitionId: subscriptionResourceId('Microsoft.Authorization/roleDefinitions', 'ba92f5b4-2d11-453d-a403-e96b0029c9fe')
  }
}

resource assetBlobDelegator 'Microsoft.Authorization/roleAssignments@2022-04-01' = if (deployRuntime && manageSharedInfrastructure) {
  name: guid(storageAccount.id, app!.id, 'storage-blob-delegator')
  scope: storageAccount
  properties: {
    principalId: app!.identity.principalId
    principalType: 'ServicePrincipal'
    roleDefinitionId: subscriptionResourceId('Microsoft.Authorization/roleDefinitions', 'db58b8e5-c6ad-4a2a-8342-4190687cbf4a')
  }
}

resource assetQueueSender 'Microsoft.Authorization/roleAssignments@2022-04-01' = if (manageSharedInfrastructure) {
  name: guid(scanQueue!.id, 'asset-api', 'storage-queue-data-message-sender')
  scope: scanQueue!
  properties: {
    principalId: deployRuntime ? app!.identity.principalId : existingApp!.identity.principalId
    principalType: 'ServicePrincipal'
    roleDefinitionId: subscriptionResourceId('Microsoft.Authorization/roleDefinitions', 'c6a89b2d-59bc-44d0-9896-0f6e12d7b80a')
  }
}

resource migrate 'Microsoft.App/jobs@2024-03-01' = if (deployMigrationJob) {
  name: 'asset-migrate'
  location: location
  identity: {
    type: 'UserAssigned'
    userAssignedIdentities: {
      '${pullIdentity.id}': {}
      '${migrationIdentity.id}': {}
    }
  }
  properties: {
    environmentId: environment.id
    workloadProfileName: 'Consumption'
    configuration: {
      triggerType: 'Manual'
      replicaTimeout: 300
      replicaRetryLimit: 0
      manualTriggerConfig: {
        parallelism: 1
        replicaCompletionCount: 1
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
          keyVaultUrl: '${migrationVault.properties.vaultUri}secrets/database-url'
          identity: migrationIdentity.id
        }
      ]
    }
    template: {
      containers: [
        {
          name: 'asset-migrate'
          image: migrationImage
          command: ['/asset-migrate']
          env: [
            { name: 'DATABASE_URL', secretRef: 'database-url' }
          ]
          resources: {
            cpu: json('0.25')
            memory: '0.5Gi'
          }
        }
      ]
    }
  }
  dependsOn: [
    acrPull
    migrationSecretAccess
  ]
}

output containerAppName string = 'asset-api'
output migrationJobName string = 'asset-migrate'
output assetContainerName string = 'assets'
output scanQueueName string = 'asset-scan'
