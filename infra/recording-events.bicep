targetScope = 'resourceGroup'
param location string
param environmentId string
param storageAccountName string
param registryServer string
param image string
param pullIdentityId string
param workerIdentityId string
param workerClientId string
param databaseSecretUrl string
param secrets array
param processingEnv array
param r2Env array
param enabled bool = false
@allowed(['1.5', '2', '4'])
param liveCPU string = '4'
@allowed([2, 4])
param processingCPU int = 2
@allowed([1, 4])
param previewCPU int = 1

resource account 'Microsoft.Storage/storageAccounts@2023-05-01' existing = { name: storageAccountName }
resource service 'Microsoft.Storage/storageAccounts/queueServices@2023-05-01' existing = { parent: account, name: 'default' }
var lanes = [ 'live', 'processing', 'preview' ]
resource queues 'Microsoft.Storage/storageAccounts/queueServices/queues@2023-05-01' = [for lane in lanes: {
 parent: service
 name: 'asset-recording-${lane}'
}]
resource poison 'Microsoft.Storage/storageAccounts/queueServices/queues@2023-05-01' = [for lane in lanes: {
 parent: service
 name: 'asset-recording-${lane}-poison'
}]
var queueEnvGroups = [for lane in lanes: [
 { name: 'ASSET_RECORDING_${toUpper(lane)}_QUEUE_URL', value: 'https://${storageAccountName}.queue.${az.environment().suffixes.storage}/asset-recording-${lane}' }
 { name: 'ASSET_RECORDING_${toUpper(lane)}_POISON_URL', value: 'https://${storageAccountName}.queue.${az.environment().suffixes.storage}/asset-recording-${lane}-poison' }
]]
var queueEnv = flatten(queueEnvGroups)
var jobEnv = concat(r2Env,processingEnv,queueEnv,[
 { name: 'DATABASE_URL', secretRef: 'database-url' }
 { name: 'AZURE_CLIENT_ID', value: workerClientId }
])
resource jobs 'Microsoft.App/jobs@2025-07-01' = [for lane in lanes: {
 name: 'asset-recording-${lane}'
 location: location
 identity: { type: 'UserAssigned', userAssignedIdentities: { '${pullIdentityId}': {}, '${workerIdentityId}': {} } }
 properties: {
  environmentId: environmentId
  workloadProfileName: 'Consumption'
  configuration: {
   triggerType: enabled ? 'Event' : 'Manual'
   replicaTimeout: 21600
   replicaRetryLimit: 0
   manualTriggerConfig: enabled ? null : { parallelism: 1, replicaCompletionCount: 1 }
   eventTriggerConfig: enabled ? {
    parallelism: 1
    replicaCompletionCount: 1
    scale: {
     pollingInterval: 10
     minExecutions: 0
     maxExecutions: 1
     rules: [{ name: 'recording-${lane}', type: 'azure-queue', identity: workerIdentityId, metadata: { accountName: storageAccountName, queueName: 'asset-recording-${lane}', queueLength: '1', queueLengthStrategy: 'visibleonly' } }]
    }
   } : null
   registries: [{ server: registryServer, identity: pullIdentityId }]
   secrets: concat([{name: 'database-url',keyVaultUrl: databaseSecretUrl,identity:workerIdentityId}],secrets)
  }
  template: {
   containers: [{
    name: 'recording-${lane}'
    image: image
    env: concat(jobEnv,[{name:'ASSET_RECORDING_WORKER_MODE',value:lane}])
    resources: {
     cpu: lane=='live' ? json(liveCPU) : lane=='processing' ? json(string(processingCPU)) : json(string(previewCPU))
     memory: lane=='live' && liveCPU=='1.5' ? '3Gi' : lane=='preview' && previewCPU==1 ? '2Gi' : (lane=='live' && liveCPU=='2') || (lane=='processing' && processingCPU==2) ? '4Gi' : '8Gi'
    }
   }]
  }
 }
 dependsOn: [queues,poison]
}]
resource maintenance 'Microsoft.App/jobs@2025-07-01' = {
 name: 'asset-recording-maintenance'
 location: location
 identity: { type:'UserAssigned',userAssignedIdentities:{'${pullIdentityId}':{},'${workerIdentityId}':{}} }
 properties: {
  environmentId: environmentId
  workloadProfileName:'Consumption'
  configuration: {
   triggerType: enabled ? 'Schedule' : 'Manual'
   replicaTimeout: 360
   replicaRetryLimit: 0
   manualTriggerConfig: enabled ? null : {parallelism:1,replicaCompletionCount:1}
   scheduleTriggerConfig: enabled ? {cronExpression: '17 * * * *',parallelism:1,replicaCompletionCount:1} : null
   registries:[{server:registryServer,identity:pullIdentityId}]
   secrets:concat([{name:'database-url',keyVaultUrl:databaseSecretUrl,identity:workerIdentityId}],secrets)
  }
  template:{containers:[{
   name:'recording-maintenance'
   image:image
   env:concat(jobEnv,[{name:'ASSET_RECORDING_WORKER_MODE',value:'maintenance'}])
   resources:{cpu:json('0.5'),memory:'1Gi'}
  }]}
 }
}
