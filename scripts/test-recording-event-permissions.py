from pathlib import Path
import json, subprocess

# Ordinary Contributor releases must not submit role-assignment writes.
normal=json.loads(subprocess.check_output(['az','bicep','build','--file','infra/recording-events.bicep','--stdout'],text=True))
assert not any(r['type']=='Microsoft.Authorization/roleAssignments' for r in normal['resources']), 'ordinary recording deployment still requires IAM write'
bootstrap=json.loads(subprocess.check_output(['az','bicep','build','--file','infra/recording-event-permissions.bicep','--stdout'],text=True))
roles=[r for r in bootstrap['resources'] if r['type']=='Microsoft.Authorization/roleAssignments']
assert len(roles)==5 and all(r['copy']['count']=="[length(variables('lanes'))]" for r in roles), 'bootstrap must manage exactly 15 scoped grants'
definitions=[r for r in bootstrap['resources'] if r['type']=='Microsoft.Authorization/roleDefinitions']
assert len(definitions)==1
assert definitions[0]['properties']['permissions']==[{'actions':[],'notActions':[],'dataActions':['Microsoft.Storage/storageAccounts/queueServices/queues/messages/write'],'notDataActions':[]}]
assert not any(r['type']=='Microsoft.App/jobs' for r in bootstrap['resources']), 'bootstrap must not deploy or start workers'
assert 'verify-recording-event-permissions.sh' in Path('.github/workflows/release.yml').read_text(), 'activation must verify prerequisites'

# Exercise the activation preflight against an Azure CLI fixture, including failures.
import os, tempfile
with tempfile.TemporaryDirectory() as temp:
    fake=Path(temp)/'az'
    fake.write_text('''#!/usr/bin/env python3
import json,os,sys
args=sys.argv[1:]
if args[:2]==['containerapp','show']:print('api')
elif args[:2]==['identity','show']:print('worker')
elif args[:3]==['storage','account','show']:print('/subscriptions/sub/resourceGroups/alive/providers/Microsoft.Storage/storageAccounts/storage')
elif args[:2]==['resource','show']:pass
elif args[:3]==['role','definition','list']:
 if os.environ.get('FAIL_DEFINITION_QUERY'):sys.exit(1)
 if '--scope' not in args or args[args.index('--scope')+1]!='/subscriptions/sub/resourceGroups/alive':
  print('[]');sys.exit(0)
 permissions={'actions':[],'notActions':[],'notDataActions':[],'dataActions':['Microsoft.Storage/storageAccounts/queueServices/queues/messages/write'],'condition':None,'conditionVersion':None}
 if os.environ.get('BROAD_DEFINITION'):permissions['actions']=['Microsoft.Storage/storageAccounts/queueServices/queues/delete']
 if os.environ.get('EXTRA_PERMISSION_BLOCK'):permissions=[permissions,{'actions':['*'],'notActions':[],'dataActions':[],'notDataActions':[]}]
 else:permissions=[permissions]
 print(json.dumps([{'name':'bf5dee66-37ae-4ef4-b34a-9f135e10d804','roleName':'HHC Recording Queue Message Write','roleType':'CustomRole','assignableScopes':['/subscriptions/sub/resourceGroups/alive'],'permissions':permissions}]))
elif args[:3]==['role','assignment','list']:
 if os.environ.get('FAIL_ROLE_QUERY'):sys.exit(1)
 scope=args[args.index('--scope')+1]
 roles=[('worker','c6a89b2d-59bc-44d0-9896-0f6e12d7b80a')] if scope.endswith('-poison') else [('api','c6a89b2d-59bc-44d0-9896-0f6e12d7b80a'),('worker','8a0f0c08-91a1-4084-bc3d-661d67233fed'),('worker','19e7f393-937e-4f77-808e-94535e297925'),('worker','bf5dee66-37ae-4ef4-b34a-9f135e10d804')]
 if os.environ.get('MISSING_ROLE'):roles=roles[:-1]
 if os.environ.get('WRONG_PRINCIPAL'):roles=[('api' if r=='bf5dee66-37ae-4ef4-b34a-9f135e10d804' else p,r) for p,r in roles]
 if os.environ.get('NORMALIZED_SCOPE'):scope=scope.replace('/resourceGroups/','/resourcegroups/')
 if os.environ.get('WRONG_SCOPE'):scope=scope+'-sibling'
 print(json.dumps([{'scope':scope,'principalId':p,'roleDefinitionId':'/roles/'+r} for p,r in roles]))
else:sys.exit(2)
''')
    fake.chmod(0o755)
    env={**os.environ,'PATH':temp+os.pathsep+os.environ['PATH'],'RESOURCE_GROUP':'alive','STORAGE_ACCOUNT_NAME':'storage'}
    for extra,expected in [({},0),({'NORMALIZED_SCOPE':'true'},0),({'NORMALIZED_SCOPE':'true','WRONG_SCOPE':'true'},1),({'MISSING_ROLE':'true'},1),({'WRONG_PRINCIPAL':'true'},1),({'FAIL_ROLE_QUERY':'true'},1),({'BROAD_DEFINITION':'true'},1),({'EXTRA_PERMISSION_BLOCK':'true'},1),({'FAIL_DEFINITION_QUERY':'true'},1)]:
        result=subprocess.run(['bash','scripts/verify-recording-event-permissions.sh'],env={**env,**extra},capture_output=True,text=True)
        assert (result.returncode==0)==(expected==0), (extra,result.stderr)
print('recording permissions: release isolation, 15 scoped grants, exact message-write definition and fail-closed preflight passed')
