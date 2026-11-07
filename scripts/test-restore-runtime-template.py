import test_no_cloud
import copy
import importlib.util
import json
import os
from pathlib import Path
import subprocess
import unittest
from unittest.mock import patch

spec=importlib.util.spec_from_file_location('restore',Path(__file__).with_name('restore-runtime-template.py'))
restore=importlib.util.module_from_spec(spec);spec.loader.exec_module(restore)

class Restore(unittest.TestCase):
    def test_restores_entire_predecessor_without_candidate_manifest(self):
        resource,container=next(iter(restore.CONTAINERS.items()))
        image='alive.azurecr.io/alive/'+resource+'@sha256:'+'a'*64
        old={'containers':[{'name':container,'image':image,'env':[{'name':'POOL','value':'old'},{'name':'TOKEN','secretRef':resource+'-existing'}],'resources':{'cpu':0.5},'probes':[{'type':'Liveness'}]}],'scale':{'minReplicas':1},'revisionSuffix':'old'}
        current={'id':'/subscriptions/fixture/resourceGroups/alive/providers/Microsoft.App/containerApps/'+resource,'properties':{'configuration':{'secrets':[{'name':'existing'}],'dapr':{'enabled':False,'appId':resource},'ingress':{'external':False,'targetPort':10001,'traffic':[{'latestRevision':True,'weight':100}]}},'template':{'containers':[{'name':container,'image':'failed','env':[{'name':'POOL','value':'bad'}]}]}}}
        before=copy.deepcopy(old)
        for scope in [False,True] if resource=='hhc-web' else [False]:
            with patch.dict(os.environ,{'GITHUB_RUN_ID':'123','GITHUB_RUN_ATTEMPT':'1'}),patch.object(restore.subprocess,'run',side_effect=[subprocess.CompletedProcess([],0,json.dumps(current),''),subprocess.CompletedProcess([],0,json.dumps({'properties':{'template':old}}),''),subprocess.CompletedProcess([],0,'','')]) as command:
                restore.restore(resource,resource+'--healthy',image,False if scope else None,True if scope else None)
            args,kwargs=command.call_args
            payload=json.loads(kwargs['input'])['properties'];expected=copy.deepcopy(old)
            expected['containers'][0]['env'][1]['secretRef']='existing';expected['revisionSuffix']='rollback-123-1'
            self.assertEqual(payload['template'],expected)
            self.assertEqual(old,before)
            self.assertIn('@/dev/stdin',args[0]);self.assertNotIn('POOL',' '.join(args[0]))
            if scope:
                self.assertIsNone(payload['configuration']['ingress'])
                self.assertTrue(payload['configuration']['dapr']['enabled'])
            else:self.assertNotIn('configuration',payload)

    def test_azure_failure_is_redacted(self):
        with patch.object(restore.subprocess,'run',return_value=subprocess.CompletedProcess([],1,'','sensitive-marker')):
            with self.assertRaisesRegex(ValueError,'values withheld') as caught:restore.azure('anything')
            self.assertNotIn('sensitive-marker',str(caught.exception))

if __name__=='__main__':unittest.main()
