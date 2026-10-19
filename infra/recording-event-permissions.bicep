targetScope = 'resourceGroup'
// One-time operator bootstrap, separately reviewed from ordinary Contributor releases.
param storageAccountName string
param workerPrincipalId string
param senderPrincipalId string
var lanes = [ 'live', 'processing', 'preview' ]
resource account 'Microsoft.Storage/storageAccounts@2023-05-01' existing = { name: storageAccountName }
resource service 'Microsoft.Storage/storageAccounts/queueServices@2023-05-01' existing = { parent: account, name: 'default' }
resource queues 'Microsoft.Storage/storageAccounts/queueServices/queues@2023-05-01' existing = [for lane in lanes: {
 parent: service
 name: 'asset-recording-${lane}'
}]
resource poison 'Microsoft.Storage/storageAccounts/queueServices/queues@2023-05-01' existing = [for lane in lanes: {
 parent: service
 name: 'asset-recording-${lane}-poison'
}]
// Azure UpdateMessage needs messages/write; Processor cannot renew visibility.
// This DataAction also permits message content writes, but no queue management.
resource messageWriteRole 'Microsoft.Authorization/roleDefinitions@2022-04-01' = {
 name: 'bf5dee66-37ae-4ef4-b34a-9f135e10d804'
 properties: {
  roleName: 'HHC Recording Queue Message Write'
  description: 'Update recording work queue messages for renewable processing leases.'
  type: 'CustomRole'
  assignableScopes: [resourceGroup().id]
  permissions: [{
   actions: []
   notActions: []
   dataActions: ['Microsoft.Storage/storageAccounts/queueServices/queues/messages/write']
   notDataActions: []
  }]
 }
}
resource messageWriter 'Microsoft.Authorization/roleAssignments@2022-04-01' = [for (lane,i) in lanes: {
 name: guid(queues[i].id,workerPrincipalId,'message-write')
 scope: queues[i]
 properties: { principalId: workerPrincipalId, principalType: 'ServicePrincipal', roleDefinitionId: messageWriteRole.id }
}]
resource sender 'Microsoft.Authorization/roleAssignments@2022-04-01' = [for (lane,i) in lanes: {
 name: guid(queues[i].id,senderPrincipalId,'sender')
 scope: queues[i]
 properties: { principalId: senderPrincipalId, principalType: 'ServicePrincipal', roleDefinitionId: subscriptionResourceId('Microsoft.Authorization/roleDefinitions','c6a89b2d-59bc-44d0-9896-0f6e12d7b80a') }
}]
resource processor 'Microsoft.Authorization/roleAssignments@2022-04-01' = [for (lane,i) in lanes: {
 name: guid(queues[i].id,workerPrincipalId,'processor')
 scope: queues[i]
 properties: { principalId: workerPrincipalId, principalType: 'ServicePrincipal', roleDefinitionId: subscriptionResourceId('Microsoft.Authorization/roleDefinitions','8a0f0c08-91a1-4084-bc3d-661d67233fed') }
}]
resource reader 'Microsoft.Authorization/roleAssignments@2022-04-01' = [for (lane,i) in lanes: {
 name: guid(queues[i].id,workerPrincipalId,'reader')
 scope: queues[i]
 properties: { principalId: workerPrincipalId, principalType: 'ServicePrincipal', roleDefinitionId: subscriptionResourceId('Microsoft.Authorization/roleDefinitions','19e7f393-937e-4f77-808e-94535e297925') }
}]
resource poisonSender 'Microsoft.Authorization/roleAssignments@2022-04-01' = [for (lane,i) in lanes: {
 name: guid(poison[i].id,workerPrincipalId,'sender')
 scope: poison[i]
 properties: { principalId: workerPrincipalId, principalType: 'ServicePrincipal', roleDefinitionId: subscriptionResourceId('Microsoft.Authorization/roleDefinitions','c6a89b2d-59bc-44d0-9896-0f6e12d7b80a') }
}]
