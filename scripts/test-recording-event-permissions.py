from pathlib import Path
import json, subprocess

# Ordinary Contributor releases must not submit role-assignment writes.
normal=json.loads(subprocess.check_output(['az','bicep','build','--file','infra/recording-events.bicep','--stdout'],text=True))
assert not any(r['type']=='Microsoft.Authorization/roleAssignments' for r in normal['resources']), 'ordinary recording deployment still requires IAM write'
bootstrap=json.loads(subprocess.check_output(['az','bicep','build','--file','infra/recording-event-permissions.bicep','--stdout'],text=True))
roles=[r for r in bootstrap['resources'] if r['type']=='Microsoft.Authorization/roleAssignments']
assert len(roles)==4 and all(r['copy']['count']=="[length(variables('lanes'))]" for r in roles), 'bootstrap must manage exactly 12 scoped grants'
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
elif args[:3]==['storage','account','show']:print('/account')
elif args[:2]==['resource','show']:pass
elif args[:3]==['role','assignment','list']:
 if os.environ.get('FAIL_ROLE_QUERY'):sys.exit(1)
 scope=args[args.index('--scope')+1]
 roles=[('worker','c6a89b2d-59bc-44d0-9896-0f6e12d7b80a')] if scope.endswith('-poison') else [('api','c6a89b2d-59bc-44d0-9896-0f6e12d7b80a'),('worker','8a0f0c08-91a1-4084-bc3d-661d67233fed'),('worker','19e7f393-937e-4f77-808e-94535e297925')]
 if os.environ.get('MISSING_ROLE'):roles=roles[:-1]
 print(json.dumps([{'scope':scope,'principalId':p,'roleDefinitionId':'/roles/'+r} for p,r in roles]))
else:sys.exit(2)
''')
    fake.chmod(0o755)
    env={**os.environ,'PATH':temp+os.pathsep+os.environ['PATH'],'RESOURCE_GROUP':'alive','STORAGE_ACCOUNT_NAME':'storage'}
    for extra,expected in [({},0),({'MISSING_ROLE':'true'},1),({'FAIL_ROLE_QUERY':'true'},1)]:
        result=subprocess.run(['bash','scripts/verify-recording-event-permissions.sh'],env={**env,**extra},capture_output=True,text=True)
        assert (result.returncode==0)==(expected==0), (extra,result.stderr)
print('recording permissions: release isolation, 12 scoped grants, complete/missing/error preflight passed')
