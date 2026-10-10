import assert from 'node:assert/strict';
import { execFileSync } from 'node:child_process';
import { readFileSync } from 'node:fs';
import { test } from 'node:test';

test('Pages bundle rejects media reads without a scoped cookie', async () => {
  execFileSync('npm', ['run', 'build'], { cwd: new URL('..', import.meta.url), stdio: 'pipe' });
  const { default: media } = await import(new URL('../pages/test/dist/_worker.js', import.meta.url));
  const url = 'https://hhc-member-media-test.pages.dev/videos/rec-1/packages/0123456789abcdef0123456789abcdef/sessions/scope-1/master.m3u8';
  const env = { ALLOWED_ORIGINS: 'https://www-test.alive.org.tw' };
  for (const method of ['GET', 'HEAD']) {
    const response = await media.fetch(new Request(url, { method }), env);
    assert.equal(response.status, 401);
    assert.equal(response.headers.get('Content-Length'), null);
    assert.equal(response.headers.get('ETag'), null);
  }
  const range = await media.fetch(new Request(url, { headers: { Range: 'bytes=0-1' } }), env);
  assert.equal(range.status, 401);
  assert.equal(range.headers.get('Content-Range'), null);
});

test('test and production Pages deployments bind separate private media', () => {
  assert.deepEqual(
    readFileSync(new URL('../pages/test/dist/_worker.js', import.meta.url)),
    readFileSync(new URL('../pages/production/dist/_worker.js', import.meta.url)),
  );
  const expected = [
    ['test', 'hhc-member-media-test', 'hhc-member-recordings-test', 'hhc-media-test', 'https://www-test.alive.org.tw,https://admin-test.alive.org.tw'],
    ['production', 'hhc-member-media', 'hhc-member-recordings-prod', 'hhc-media-prod', 'https://www.alive.org.tw,https://admin.alive.org.tw'],
  ];
  for (const [environment, name, bucket, issuer, origin] of expected) {
    const config = JSON.parse(readFileSync(new URL(`../pages/${environment}/wrangler.jsonc`, import.meta.url), 'utf8'));
    assert.equal(config.name, name);
    assert.equal(config.pages_build_output_dir, './dist');
    assert.deepEqual(config.r2_buckets, [{ binding: 'MEDIA_BUCKET', bucket_name: bucket }]);
    assert.deepEqual(config.vars, { MEDIA_ISSUER: issuer, ALLOWED_ORIGINS: origin });
    assert.equal(config.routes, undefined);
    assert.equal(config.workers_dev, undefined);
  }
});

 test('deployed origins permit Admin preview preflight without granting media access', async () => {
  const {default: media} = await import('../pages/production/dist/_worker.js');
  const {vars} = JSON.parse(readFileSync(new URL('../pages/production/wrangler.jsonc', import.meta.url), 'utf8'));
  const url='https://media.alive.org.tw/videos/11111111-1111-4111-8111-111111111111/captures/aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa/previews/22222222-2222-4222-8222-222222222222/cookie';
  const headers={Origin:'https://admin.alive.org.tw','Access-Control-Request-Method':'POST','Access-Control-Request-Headers':'content-type'};
  const preflight=await media.fetch(new Request(url,{method:'OPTIONS',headers}),vars);
  assert.equal(preflight.status,204);
  assert.equal(preflight.headers.get('Access-Control-Allow-Origin'),headers.Origin);
  assert.equal(preflight.headers.get('Access-Control-Allow-Credentials'),'true');
  assert.equal((await media.fetch(new Request(url,{method:'POST',headers,body:JSON.stringify({credential:'invalid'})}),vars)).status,401);
  assert.equal((await media.fetch(new Request(url,{method:'OPTIONS',headers:{...headers,Origin:'https://attacker.example'}}),vars)).status,403);
 });
