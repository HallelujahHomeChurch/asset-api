#!/usr/bin/env bash
set -euo pipefail
node --input-type=module <<'JS'
import assert from 'node:assert/strict';
import {mkdtempSync, readFileSync, rmSync, writeFileSync} from 'node:fs';
import {tmpdir} from 'node:os';
import {join, resolve} from 'node:path';
import {spawnSync} from 'node:child_process';

const temp = mkdtempSync(join(tmpdir(), 'hhc-asset-role-'));
const script = resolve('scripts/ensure-workload-app-role.sh');
const ids = {tenant: '10000000-0000-4000-8000-000000000001', client: '10000000-0000-4000-8000-000000000002', principal: '10000000-0000-4000-8000-000000000003', apiClient: '10000000-0000-4000-8000-000000000004', resource: '10000000-0000-4000-8000-000000000005', role: '10000000-0000-4000-8000-000000000006', deployment: '10000000-0000-4000-8000-000000000007'};
writeFileSync(join(temp, 'az'), `#!/usr/bin/env node
const fs = require('node:fs');
const ids = ${JSON.stringify(ids)};
const args = process.argv.slice(2);
const scenario = process.env.ROLE_FIXTURE;
const log = process.env.ROLE_LOG;
const emit = value => console.log(JSON.stringify(value));
if (args[0] === 'identity') {
  if (scenario === 'missing-identity') process.exit(1);
  if (args[args.indexOf('--name') + 1] !== process.env.ROLE_EXPECTED_IDENTITY) process.exit(4);
  emit({clientId: ids.client, principalId: ids.principal, tenantId: scenario === 'wrong-tenant' ? ids.resource : ids.tenant});
} else if (args[0] === 'account') {
  emit({tenantId: ids.tenant, user: {type: scenario === 'human-apply' ? 'user' : 'servicePrincipal', name: ids.deployment}});
} else if (args[0] === 'ad') {
  const id = args[args.indexOf('--id') + 1];
  if (id === ids.client) emit({id: scenario === 'wrong-object' ? ids.resource : ids.principal, appId: ids.client, servicePrincipalType: 'ManagedIdentity'});
  else emit({id: ids.resource, appId: ids.apiClient, appOwnerOrganizationId: ids.tenant, servicePrincipalType: 'Application', servicePrincipalNames: [scenario === 'wrong-audience' ? 'other' : 'api://' + ids.apiClient], appRoles: [{id: ids.role, value: 'Asset.Invoke', isEnabled: scenario !== 'disabled-role', allowedMemberTypes: ['Application']}]});
} else if (args[0] === 'rest') {
  const method = args[args.indexOf('--method') + 1];
  const url = args[args.indexOf('--url') + 1];
  if (method === 'POST') {
    if (!url.endsWith('/' + ids.resource + '/appRoleAssignedTo')) process.exit(2);
    const body = JSON.parse(args[args.indexOf('--body') + 1]);
    if (body.principalId !== ids.principal || body.resourceId !== ids.resource || body.appRoleId !== ids.role) process.exit(3);
    if (scenario === 'denied-apply') process.exit(1);
    fs.appendFileSync(log, 'POST');
  } else {
    let value = [];
    const record = {principalId: ids.principal, resourceId: ids.resource, appRoleId: ids.role};
    if (scenario === 'existing' || fs.readFileSync(log, 'utf8').includes('POST')) value = [record];
    if (scenario === 'duplicate') value = [record, record];
    if (scenario === 'wrong-resource') value = [{...record, resourceId: ids.client}];
    emit({value, ...(scenario === 'pagination' ? {'@odata.nextLink': 'https://graph.microsoft.com/next'} : {})});
  }
} else process.exit(2);
`, {mode: 0o755});

const run = (scenario, mode = 'check', approved = false, subject = 'extractor', extra = {}) => {
  const log = join(temp, 'calls');
  writeFileSync(log, '');
  const result = spawnSync('bash', [script, mode], {encoding: 'utf8', env: {...process.env, PATH: `${temp}:${process.env.PATH}`, RESOURCE_GROUP: 'fixture', AZURE_CLIENT_ID: ids.deployment, AZURE_TENANT_ID: ids.tenant, ASSET_WORKLOAD_CLIENT_ID: ids.apiClient, ASSET_WORKLOAD_AUDIENCE: `api://${ids.apiClient}`, ASSET_EXTRACTOR_WORKLOAD_CLIENT_ID: ids.client, ASSET_EXTRACTOR_WORKLOAD_OBJECT_ID: ids.principal, EXTRACTOR_ROLE_APPROVED: String(approved), ROLE_FIXTURE: scenario, ROLE_LOG: log,
    ...(subject === 'watermark' ? {ROLE_SUBJECT: 'watermark', ASSET_EXTRACTOR_WORKLOAD_CLIENT_ID: '', ASSET_EXTRACTOR_WORKLOAD_OBJECT_ID: '', EXTRACTOR_ROLE_APPROVED: 'true', ASSET_WATERMARK_WORKLOAD_CLIENT_ID: ids.client, ASSET_WATERMARK_WORKLOAD_OBJECT_ID: ids.principal, WATERMARK_ROLE_APPROVED: String(approved), ROLE_EXPECTED_IDENTITY: 'hhc-web-bulletin-worker-identity'} : {ROLE_EXPECTED_IDENTITY: 'hhc-web-api-identity'}),
    ...extra}});
  assert.ok(!result.stdout.includes(ids.principal), 'do not log workload identity values');
  if (scenario === 'existing') assert.equal(result.status, 0, result.stderr);
  return {status: result.status, posts: readFileSync(log, 'utf8').split('POST').length - 1};
};
try {
  assert.deepEqual(run('existing'), {status: 0, posts: 0});
  assert.notEqual(run('missing').status, 0, 'missing assignment must fail read-only check');
  assert.equal(run('missing', 'apply').posts, 0, 'unapproved apply must never mutate');
  assert.notEqual(run('missing', 'apply').status, 0);
  assert.deepEqual(run('missing', 'apply', true), {status: 0, posts: 1});
  assert.deepEqual(run('existing', 'apply', true), {status: 0, posts: 0});
  for (const scenario of ['duplicate', 'wrong-resource', 'wrong-audience', 'wrong-object', 'wrong-tenant', 'missing-identity', 'disabled-role', 'pagination', 'human-apply', 'denied-apply']) {
    const result = run(scenario, 'apply', true);
    assert.notEqual(result.status, 0, scenario);
    assert.equal(result.posts, 0, scenario);
  }
  // Watermark subject: fixed identity name and its own approval input.
  assert.deepEqual(run('existing', 'check', false, 'watermark'), {status: 0, posts: 0});
  assert.notEqual(run('missing', 'check', false, 'watermark').status, 0);
  const crossApproval = run('missing', 'apply', false, 'watermark');
  assert.ok(crossApproval.status !== 0 && crossApproval.posts === 0, 'extractor approval must not authorize the watermark subject');
  assert.deepEqual(run('missing', 'apply', true, 'watermark'), {status: 0, posts: 1});
  for (const scenario of ['wrong-object', 'wrong-tenant', 'missing-identity', 'duplicate']) {
    const result = run(scenario, 'apply', true, 'watermark');
    assert.ok(result.status !== 0 && result.posts === 0, 'watermark ' + scenario);
  }
  const unknown = run('unknown-subject', 'check', false, 'extractor', {ROLE_SUBJECT: 'other'});
  assert.notEqual(unknown.status, 0, 'unknown subject must fail');
  console.log('Extractor and watermark app-role checks pass; only approved exact missing role can mutate.');
} finally { rmSync(temp, {recursive: true, force: true}); }
JS
